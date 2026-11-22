// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package clusterpki runs the cluster CA on top of the replicated state
// (ADR 0006, FA-PKI-01..04): it creates the CA at bootstrap, issues and
// redeems join tokens, issues and renews node certificates, keeps the
// revocation list at hand for TLS handshakes and manages this node's own
// certificate.
package clusterpki

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/pulsequorum/internal/cluster"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/store"
)

// Join token lifetimes (FA-PKI-02).
const (
	DefaultTokenTTL = time.Hour
	MaxTokenTTL     = 24 * time.Hour
)

const bootstrapCARequestID = "bootstrap:cluster-ca"

// ErrInvalidToken is the only error a joining node sees for a bad token,
// whatever the reason (unknown, used, expired, wrong secret or CA).
var ErrInvalidToken = errors.New("invalid join token")

// ErrNoCA means the cluster has no CA yet (bootstrap still running).
var ErrNoCA = errors.New("cluster CA does not exist yet")

// Applier proposes commands to the Raft cluster.
type Applier interface {
	Apply(ctx context.Context, cmd *pb.Command) (cluster.ApplyResult, error)
}

// Keyring encrypts the CA key (ADR 0011).
type Keyring interface {
	Encrypt(plaintext []byte, aad string) (string, error)
	Decrypt(ctx context.Context, ciphertext, aad string) ([]byte, error)
}

// Authority is the cluster CA. Issuing needs the CA key and therefore runs on
// the leader; reading the bundle works on every node.
type Authority struct {
	State   store.State
	Cluster Applier
	Keyring Keyring
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
}

func (a *Authority) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// caKeyAAD binds the encrypted CA key to its CA (ADR 0011).
func caKeyAAD(fingerprint string) string { return "cluster_ca/" + fingerprint + "/key" }

// Ensure creates the cluster CA if none exists. Only the leader creates it;
// followers receive it through Raft. Concurrent attempts are harmless: the
// FSM keeps the first CA and rejects any other.
func (a *Authority) Ensure(ctx context.Context, isLeader bool) error {
	if _, err := a.current(ctx); !errors.Is(err, ErrNoCA) || !isLeader {
		return ignoreNoCA(err)
	}
	clusterID, err := uuid.NewRandom()
	if err != nil {
		return fmt.Errorf("cluster ID: %w", err)
	}
	ca, err := pki.NewCA(clusterID.String(), a.now())
	if err != nil {
		return err
	}
	keyDER, err := ca.MarshalKey()
	if err != nil {
		return fmt.Errorf("marshal CA key: %w", err)
	}
	fp := pki.Fingerprint(ca.Cert)
	keyEnc, err := a.Keyring.Encrypt(keyDER, caKeyAAD(fp))
	clear(keyDER)
	if err != nil {
		return fmt.Errorf("encrypt CA key: %w", err)
	}
	cmd, err := cluster.NewPutClusterCA(bootstrapCARequestID, fp, cluster.CADocument{
		ClusterID: clusterID.String(), CertDER: ca.Cert.Raw, KeyEnc: keyEnc,
	})
	if err != nil {
		return err
	}
	res, err := a.Cluster.Apply(ctx, cmd)
	if err == nil {
		err = res.Err
	}
	var cmdErr *cluster.CommandError
	if errors.As(err, &cmdErr) && cmdErr.Code == cluster.CodeConflict {
		return nil // another leader created it first
	}
	if err != nil {
		return fmt.Errorf("store cluster CA: %w", err)
	}
	return nil
}

func ignoreNoCA(err error) error {
	if errors.Is(err, ErrNoCA) {
		return nil
	}
	return err
}

// current returns the CA row of the cluster.
func (a *Authority) current(ctx context.Context) (store.ClusterCA, error) {
	cas, err := a.State.ListClusterCAs(ctx)
	if err != nil {
		return store.ClusterCA{}, err
	}
	if len(cas) == 0 {
		return store.ClusterCA{}, ErrNoCA
	}
	return cas[0], nil
}

// Bundle returns the trusted CA certificates and the cluster ID.
func (a *Authority) Bundle(ctx context.Context) ([]*x509.Certificate, string, error) {
	cas, err := a.State.ListClusterCAs(ctx)
	if err != nil {
		return nil, "", err
	}
	if len(cas) == 0 {
		return nil, "", ErrNoCA
	}
	out := make([]*x509.Certificate, 0, len(cas))
	for _, c := range cas {
		cert, err := x509.ParseCertificate(c.CertDER)
		if err != nil {
			return nil, "", fmt.Errorf("parse cluster CA: %w", err)
		}
		out = append(out, cert)
	}
	return out, cas[0].ClusterID, nil
}

// signer loads the CA with its decrypted key. The key stays in memory only
// for the duration of the caller's operation.
func (a *Authority) signer(ctx context.Context) (*pki.CA, error) {
	row, err := a.current(ctx)
	if err != nil {
		return nil, err
	}
	keyDER, err := a.Keyring.Decrypt(ctx, row.KeyEnc, caKeyAAD(row.Fingerprint))
	if err != nil {
		return nil, fmt.Errorf("decrypt CA key: %w", err)
	}
	defer clear(keyDER)
	return pki.ParseCA(row.CertDER, keyDER)
}

// Issued is a node certificate with the trust bundle to send to the node.
type Issued struct {
	Identity pki.Identity
	Leaf     *x509.Certificate
	// ChainDER is leaf then CA, as nodes present it (pki.Leaf).
	ChainDER [][]byte
	// BundleDER are the trusted CA certificates.
	BundleDER [][]byte
}

func (a *Authority) issue(ctx context.Context, csrDER []byte, id pki.Identity) (Issued, error) {
	ca, err := a.signer(ctx)
	if err != nil {
		return Issued{}, err
	}
	leaf, err := ca.Issue(csrDER, id, a.now())
	if err != nil {
		return Issued{}, err
	}
	bundle, _, err := a.Bundle(ctx)
	if err != nil {
		return Issued{}, err
	}
	out := Issued{Identity: id, Leaf: leaf, ChainDER: [][]byte{leaf.Raw, ca.Cert.Raw}}
	for _, c := range bundle {
		out.BundleDER = append(out.BundleDER, c.Raw)
	}
	return out, nil
}

// IssueSelf issues a voter certificate for this node directly, without a
// token: the bootstrap node and the leader renewing its own certificate.
func (a *Authority) IssueSelf(ctx context.Context, nodeID string, csrDER []byte) (Issued, error) {
	_, clusterID, err := a.Bundle(ctx)
	if err != nil {
		return Issued{}, err
	}
	return a.issue(ctx, csrDER, pki.Identity{ClusterID: clusterID, NodeID: nodeID, Role: pki.RoleVoter})
}

// CreateJoinToken stores a new one-time token and returns it for the
// operator; the secret itself is never stored (FA-PKI-02). ttl 0 means
// DefaultTokenTTL.
func (a *Authority) CreateJoinToken(ctx context.Context, role pki.Role, tenantID string, ttl time.Duration) (string, time.Time, error) {
	return a.CreateJoinTokenFor(ctx, role, tenantID, ttl, nil)
}

// CreateJoinTokenFor stores a new one-time join token on behalf of actor.
// Tenant-bound probe tokens at command level 10+ require a KIND_USER actor (ADR 0035).
func (a *Authority) CreateJoinTokenFor(ctx context.Context, role pki.Role, tenantID string, ttl time.Duration, actor *pb.Actor) (string, time.Time, error) {
	if ttl == 0 {
		ttl = DefaultTokenTTL
	}
	switch {
	case !role.Valid():
		return "", time.Time{}, fmt.Errorf("invalid role %q", role)
	case ttl < 0 || ttl > MaxTokenTTL:
		return "", time.Time{}, fmt.Errorf("token lifetime must be between 0 and %s", MaxTokenTTL)
	case tenantID != "" && role != pki.RoleProbe:
		return "", time.Time{}, errors.New("only probe tokens can be bound to a tenant")
	}
	bundle, _, err := a.Bundle(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	tok, err := pki.NewJoinToken(pki.Fingerprint(bundle[0]))
	if err != nil {
		return "", time.Time{}, err
	}
	expires := a.now().Add(ttl)
	cmd, err := cluster.NewPutJoinToken(uuid.NewString(), tok.ID, cluster.JoinTokenDocument{
		SecretHash: pki.HashSecret(tok.Secret), Role: string(role), TenantID: tenantID, ExpiresMs: expires.UnixMilli(),
	})
	if err != nil {
		return "", time.Time{}, err
	}
	cmd.Actor = actor
	res, err := a.Cluster.Apply(ctx, cmd)
	if err == nil {
		err = res.Err
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("store join token: %w", err)
	}
	return tok.String(), expires, nil
}

// Redeem enrolls a node with a join token: it checks the token, uses it up
// through Raft and issues a certificate with the token's role and tenant.
// The CSR is checked before the token is used, so a broken request does not
// burn it. Every token problem yields ErrInvalidToken.
func (a *Authority) Redeem(ctx context.Context, token, nodeID string, csrDER []byte) (Issued, error) {
	tok, err := pki.ParseJoinToken(token)
	if err != nil {
		return Issued{}, ErrInvalidToken
	}
	bundle, clusterID, err := a.Bundle(ctx)
	if err != nil {
		return Issued{}, err
	}
	if !pinned(bundle, tok.Fingerprint) {
		return Issued{}, ErrInvalidToken
	}
	stored, err := a.State.GetJoinToken(ctx, tok.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return Issued{}, ErrInvalidToken
	case err != nil:
		return Issued{}, err
	}
	if stored.UsedMs != 0 || !pki.SecretMatches(tok.Secret, stored.SecretHash) {
		return Issued{}, ErrInvalidToken
	}
	// Validate the node ID and CSR before the token is spent.
	probeID := pki.Identity{ClusterID: clusterID, NodeID: nodeID, Role: pki.Role(stored.Role), TenantID: stored.TenantID}
	if err := checkCSR(csrDER); err != nil {
		return Issued{}, err
	}
	if _, err := uuid.Parse(nodeID); err != nil {
		return Issued{}, errors.New("node ID must be a UUID")
	}

	// A random request ID on purpose: a request ID derived from the token
	// would let anyone holding a used token replay it and receive the
	// deduplicated success. A join whose outcome is unknown needs a new token.
	res, err := a.Cluster.Apply(ctx, cluster.NewRedeemJoinToken(uuid.NewString(), tok.ID, pki.HashSecret(tok.Secret), nodeID))
	if err == nil {
		err = res.Err
	}
	var cmdErr *cluster.CommandError
	if errors.As(err, &cmdErr) {
		return Issued{}, ErrInvalidToken
	}
	if err != nil {
		return Issued{}, fmt.Errorf("redeem join token: %w", err)
	}
	var granted cluster.JoinTokenResult
	if err := json.Unmarshal(res.Result, &granted); err != nil {
		return Issued{}, fmt.Errorf("redeem join token: %w", err)
	}
	if res.Duplicate {
		return Issued{}, ErrInvalidToken
	}
	probeID.Role, probeID.TenantID = pki.Role(granted.Role), granted.TenantID
	return a.issue(ctx, csrDER, probeID)
}

// Renew issues a new certificate for an authenticated peer, keeping its
// identity (FA-PKI-03). The caller must have verified the peer's current
// certificate (mTLS) and passes the identity from it.
func (a *Authority) Renew(ctx context.Context, peer pki.Identity, csrDER []byte) (Issued, error) {
	return a.issue(ctx, csrDER, peer)
}

// Revoke adds a certificate to the revocation list (FA-PKI-04).
func (a *Authority) Revoke(ctx context.Context, cert *x509.Certificate) error {
	id, err := pki.IdentityOf(cert)
	if err != nil {
		return err
	}
	res, err := a.Cluster.Apply(ctx, cluster.NewRevokeCertificate(uuid.NewString(), pki.SerialString(cert), id.NodeID, cert.NotAfter.UnixMilli()))
	if err == nil {
		err = res.Err
	}
	if err != nil {
		return fmt.Errorf("revoke certificate: %w", err)
	}
	return nil
}

func pinned(bundle []*x509.Certificate, fingerprint string) bool {
	for _, c := range bundle {
		if pki.Fingerprint(c) == fingerprint {
			return true
		}
	}
	return false
}

func checkCSR(der []byte) error {
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		return fmt.Errorf("invalid certificate request: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return fmt.Errorf("invalid certificate request: %w", err)
	}
	return nil
}
