// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/secrets"
	"github.com/joinsider/uptimecluster/internal/store"
)

// Local authentication in the replicated state (ADR 0008). The FSM never sees
// a plaintext password, refresh token or private key: it stores Argon2id
// hashes, token hashes and encrypted keys only, and rejects anything else.

// UserDocument is the JSON document of a local user inside PutEntity.
type UserDocument struct {
	Username     string `json:"username"`
	DisplayName  string `json:"displayName,omitempty"`
	PasswordHash string `json:"passwordHash"`
	Role         string `json:"role"`
	Disabled     bool   `json:"disabled,omitempty"`
	// Setup creates the first user; it is rejected once any user exists.
	Setup bool `json:"setup,omitempty"`
}

// SigningKeyDocument is an access token signing key (FA-AUTH-02).
type SigningKeyDocument struct {
	Alg           string `json:"alg"`
	PublicKey     []byte `json:"publicKey"`
	PrivateKeyEnc string `json:"privateKeyEnc"`
	RetiredMs     int64  `json:"retiredMs,omitempty"`
}

// Session command results.
const (
	SessionRotated = "rotated"
	SessionReused  = "reused"
	SessionRevoked = "revoked"
	SessionExpired = "expired"
)

// SessionResult is the result of RotateSession. Only SessionRotated means the
// new token is valid.
type SessionResult struct {
	Status   string `json:"status"`
	UserID   string `json:"userId,omitempty"`
	TenantID string `json:"tenantId,omitempty"`
}

var validRoles = []string{"platform-admin", "tenant-admin", "tenant-editor", "tenant-viewer"}

// NewPutUser builds a command that creates (expectedRevision 0) or replaces a user.
func NewPutUser(requestID, tenantID, userID string, doc UserDocument, expectedRevision uint64) (*pb.Command, error) {
	return newPut(requestID, tenantID, pb.EntityKind_ENTITY_KIND_USER, userID, doc, expectedRevision)
}

// NewDeleteUser builds a command that deletes a user and its sessions.
func NewDeleteUser(requestID, tenantID, userID string) *pb.Command {
	return &pb.Command{
		Version: CommandVersion, RequestId: requestID, TenantId: tenantID,
		Payload: &pb.Command_DeleteEntity{DeleteEntity: &pb.DeleteEntity{Kind: pb.EntityKind_ENTITY_KIND_USER, Id: userID}},
	}
}

// NewPutSigningKey builds a platform-level command that stores or retires a signing key.
func NewPutSigningKey(requestID, kid string, doc SigningKeyDocument) (*pb.Command, error) {
	return newPut(requestID, "", pb.EntityKind_ENTITY_KIND_SIGNING_KEY, kid, doc, 0)
}

// NewCreateSession builds the command that starts a token family.
func NewCreateSession(requestID, tenantID, userID, tokenHash, familyID string, expiresMs int64) *pb.Command {
	return &pb.Command{
		Version: CommandVersion, RequestId: requestID, TenantId: tenantID,
		Actor: &pb.Actor{Kind: pb.Actor_KIND_USER, Id: userID},
		Payload: &pb.Command_CreateSession{CreateSession: &pb.CreateSession{
			UserId: userID, TokenHash: tokenHash, FamilyId: familyID, ExpiresMs: expiresMs,
		}},
	}
}

// NewRotateSession builds the refresh command. The request ID covers both
// hashes: retrying the same refresh is deduplicated, while presenting the old
// token again (with a new random successor) is applied and detected as reuse.
func NewRotateSession(oldHash, newHash string, expiresMs int64) *pb.Command {
	return &pb.Command{
		Version: CommandVersion, RequestId: "rotate:" + oldHash + ":" + newHash,
		Payload: &pb.Command_RotateSession{RotateSession: &pb.RotateSession{
			OldTokenHash: oldHash, NewTokenHash: newHash, ExpiresMs: expiresMs,
		}},
	}
}

// NewRevokeFamily builds the logout command.
func NewRevokeFamily(requestID, familyID string) *pb.Command {
	return &pb.Command{
		Version: CommandVersion, RequestId: requestID,
		Payload: &pb.Command_RevokeSessions{RevokeSessions: &pb.RevokeSessions{FamilyId: familyID}},
	}
}

// NewRevokeUserSessions builds the command that ends all sessions of a user.
func NewRevokeUserSessions(requestID, tenantID, userID string) *pb.Command {
	return &pb.Command{
		Version: CommandVersion, RequestId: requestID, TenantId: tenantID,
		Payload: &pb.Command_RevokeSessions{RevokeSessions: &pb.RevokeSessions{UserId: userID}},
	}
}

func applyPutUser(ctx context.Context, tx store.StateTx, cmd *pb.Command, put *pb.PutEntity) (json.RawMessage, error) {
	var doc UserDocument
	if err := json.Unmarshal(put.GetDocumentJson(), &doc); err != nil {
		return nil, invalid("user document: %v", err)
	}
	tenantID := cmd.GetTenantId()
	username := strings.TrimSpace(doc.Username)
	switch {
	case tenantID == "":
		return nil, invalid("user command without tenant")
	case username == "" || len(username) > 64:
		return nil, invalid("username must have 1 to 64 characters")
	case !strings.HasPrefix(doc.PasswordHash, "$argon2id$"):
		return nil, invalid("password must be an Argon2id hash")
	}
	if !containsString(validRoles, doc.Role) {
		return nil, invalid("role %q", doc.Role)
	}
	issuedAt := cmd.GetIssuedAtMs()
	if doc.Setup {
		n, err := tx.CountUsers(ctx)
		if err != nil {
			return nil, err
		}
		if n > 0 || put.GetExpectedRevision() != 0 {
			return nil, &CommandError{Code: CodeConflict, Message: "setup is already complete"}
		}
	}

	// A new password or disabling ends every session of the user (FA-AUTH-03).
	revoke := false
	if put.GetExpectedRevision() != 0 {
		cur, err := tx.GetUser(ctx, tenantID, put.GetId())
		if err != nil {
			return nil, storeError(err)
		}
		revoke = doc.Disabled || cur.PasswordHash != doc.PasswordHash
	}
	err := tx.PutUser(ctx, store.User{
		TenantID: tenantID, ID: put.GetId(), Username: username, DisplayName: doc.DisplayName,
		PasswordHash: doc.PasswordHash, Role: doc.Role, Disabled: doc.Disabled, CreatedMs: issuedAt, UpdatedMs: issuedAt,
	}, put.GetExpectedRevision())
	if err != nil {
		return nil, storeError(err)
	}
	if revoke {
		if err := tx.RevokeUserSessions(ctx, tenantID, put.GetId(), issuedAt); err != nil {
			return nil, err
		}
	}
	return json.Marshal(PutResult{ID: put.GetId(), Revision: put.GetExpectedRevision() + 1})
}

func applyPutSigningKey(ctx context.Context, tx store.StateTx, cmd *pb.Command, put *pb.PutEntity) (json.RawMessage, error) {
	var doc SigningKeyDocument
	if err := json.Unmarshal(put.GetDocumentJson(), &doc); err != nil {
		return nil, invalid("signing key document: %v", err)
	}
	switch {
	case put.GetId() == "":
		return nil, invalid("signing key without ID")
	case doc.Alg != "EdDSA" || len(doc.PublicKey) != 32:
		return nil, invalid("signing key must be an Ed25519 public key")
	case !secrets.IsEncrypted(doc.PrivateKeyEnc):
		return nil, invalid("signing key private part must be encrypted")
	}
	err := tx.PutSigningKey(ctx, store.SigningKey{
		KID: put.GetId(), Alg: doc.Alg, PublicKey: doc.PublicKey, PrivateKeyEnc: doc.PrivateKeyEnc,
		CreatedMs: cmd.GetIssuedAtMs(), RetiredMs: doc.RetiredMs,
	})
	if err != nil {
		return nil, storeError(err)
	}
	return json.Marshal(PutResult{ID: put.GetId()})
}

func applyCreateSession(ctx context.Context, tx store.StateTx, cmd *pb.Command, c *pb.CreateSession) (json.RawMessage, error) {
	now := cmd.GetIssuedAtMs()
	switch {
	case cmd.GetTenantId() == "" || c.GetUserId() == "" || c.GetFamilyId() == "":
		return nil, invalid("session without tenant, user or family")
	case len(c.GetTokenHash()) != 64:
		return nil, invalid("session token must be a SHA-256 hash")
	case c.GetExpiresMs() <= now:
		return nil, invalid("session already expired")
	}
	u, err := tx.GetUser(ctx, cmd.GetTenantId(), c.GetUserId())
	if err != nil {
		return nil, storeError(err)
	}
	if u.Disabled {
		return nil, invalid("user is disabled")
	}
	err = tx.InsertSession(ctx, store.Session{
		TokenHash: c.GetTokenHash(), FamilyID: c.GetFamilyId(), TenantID: u.TenantID, UserID: u.ID,
		CreatedMs: now, ExpiresMs: c.GetExpiresMs(),
	})
	if err != nil {
		return nil, storeError(err)
	}
	if err := tx.SetLastLogin(ctx, u.TenantID, u.ID, now); err != nil {
		return nil, err
	}
	if err := tx.PruneSessions(ctx, now); err != nil {
		return nil, err
	}
	return json.Marshal(SessionResult{Status: SessionRotated, UserID: u.ID, TenantID: u.TenantID})
}

// applyRotateSession implements refresh token rotation with reuse detection
// (FA-AUTH-03). Rejections that must persist (revoking a family) are results,
// not command errors, because command errors roll the transaction back.
func applyRotateSession(ctx context.Context, tx store.StateTx, cmd *pb.Command, r *pb.RotateSession) (json.RawMessage, error) {
	now := cmd.GetIssuedAtMs()
	if len(r.GetOldTokenHash()) != 64 || len(r.GetNewTokenHash()) != 64 || r.GetExpiresMs() <= now {
		return nil, invalid("invalid rotation")
	}
	old, err := tx.GetSession(ctx, r.GetOldTokenHash())
	if err != nil {
		return nil, storeError(err)
	}
	result := func(status string) (json.RawMessage, error) {
		return json.Marshal(SessionResult{Status: status, UserID: old.UserID, TenantID: old.TenantID})
	}
	switch {
	case old.RevokedMs != 0:
		return result(SessionRevoked)
	case old.UsedMs != 0:
		// A token was presented twice: someone else may hold it. End the family.
		if err := tx.RevokeSessionFamily(ctx, old.FamilyID, now); err != nil {
			return nil, err
		}
		return result(SessionReused)
	case old.ExpiresMs <= now:
		return result(SessionExpired)
	}
	u, err := tx.GetUser(ctx, old.TenantID, old.UserID)
	if err != nil {
		return nil, storeError(err)
	}
	if u.Disabled {
		if err := tx.RevokeSessionFamily(ctx, old.FamilyID, now); err != nil {
			return nil, err
		}
		return result(SessionRevoked)
	}
	if err := tx.MarkSessionUsed(ctx, old.TokenHash, now); err != nil {
		return nil, err
	}
	err = tx.InsertSession(ctx, store.Session{
		TokenHash: r.GetNewTokenHash(), FamilyID: old.FamilyID, TenantID: old.TenantID, UserID: old.UserID,
		CreatedMs: now, ExpiresMs: r.GetExpiresMs(),
	})
	if err != nil {
		return nil, storeError(err)
	}
	if err := tx.PruneSessions(ctx, now); err != nil {
		return nil, err
	}
	return result(SessionRotated)
}

func applyRevokeSessions(ctx context.Context, tx store.StateTx, cmd *pb.Command, r *pb.RevokeSessions) (json.RawMessage, error) {
	now := cmd.GetIssuedAtMs()
	switch {
	case r.GetFamilyId() != "" && r.GetUserId() == "":
		if err := tx.RevokeSessionFamily(ctx, r.GetFamilyId(), now); err != nil {
			return nil, err
		}
	case r.GetUserId() != "" && r.GetFamilyId() == "" && cmd.GetTenantId() != "":
		if err := tx.RevokeUserSessions(ctx, cmd.GetTenantId(), r.GetUserId(), now); err != nil {
			return nil, err
		}
	default:
		return nil, invalid("revoke needs either a family or a user of a tenant")
	}
	return json.Marshal(PutResult{ID: fmt.Sprint(r.GetFamilyId(), r.GetUserId())})
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
