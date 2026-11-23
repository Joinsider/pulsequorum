// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/secrets"
	"github.com/joinsider/pulsequorum/internal/store"
)

// testCA creates a real cluster CA and returns it with its encrypted key.
func testCA(t *testing.T, clusterID string) (*pki.CA, string) {
	t.Helper()
	ca, err := pki.NewCA(clusterID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := ca.MarshalKey()
	if err != nil {
		t.Fatal(err)
	}
	return ca, encryptedKey(t, keyDER)
}

// encryptedKey encrypts plaintext with a test data key, as the leader does (ADR 0011).
func encryptedKey(t *testing.T, plaintext []byte) string {
	t.Helper()
	ctx := t.Context()
	kek, err := secrets.LoadKEK("", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))), "")
	if err != nil {
		t.Fatal(err)
	}
	dek, err := secrets.NewDEK()
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := kek.Wrap("key-1", dek)
	if err != nil {
		t.Fatal(err)
	}
	kr := secrets.NewKeyring(kek, func(context.Context) ([]store.SecretKey, error) {
		return []store.SecretKey{{KeyID: "key-1", WrappedDEK: wrapped, KEKFingerprint: kek.Fingerprint}}, nil
	})
	if err := kr.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	enc, err := kr.Encrypt(plaintext, "cluster-ca")
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

// selfSigned returns a certificate that is not issued by the cluster CA.
func selfSigned(t *testing.T, isCA bool) *x509.Certificate {
	t.Helper()
	key, err := pki.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(7),
		Subject:               pkix.Name{CommonName: "not a CA"},
		NotBefore:             now,
		NotAfter:              now.Add(time.Hour),
		BasicConstraintsValid: true,
		IsCA:                  isCA,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// expectErr checks the outcome of a command: nil means success, otherwise
// the error must match want (ErrInvalidCommand, store.ErrConflict, ...).
func expectErr(t *testing.T, res ApplyResult, want error) {
	t.Helper()
	switch {
	case want == nil && res.Err != nil:
		t.Errorf("error = %v, want success", res.Err)
	case want != nil && !errors.Is(res.Err, want):
		t.Errorf("error = %v, want %v", res.Err, want)
	}
}

func caDoc(ca *pki.CA, keyEnc string) CADocument {
	return CADocument{ClusterID: ca.ClusterID(), CertDER: ca.Cert.Raw, KeyEnc: keyEnc}
}

// TestClusterCAValidation covers FA-PKI-01: the CA is stored only if its
// certificate is a CA matching the fingerprint, and its key is encrypted.
func TestClusterCAValidation(t *testing.T) {
	t.Parallel()
	ca, keyEnc := testCA(t, "cluster-1")
	other, _ := testCA(t, "cluster-1")
	fp := pki.Fingerprint(ca.Cert)
	keyDER, err := ca.MarshalKey()
	if err != nil {
		t.Fatal(err)
	}
	leaf := selfSigned(t, false)

	tests := []struct {
		name    string
		id      string
		doc     CADocument
		wantErr error
	}{
		{"valid CA", fp, caDoc(ca, keyEnc), nil},
		{"fingerprint of another CA", pki.Fingerprint(other.Cert), caDoc(ca, keyEnc), ErrInvalidCommand},
		{"malformed fingerprint", "short", caDoc(ca, keyEnc), ErrInvalidCommand},
		{"fingerprint with padding", fp + "=", caDoc(ca, keyEnc), ErrInvalidCommand},
		{"certificate is not a CA", pki.Fingerprint(leaf), CADocument{ClusterID: "c", CertDER: leaf.Raw, KeyEnc: keyEnc}, ErrInvalidCommand},
		{"garbage certificate", fp, CADocument{ClusterID: "c", CertDER: []byte("garbage"), KeyEnc: keyEnc}, ErrInvalidCommand},
		{"plaintext key", fp, CADocument{ClusterID: "c", CertDER: ca.Cert.Raw, KeyEnc: base64.StdEncoding.EncodeToString(keyDER)}, ErrInvalidCommand},
		{"missing key", fp, CADocument{ClusterID: "c", CertDER: ca.Cert.Raw}, ErrInvalidCommand},
		{"empty cluster ID", fp, CADocument{CertDER: ca.Cert.Raw, KeyEnc: keyEnc}, ErrInvalidCommand},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newReplica(t)
			cmd, err := NewPutClusterCA("ca-"+tt.name, tt.id, tt.doc)
			if err != nil {
				t.Fatal(err)
			}
			expectErr(t, r.apply(cmd, 3000), tt.wantErr)
		})
	}
}

// TestClusterCAIsSingleAndIdempotent covers the rule that a cluster has one CA
// until rotation (P5): the same CA again is a no-op, another one a conflict.
func TestClusterCAIsSingleAndIdempotent(t *testing.T) {
	t.Parallel()
	r := newReplica(t)
	ca, keyEnc := testCA(t, "cluster-1")
	other, otherKey := testCA(t, "cluster-1")
	put := func(id string, ca *pki.CA, keyEnc string) ApplyResult {
		cmd, err := NewPutClusterCA("ca-"+id, pki.Fingerprint(ca.Cert), caDoc(ca, keyEnc))
		if err != nil {
			t.Fatal(err)
		}
		return r.apply(cmd, 3000)
	}
	expectErr(t, put("first", ca, keyEnc), nil)
	expectErr(t, put("retry", ca, keyEnc), nil)
	res := put("second", other, otherKey)
	expectErr(t, res, store.ErrConflict)
	if res.Err != nil && !strings.Contains(res.Err.Error(), "cluster CA already exists") {
		t.Errorf("conflict message = %v", res.Err)
	}
	cas, err := r.st.ListClusterCAs(t.Context())
	if err != nil || len(cas) != 1 || cas[0].Fingerprint != pki.Fingerprint(ca.Cert) {
		t.Errorf("stored CAs = %+v, %v; want only the first CA", cas, err)
	}
}

// TestJoinTokenCreateRules covers FA-PKI-02 for creating a token: hash shape,
// role, tenant binding, and a lifetime of at most 24 hours.
func TestJoinTokenCreateRules(t *testing.T) {
	t.Parallel()
	const issuedAt = int64(10_000)
	hash := pki.HashSecret("create-secret")
	hour := time.Hour.Milliseconds()
	tests := []struct {
		name    string
		doc     JoinTokenDocument
		revise  uint64
		wantErr error
	}{
		{"voter without tenant", JoinTokenDocument{SecretHash: hash, Role: "voter", ExpiresMs: issuedAt + hour}, 0, nil},
		{"probe of a tenant", JoinTokenDocument{SecretHash: hash, Role: "probe", TenantID: DefaultTenantID, ExpiresMs: issuedAt + hour}, 0, nil},
		{"probe of an unknown tenant", JoinTokenDocument{SecretHash: hash, Role: "probe", TenantID: "private", ExpiresMs: issuedAt + hour}, 0, ErrInvalidCommand},
		{"voter bound to a tenant", JoinTokenDocument{SecretHash: hash, Role: "voter", TenantID: "private", ExpiresMs: issuedAt + hour}, 0, ErrInvalidCommand},
		{"unknown role", JoinTokenDocument{SecretHash: hash, Role: "admin", ExpiresMs: issuedAt + hour}, 0, ErrInvalidCommand},
		{"lifetime above 24 hours", JoinTokenDocument{SecretHash: hash, Role: "voter", ExpiresMs: issuedAt + 24*hour + 1}, 0, ErrInvalidCommand},
		{"lifetime of exactly 24 hours", JoinTokenDocument{SecretHash: hash, Role: "voter", ExpiresMs: issuedAt + 24*hour}, 0, nil},
		{"expires at issue time", JoinTokenDocument{SecretHash: hash, Role: "voter", ExpiresMs: issuedAt}, 0, ErrInvalidCommand},
		{"expired before issue time", JoinTokenDocument{SecretHash: hash, Role: "voter", ExpiresMs: issuedAt - 1}, 0, ErrInvalidCommand},
		{"upper-case hash", JoinTokenDocument{SecretHash: "A" + hash[1:], Role: "voter", ExpiresMs: issuedAt + hour}, 0, ErrInvalidCommand},
		{"short hash", JoinTokenDocument{SecretHash: "abcd", Role: "voter", ExpiresMs: issuedAt + hour}, 0, ErrInvalidCommand},
		{"plain secret instead of hash", JoinTokenDocument{SecretHash: "create-secret", Role: "voter", ExpiresMs: issuedAt + hour}, 0, ErrInvalidCommand},
		{"update instead of create", JoinTokenDocument{SecretHash: hash, Role: "voter", ExpiresMs: issuedAt + hour}, 1, store.ErrConflict},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newReplica(t)
			cmd, err := NewPutJoinToken(fmt.Sprint("create-", i), fmt.Sprint("tok-", i), tt.doc)
			if err != nil {
				t.Fatal(err)
			}
			cmd.GetPutEntity().ExpectedRevision = tt.revise
			expectErr(t, r.apply(cmd, issuedAt), tt.wantErr)
		})
	}
}

// TestJoinTokenDuplicateAndDelete covers the creation of an existing token
// and the deletion of tokens.
func TestJoinTokenDuplicateAndDelete(t *testing.T) {
	t.Parallel()
	r := newReplica(t)
	hash := pki.HashSecret("dup")
	create := func(req string) ApplyResult {
		cmd, err := NewPutJoinToken(req, "dup-tok", JoinTokenDocument{SecretHash: hash, Role: "voter", ExpiresMs: 20_000})
		if err != nil {
			t.Fatal(err)
		}
		return r.apply(cmd, 10_000)
	}
	expectErr(t, create("dup-1"), nil)
	expectErr(t, create("dup-2"), store.ErrConflict)

	expectErr(t, r.apply(NewDeleteJoinToken("del-1", "dup-tok"), 11_000), nil)
	expectErr(t, r.apply(NewDeleteJoinToken("del-2", "dup-tok"), 11_100), store.ErrNotFound)
	if _, err := r.st.GetJoinToken(t.Context(), "dup-tok"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleted token error = %v, want ErrNotFound", err)
	}
}

// TestJoinTokenPruning covers the pruning of tokens that expired more than
// 24 hours before the command time.
func TestJoinTokenPruning(t *testing.T) {
	t.Parallel()
	r := newReplica(t)
	hour := time.Hour.Milliseconds()
	const first = int64(10_000)
	put := func(req, id string, issuedAt int64) {
		cmd, err := NewPutJoinToken(req, id, JoinTokenDocument{SecretHash: pki.HashSecret(id), Role: "voter", ExpiresMs: issuedAt + hour})
		if err != nil {
			t.Fatal(err)
		}
		expectErr(t, r.apply(cmd, issuedAt), nil)
	}
	put("prune-a", "old", first)
	// The old token expired more than 24 hours before this command time.
	later := first + hour + 24*hour + 1
	put("prune-b", "new", later)

	if _, err := r.st.GetJoinToken(t.Context(), "old"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("old token error = %v, want it pruned", err)
	}
	if _, err := r.st.GetJoinToken(t.Context(), "new"); err != nil {
		t.Errorf("new token: %v", err)
	}
}

// TestRedeemJoinToken covers FA-PKI-02 for redemption: one use, the stored
// secret hash, the expiry, and one generic rejection for every failure.
func TestRedeemJoinToken(t *testing.T) {
	t.Parallel()
	r := newReplica(t)
	hour := time.Hour.Milliseconds()
	const created = int64(10_000)
	expires := created + hour
	create := func(req, id, role, tenantID, secret string) {
		cmd, err := NewPutJoinToken(req, id, JoinTokenDocument{SecretHash: pki.HashSecret(secret), Role: role, TenantID: tenantID, ExpiresMs: expires})
		if err != nil {
			t.Fatal(err)
		}
		expectErr(t, r.apply(cmd, created), nil)
	}
	create("mk-probe", "probe-tok", "probe", DefaultTenantID, "probe-secret")
	create("mk-voter", "voter-tok", "voter", "", "voter-secret")

	// Every failure must look the same to the caller.
	var failures []*CommandError
	rejected := func(t *testing.T, res ApplyResult) {
		t.Helper()
		var ce *CommandError
		if !errors.As(res.Err, &ce) || ce.Code != CodeInvalid || ce.Message != invalidJoinToken {
			t.Errorf("rejection = %v, want invalid %q", res.Err, invalidJoinToken)
			return
		}
		failures = append(failures, ce)
	}

	rejected(t, r.apply(NewRedeemJoinToken("rd-wrong", "voter-tok", pki.HashSecret("guess"), "node-x"), 20_000))
	rejected(t, r.apply(NewRedeemJoinToken("rd-unknown", "missing-tok", pki.HashSecret("voter-secret"), "node-x"), 20_000))
	rejected(t, r.apply(NewRedeemJoinToken("rd-expired", "voter-tok", pki.HashSecret("voter-secret"), "node-x"), expires))
	rejected(t, r.apply(NewRedeemJoinToken("rd-no-node", "voter-tok", pki.HashSecret("voter-secret"), ""), 20_000))
	rejected(t, r.apply(NewRedeemJoinToken("rd-bad-hash", "voter-tok", "ABC", "node-x"), 20_000))

	// A wrong attempt does not use the token up.
	res := r.apply(NewRedeemJoinToken("rd-voter", "voter-tok", pki.HashSecret("voter-secret"), "node-v"), 20_000)
	expectErr(t, res, nil)
	var got JoinTokenResult
	if err := json.Unmarshal(res.Result, &got); err != nil || got != (JoinTokenResult{Role: "voter", TenantID: ""}) {
		t.Errorf("voter result = %s, %v", res.Result, err)
	}
	// Redeeming twice fails, even with another request ID.
	rejected(t, r.apply(NewRedeemJoinToken("rd-again", "voter-tok", pki.HashSecret("voter-secret"), "node-w"), 20_100))

	res = r.apply(NewRedeemJoinToken("rd-probe", "probe-tok", pki.HashSecret("probe-secret"), "node-p"), 20_000)
	expectErr(t, res, nil)
	if err := json.Unmarshal(res.Result, &got); err != nil || got != (JoinTokenResult{Role: "probe", TenantID: DefaultTenantID}) {
		t.Errorf("probe result = %s, %v", res.Result, err)
	}
	rejected(t, r.apply(NewRedeemJoinToken("rd-probe-again", "probe-tok", pki.HashSecret("probe-secret"), "node-q"), 20_200))

	if len(failures) != 7 {
		t.Fatalf("checked %d rejections, want 7", len(failures))
	}
	stored, err := r.st.GetJoinToken(t.Context(), "voter-tok")
	if err != nil || stored.UsedMs != 20_000 || stored.UsedBy != "node-v" {
		t.Errorf("voter token = %+v, %v; want used at 20000 by node-v", stored, err)
	}
}

// TestRedeemRejectsDeletedAndMalformedTokens covers redemption of a token that
// was deleted, and of a token whose stored hash does not match.
func TestRedeemRejectsDeletedAndMalformedTokens(t *testing.T) {
	t.Parallel()
	r := newReplica(t)
	cmd, err := NewPutJoinToken("del-mk", "gone", JoinTokenDocument{SecretHash: pki.HashSecret("gone-secret"), Role: "voter", ExpiresMs: 90_000})
	if err != nil {
		t.Fatal(err)
	}
	expectErr(t, r.apply(cmd, 10_000), nil)
	expectErr(t, r.apply(NewDeleteJoinToken("del-gone", "gone"), 11_000), nil)
	res := r.apply(NewRedeemJoinToken("rd-gone", "gone", pki.HashSecret("gone-secret"), "node-g"), 12_000)
	expectErr(t, res, ErrInvalidCommand)
	if res.Err == nil || !strings.HasSuffix(res.Err.Error(), invalidJoinToken) {
		t.Errorf("deleted token error = %v, want %q", res.Err, invalidJoinToken)
	}
}

// TestRevokeCertificate covers FA-PKI-04: strict serial and node rules,
// idempotent revocation and pruning of expired certificates.
func TestRevokeCertificate(t *testing.T) {
	t.Parallel()
	r := newReplica(t)
	tests := []struct {
		name     string
		serial   string
		node     string
		notAfter int64
		wantErr  error
	}{
		{"valid serial", "1a2b", "node-1", 50_000, nil},
		{"upper-case serial", "1A2B", "node-1", 50_000, ErrInvalidCommand},
		{"serial longer than 40", strings.Repeat("a", 41), "node-1", 50_000, ErrInvalidCommand},
		{"empty serial", "", "node-1", 50_000, ErrInvalidCommand},
		{"non-hex serial", "xyz", "node-1", 50_000, ErrInvalidCommand},
		{"no node", "3c4d", "", 50_000, ErrInvalidCommand},
		{"no expiry", "3c4d", "node-2", 0, ErrInvalidCommand},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expectErr(t, r.apply(NewRevokeCertificate(fmt.Sprint("rv-", i), tt.serial, tt.node, tt.notAfter), 10_000), tt.wantErr)
		})
	}
	// Revoking again changes nothing and is not an error.
	expectErr(t, r.apply(NewRevokeCertificate("rv-again", "1a2b", "node-9", 99_000), 10_500), nil)

	// Entries of certificates that expired before the command time are pruned.
	expectErr(t, r.apply(NewRevokeCertificate("rv-old", "5e6f", "node-3", 15_000), 10_600), nil)
	expectErr(t, r.apply(NewRevokeCertificate("rv-new", "7a8b", "node-4", 90_000), 20_000), nil)
	list, err := r.st.ListRevokedCerts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	serials := map[string]store.RevokedCert{}
	for _, c := range list {
		serials[c.Serial] = c
	}
	if _, ok := serials["5e6f"]; ok {
		t.Error("expired revocation 5e6f was not pruned")
	}
	if c := serials["1a2b"]; c.NodeID != "node-1" || c.NotAfterMs != 50_000 {
		t.Errorf("1a2b = %+v, want the first revocation", c)
	}
	if _, ok := serials["7a8b"]; !ok {
		t.Error("current revocation 7a8b missing")
	}
}

// TestRevokeNode covers FA-PKI-04: a node revocation is stored with the command
// time, node IDs are validated, and a repeated revocation keeps the later time.
func TestRevokeNode(t *testing.T) {
	t.Parallel()
	r := newReplica(t)
	tests := []struct {
		name    string
		node    string
		wantErr error
	}{
		{"valid ID", "node-1_a", nil},
		{"maximum length", strings.Repeat("a", 128), nil},
		{"too long", strings.Repeat("a", 129), ErrInvalidCommand},
		{"empty ID", "", ErrInvalidCommand},
		{"space", "node 1", ErrInvalidCommand},
		{"dot", "node.1", ErrInvalidCommand},
		{"slash", "node/1", ErrInvalidCommand},
		{"non-ASCII letter", "knoten-\u00e4", ErrInvalidCommand},
		{"newline", "node-1\n", ErrInvalidCommand},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expectErr(t, r.apply(NewRevokeNode(fmt.Sprint("rn-", i), tt.node), 10_000), tt.wantErr)
		})
	}

	// The result carries the revocation time.
	res := r.apply(NewRevokeNode("rn-result", "node-2"), 11_000)
	expectErr(t, res, nil)
	if want := `{"revokedMs":11000}`; string(res.Result) != want {
		t.Errorf("result = %s, want %s", res.Result, want)
	}

	// A repeated revocation with a later time replaces the earlier one.
	expectErr(t, r.apply(NewRevokeNode("rn-later", "node-2"), 12_000), nil)
	// An earlier time never moves the revocation back.
	expectErr(t, r.apply(NewRevokeNode("rn-earlier", "node-2"), 11_500), nil)

	list, err := r.st.ListRevokedNodes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, n := range list {
		got[n.NodeID] = n.RevokedMs
	}
	if got["node-1_a"] != 10_000 {
		t.Errorf("node-1_a revoked at %d, want 10000", got["node-1_a"])
	}
	if got["node-2"] != 12_000 {
		t.Errorf("node-2 revoked at %d, want the later 12000", got["node-2"])
	}
	if _, ok := got["node 1"]; ok {
		t.Error("invalid node ID stored")
	}
}

// TestPKICommandsAreDeterministic covers the determinism rule of the FSM: two
// replicas applying the same log reach the same state and results.
func TestPKICommandsAreDeterministic(t *testing.T) {
	t.Parallel()
	ca, keyEnc := testCA(t, "cluster-det")
	fp := pki.Fingerprint(ca.Cert)
	secret := "det-secret"
	var prints []string
	for range 2 {
		r := newReplica(t)
		steps := []struct {
			cmd *pb.Command
			at  int64
		}{
			{must(NewPutClusterCA("pk-ca", fp, caDoc(ca, keyEnc))), 4000},
			{must(NewPutClusterCA("pk-ca-2", fp, caDoc(ca, keyEnc))), 4100},
			{must(NewPutJoinToken("pk-tok", "det-tok", JoinTokenDocument{SecretHash: pki.HashSecret(secret), Role: "probe", TenantID: DefaultTenantID, ExpiresMs: 5000 + 3_600_000})), 5000},
			{NewRedeemJoinToken("pk-wrong", "det-tok", pki.HashSecret("nope"), "n1"), 5100},
			{NewRedeemJoinToken("pk-redeem", "det-tok", pki.HashSecret(secret), "n1"), 5200},
			{NewRedeemJoinToken("pk-again", "det-tok", pki.HashSecret(secret), "n2"), 5300},
			{NewRevokeCertificate("pk-rev", "abc123", "n1", 9000), 5400},
			{NewDeleteJoinToken("pk-del", "det-tok"), 5500},
			{NewRevokeNode("pk-node", "n1"), 5600},
			{NewRevokeNode("pk-node-again", "n1"), 5700},
			{NewRevokeNode("pk-node-bad", "n 1"), 5800},
		}
		var outcome strings.Builder
		for _, s := range steps {
			res := r.apply(s.cmd, s.at)
			fmt.Fprintf(&outcome, "%v|%s\n", res.Err, res.Result)
		}
		fp, err := r.st.Fingerprint(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		prints = append(prints, fp+"\n"+outcome.String())
	}
	if prints[0] != prints[1] {
		t.Fatalf("replicas diverged:\n%s\nvs\n%s", prints[0], prints[1])
	}
}
