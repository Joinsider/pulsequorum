// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import (
	"context"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/secrets"
	"github.com/joinsider/pulsequorum/internal/store"
)

// Cluster PKI in the replicated state (ADR 0006). The FSM stores the CA only
// as ciphertext, join tokens only as hashes of their secrets, and revoked
// serials until their certificates expire.

// joinTokenMaxTTL bounds the lifetime of a join token, measured in command time.
const joinTokenMaxTTL = 24 * time.Hour

// invalidJoinToken is the only message of a failed redemption, so a caller
// cannot tell an unknown, used, expired or wrong token apart.
const invalidJoinToken = "invalid join token"

// CADocument is the JSON document of the cluster CA inside PutEntity.
// CertDER is the DER certificate, KeyEnc the encrypted PKCS#8 key (ADR 0011).
type CADocument struct {
	ClusterID string `json:"clusterId"`
	CertDER   []byte `json:"certDer"`
	KeyEnc    string `json:"keyEnc"`
}

// JoinTokenDocument is the JSON document of a join token inside PutEntity.
// SecretHash is pki.HashSecret of the secret; the secret itself never arrives.
type JoinTokenDocument struct {
	SecretHash string `json:"secretHash"`
	Role       string `json:"role"`
	TenantID   string `json:"tenantId,omitempty"`
	ExpiresMs  int64  `json:"expiresMs"`
}

// JoinTokenResult is the result of RedeemJoinToken: the role and tenant the
// joining node is granted.
type JoinTokenResult struct {
	Role     string `json:"role"`
	TenantID string `json:"tenantId"`
}

// NewPutClusterCA builds a platform-level command that stores the cluster CA.
// The ID is the CA fingerprint (pki.Fingerprint).
func NewPutClusterCA(requestID, fingerprint string, doc CADocument) (*pb.Command, error) {
	return newPut(requestID, "", pb.EntityKind_ENTITY_KIND_CLUSTER_CA, fingerprint, doc, 0)
}

// NewPutJoinToken builds a platform-level command that creates a join token.
func NewPutJoinToken(requestID, tokenID string, doc JoinTokenDocument) (*pb.Command, error) {
	return newPut(requestID, "", pb.EntityKind_ENTITY_KIND_JOIN_TOKEN, tokenID, doc, 0)
}

// NewDeleteJoinToken builds a platform-level command that deletes a join token.
func NewDeleteJoinToken(requestID, tokenID string) *pb.Command {
	return stamp(&pb.Command{
		RequestId: requestID,
		Payload:   &pb.Command_DeleteEntity{DeleteEntity: &pb.DeleteEntity{Kind: pb.EntityKind_ENTITY_KIND_JOIN_TOKEN, Id: tokenID}},
	})
}

// NewRedeemJoinToken builds the command that uses up a join token for a node.
func NewRedeemJoinToken(requestID, tokenID, secretHash, nodeID string) *pb.Command {
	return stamp(&pb.Command{
		RequestId: requestID,
		Payload: &pb.Command_RedeemJoinToken{RedeemJoinToken: &pb.RedeemJoinToken{
			TokenId: tokenID, SecretHash: secretHash, NodeId: nodeID,
		}},
	})
}

// NewRevokeCertificate builds the command that adds a node certificate to the
// revocation list. The serial is pki.SerialString of the certificate.
func NewRevokeCertificate(requestID, serial, nodeID string, notAfterMs int64) *pb.Command {
	return stamp(&pb.Command{
		RequestId: requestID,
		Payload: &pb.Command_RevokeCertificate{RevokeCertificate: &pb.RevokeCertificate{
			Serial: serial, NodeId: nodeID, NotAfterMs: notAfterMs,
		}},
	})
}

// NewRevokeNode builds the platform-level command that revokes all certificates
// of a node issued so far (FA-PKI-04). The command time is the revocation time.
func NewRevokeNode(requestID, nodeID string) *pb.Command {
	return stamp(&pb.Command{
		RequestId: requestID,
		Payload:   &pb.Command_RevokeNode{RevokeNode: &pb.RevokeNode{NodeId: nodeID}},
	})
}

// RevokeNodeResult is the result of a node revocation: the revocation time.
type RevokeNodeResult struct {
	RevokedMs int64 `json:"revokedMs"`
}

func applyPutClusterCA(ctx context.Context, tx store.StateTx, cmd *pb.Command, put *pb.PutEntity) (json.RawMessage, error) {
	var doc CADocument
	if err := json.Unmarshal(put.GetDocumentJson(), &doc); err != nil {
		return nil, invalid("cluster CA document: %v", err)
	}
	fingerprint := put.GetId()
	if !isFingerprint(fingerprint) {
		return nil, invalid("cluster CA ID must be a base64url SHA-256 fingerprint")
	}
	if doc.ClusterID == "" {
		return nil, invalid("cluster CA without cluster ID")
	}
	cert, err := x509.ParseCertificate(doc.CertDER)
	switch {
	case err != nil:
		return nil, invalid("cluster CA certificate: %v", err)
	case !cert.IsCA:
		return nil, invalid("cluster CA certificate is not a CA")
	case pki.Fingerprint(cert) != fingerprint:
		return nil, invalid("cluster CA certificate does not match its fingerprint")
	case !secrets.IsEncrypted(doc.KeyEnc):
		return nil, invalid("cluster CA key must be encrypted")
	}

	cas, err := tx.ListClusterCAs(ctx)
	if err != nil {
		return nil, err
	}
	for _, ca := range cas {
		if ca.Fingerprint == fingerprint {
			// Repeated write of the same CA: nothing changes.
			return json.Marshal(PutResult{ID: fingerprint})
		}
	}
	if len(cas) > 0 {
		// Replacing the CA is rotation, which comes with P5.
		return nil, &CommandError{Code: CodeConflict, Message: "cluster CA already exists"}
	}
	err = tx.PutClusterCA(ctx, store.ClusterCA{
		Fingerprint: fingerprint, ClusterID: doc.ClusterID, CertDER: doc.CertDER,
		KeyEnc: doc.KeyEnc, CreatedMs: cmd.GetIssuedAtMs(),
	})
	if err != nil {
		return nil, storeError(err)
	}
	return json.Marshal(PutResult{ID: fingerprint})
}

func applyPutJoinToken(ctx context.Context, tx store.StateTx, cmd *pb.Command, put *pb.PutEntity) (json.RawMessage, error) {
	var doc JoinTokenDocument
	if err := json.Unmarshal(put.GetDocumentJson(), &doc); err != nil {
		return nil, invalid("join token document: %v", err)
	}
	id := put.GetId()
	issuedAt := cmd.GetIssuedAtMs()
	role := pki.Role(doc.Role)
	switch {
	case put.GetExpectedRevision() != 0:
		return nil, &CommandError{Code: CodeConflict, Message: "join tokens can only be created"}
	case !isLowerHex(doc.SecretHash) || len(doc.SecretHash) != 64:
		return nil, invalid("join token secret hash must be a SHA-256 hex digest")
	case !role.Valid():
		return nil, invalid("join token role %q", doc.Role)
	case doc.TenantID != "" && role != pki.RoleProbe:
		return nil, invalid("only probe tokens can be bound to a tenant")
	case doc.ExpiresMs <= issuedAt:
		return nil, invalid("join token already expired")
	case doc.ExpiresMs > issuedAt+joinTokenMaxTTL.Milliseconds():
		return nil, invalid("join token lifetime exceeds %s", joinTokenMaxTTL)
	}

	// A private probe must belong to an existing tenant; otherwise the token
	// could enroll a probe for a tenant created later with that ID.
	if doc.TenantID != "" {
		if _, err := tx.GetTenant(ctx, doc.TenantID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, invalid("join token for unknown tenant %q", doc.TenantID)
			}
			return nil, err
		}
	}

	_, err := tx.GetJoinToken(ctx, id)
	switch {
	case err == nil:
		return nil, &CommandError{Code: CodeConflict, Message: "join token already exists"}
	case !errors.Is(err, store.ErrNotFound):
		return nil, err
	}
	err = tx.InsertJoinToken(ctx, store.JoinToken{
		ID: id, SecretHash: doc.SecretHash, Role: doc.Role, TenantID: doc.TenantID,
		ExpiresMs: doc.ExpiresMs, CreatedMs: issuedAt,
	})
	if err != nil {
		return nil, storeError(err)
	}
	if err := tx.PruneJoinTokens(ctx, issuedAt-joinTokenMaxTTL.Milliseconds()); err != nil {
		return nil, err
	}
	return json.Marshal(PutResult{ID: id})
}

func applyDeleteJoinToken(ctx context.Context, tx store.StateTx, id string) (json.RawMessage, error) {
	if err := tx.DeleteJoinToken(ctx, id); err != nil {
		return nil, storeError(err)
	}
	return json.Marshal(PutResult{ID: id})
}

// applyRedeemJoinToken uses up a join token once (FA-PKI-02). Every failure is
// rejected with invalidJoinToken, whatever the cause.
func applyRedeemJoinToken(ctx context.Context, tx store.StateTx, cmd *pb.Command, r *pb.RedeemJoinToken) (json.RawMessage, error) {
	now := cmd.GetIssuedAtMs()
	reject := func() error { return &CommandError{Code: CodeInvalid, Message: invalidJoinToken} }
	if r.GetTokenId() == "" || r.GetNodeId() == "" || len(r.GetSecretHash()) != 64 || !isLowerHex(r.GetSecretHash()) {
		return nil, reject()
	}
	jt, err := tx.GetJoinToken(ctx, r.GetTokenId())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, reject()
	case err != nil:
		return nil, err
	}
	if jt.UsedMs != 0 || now >= jt.ExpiresMs ||
		subtle.ConstantTimeCompare([]byte(jt.SecretHash), []byte(r.GetSecretHash())) != 1 {
		return nil, reject()
	}
	err = tx.MarkJoinTokenUsed(ctx, jt.ID, now, r.GetNodeId())
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrConflict):
		return nil, reject()
	case err != nil:
		return nil, err
	}
	return json.Marshal(JoinTokenResult{Role: jt.Role, TenantID: jt.TenantID})
}

// applyRevokeCertificate adds a serial to the revocation list (FA-PKI-04).
// Entries of certificates that already expired are pruned.
func applyRevokeCertificate(ctx context.Context, tx store.StateTx, cmd *pb.Command, r *pb.RevokeCertificate) (json.RawMessage, error) {
	now := cmd.GetIssuedAtMs()
	switch {
	case r.GetSerial() == "" || len(r.GetSerial()) > 40 || !isLowerHex(r.GetSerial()):
		return nil, invalid("certificate serial must be lower-case hex of at most 40 characters")
	case r.GetNodeId() == "":
		return nil, invalid("revoked certificate without node ID")
	case r.GetNotAfterMs() <= 0:
		return nil, invalid("revoked certificate without expiry")
	}
	err := tx.PutRevokedCert(ctx, store.RevokedCert{
		Serial: r.GetSerial(), NodeID: r.GetNodeId(), RevokedMs: now, NotAfterMs: r.GetNotAfterMs(),
	})
	if err != nil {
		return nil, storeError(err)
	}
	if err := tx.PruneRevokedCerts(ctx, now); err != nil {
		return nil, err
	}
	return json.Marshal(PutResult{ID: r.GetSerial()})
}

// applyRevokeNode revokes a node: its certificates issued before the command
// time are rejected from now on (FA-PKI-04). A repeated revocation keeps the
// later time in the store. The registry entry of the node is removed (ADR 0017).
func applyRevokeNode(ctx context.Context, tx store.StateTx, cmd *pb.Command, r *pb.RevokeNode) (json.RawMessage, error) {
	id := r.GetNodeId()
	if !isNodeID(id) {
		return nil, invalid("node ID must be 1 to 128 characters of [A-Za-z0-9_-]")
	}
	now := cmd.GetIssuedAtMs()
	if err := tx.PutRevokedNode(ctx, store.RevokedNode{NodeID: id, RevokedMs: now}); err != nil {
		return nil, storeError(err)
	}
	// The registry entry goes with the node (ADR 0017).
	if err := tx.DeleteNode(ctx, id); err != nil {
		return nil, storeError(err)
	}
	return json.Marshal(RevokeNodeResult{RevokedMs: now})
}

// isNodeID reports whether s is a valid node ID: 1 to 128 characters of ASCII
// letters, digits, '_' and '-'.
func isNodeID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// isFingerprint reports whether s is a base64url SHA-256 without padding (43 characters).
func isFingerprint(s string) bool {
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	return err == nil && len(s) == 43 && len(b) == 32
}

// isLowerHex reports whether s is non-empty and consists of lower-case hex digits only.
func isLowerHex(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
