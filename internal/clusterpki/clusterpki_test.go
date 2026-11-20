// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package clusterpki

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/secrets"
	"github.com/joinsider/pulsequorum/internal/store/sqlite"
)

type env struct {
	ctx   context.Context
	dir   string
	node  *cluster.Node
	state *sqlite.StateStore
	auth  *Authority
	clock time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	dir := t.TempDir()
	log := slog.New(slog.DiscardHandler)
	state, err := sqlite.OpenState(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	node, err := cluster.Start(ctx, cluster.Config{NodeID: uuid.NewString(), Advertise: "n:9443", DataDir: dir,
		Timeout: 200 * time.Millisecond, Logger: log, RaftLogLevel: "error"}, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = node.Shutdown()
		_ = state.Close()
	})
	if err := cluster.EnsureDefaultTenant(ctx, node, state); err != nil {
		t.Fatal(err)
	}
	kek, err := secrets.LoadKEK("", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.EnsureSecretKey(ctx, node, state, kek); err != nil {
		t.Fatal(err)
	}
	kr := secrets.NewKeyring(kek, state.ListSecretKeys)
	if err := kr.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	e := &env{ctx: ctx, dir: dir, node: node, state: state, clock: time.Now()}
	e.auth = &Authority{State: state, Cluster: node, Keyring: kr, Now: func() time.Time { return e.clock }}
	if err := e.auth.Ensure(ctx, true); err != nil {
		t.Fatal(err)
	}
	return e
}

func csr(t *testing.T) []byte {
	t.Helper()
	key, err := pki.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	der, err := pki.NewCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func (e *env) verifier(t *testing.T, rev *Revocations) *pki.Verifier {
	t.Helper()
	bundle, clusterID, err := e.auth.Bundle(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	v := &pki.Verifier{ClusterID: clusterID, Roots: func() []*x509.Certificate { return bundle }}
	if rev != nil {
		v.Revoked = rev.Revoked
	}
	return v
}

// FA-PKI-01: the CA exists once, its key only encrypted in the state.
func TestEnsureCreatesOneEncryptedCA(t *testing.T) {
	e := newEnv(t)
	if err := e.auth.Ensure(e.ctx, true); err != nil {
		t.Fatal(err)
	}
	cas, err := e.state.ListClusterCAs(e.ctx)
	if err != nil || len(cas) != 1 {
		t.Fatalf("CAs = %d, %v", len(cas), err)
	}
	if !secrets.IsEncrypted(cas[0].KeyEnc) {
		t.Fatal("CA key is not encrypted")
	}
	ca, err := e.auth.signer(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := ca.MarshalKey()
	if strings.Contains(cas[0].KeyEnc, base64.StdEncoding.EncodeToString(keyDER)) {
		t.Fatal("CA key readable in the state")
	}
	if ca.ClusterID() != cas[0].ClusterID || pki.Fingerprint(ca.Cert) != cas[0].Fingerprint {
		t.Error("stored CA does not match")
	}

	// A follower never creates a CA.
	empty := &Authority{State: e.state, Cluster: e.node, Keyring: e.auth.Keyring}
	if err := empty.Ensure(e.ctx, false); err != nil {
		t.Fatal(err)
	}
	if cas, _ := e.state.ListClusterCAs(e.ctx); len(cas) != 1 {
		t.Errorf("CAs after follower Ensure = %d", len(cas))
	}
}

// FA-PKI-02: one-time, time-limited, role-bound tokens pinned to the CA.
func TestJoinTokenRedeem(t *testing.T) {
	e := newEnv(t)
	token, expires, err := e.auth.CreateJoinToken(e.ctx, pki.RoleProbe, cluster.DefaultTenantID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := expires.Sub(e.clock); got != DefaultTokenTTL {
		t.Errorf("TTL = %v", got)
	}
	tok, _ := pki.ParseJoinToken(token)
	stored, err := e.state.GetJoinToken(e.ctx, tok.ID)
	if err != nil || strings.Contains(stored.SecretHash, tok.Secret) || stored.SecretHash == tok.Secret {
		t.Fatalf("stored token = %+v, %v", stored, err)
	}

	nodeID := uuid.NewString()
	// Failures before the token is spent.
	wrongSecret := strings.Replace(token, tok.Secret, strings.Repeat("A", len(tok.Secret)), 1)
	otherCA, _ := pki.NewCA("other", time.Now())
	wrongCA := strings.Replace(token, tok.Fingerprint, pki.Fingerprint(otherCA.Cert), 1)
	for name, bad := range map[string]string{"wrong secret": wrongSecret, "wrong CA": wrongCA, "garbage": "uc1.x.y.z"} {
		if _, err := e.auth.Redeem(e.ctx, bad, nodeID, csr(t)); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := e.auth.Redeem(e.ctx, token, nodeID, []byte("not a CSR")); err == nil || errors.Is(err, ErrInvalidToken) {
		t.Errorf("bad CSR: err = %v", err)
	}
	if _, err := e.auth.Redeem(e.ctx, token, "../etc", csr(t)); err == nil {
		t.Error("bad node ID accepted")
	}

	issued, err := e.auth.Redeem(e.ctx, token, nodeID, csr(t))
	if err != nil {
		t.Fatalf("redeem after failed attempts: %v", err)
	}
	want := pki.Identity{ClusterID: issued.Identity.ClusterID, NodeID: nodeID, Role: pki.RoleProbe, TenantID: cluster.DefaultTenantID}
	if issued.Identity != want {
		t.Errorf("identity = %+v", issued.Identity)
	}
	if _, err := e.verifier(t, nil).Verify([]*x509.Certificate{issued.Leaf}, x509.ExtKeyUsageClientAuth); err != nil {
		t.Errorf("issued certificate does not verify: %v", err)
	}

	// Single use, also for the same node and request shape.
	for _, id := range []string{nodeID, uuid.NewString()} {
		if _, err := e.auth.Redeem(e.ctx, token, id, csr(t)); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("second redeem for %s: err = %v", id, err)
		}
	}
}

func TestJoinTokenExpires(t *testing.T) {
	e := newEnv(t)
	// The FSM decides with the leader's command time, so the token must
	// really expire between creation and redemption.
	short, _, err := e.auth.CreateJoinToken(e.ctx, pki.RoleVoter, "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := e.auth.Redeem(e.ctx, short, uuid.NewString(), csr(t)); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("expired token: err = %v", err)
	}

	for name, f := range map[string]func() error{
		"too long":       func() error { _, _, err := e.auth.CreateJoinToken(e.ctx, pki.RoleVoter, "", 25*time.Hour); return err },
		"tenant voter":   func() error { _, _, err := e.auth.CreateJoinToken(e.ctx, pki.RoleVoter, "default", 0); return err },
		"unknown tenant": func() error { _, _, err := e.auth.CreateJoinToken(e.ctx, pki.RoleProbe, "nope", 0); return err },
		"bad role":       func() error { _, _, err := e.auth.CreateJoinToken(e.ctx, "admin", "", 0); return err },
	} {
		if f() == nil {
			t.Errorf("%s: token created", name)
		}
	}
}

// FA-PKI-03/04 with a real handshake: the node certificate is issued,
// stored, renewed and revoked.
func TestNodeCertificateLifecycle(t *testing.T) {
	e := newEnv(t)
	nodeID := uuid.NewString()
	nc := &NodeCert{DataDir: e.dir, NodeID: nodeID}
	if err := nc.EnsureSelf(e.ctx, e.auth, false, e.clock); err != nil || nc.Leaf() != nil {
		t.Fatalf("follower without certificate: %v, leaf %v", err, nc.Leaf())
	}
	if err := nc.EnsureSelf(e.ctx, e.auth, true, e.clock); err != nil {
		t.Fatal(err)
	}
	first := nc.Leaf()
	if first == nil || nc.Holder.Get() == nil {
		t.Fatal("no certificate after EnsureSelf")
	}
	info, err := os.Stat(filepath.Join(e.dir, tlsDir, keyFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, %v", info.Mode(), err)
	}

	// A restart loads the stored certificate instead of issuing a new one.
	again := &NodeCert{DataDir: e.dir, NodeID: nodeID}
	if err := again.EnsureSelf(e.ctx, e.auth, true, e.clock); err != nil || pki.SerialString(again.Leaf()) != pki.SerialString(first) {
		t.Fatalf("reload: %v", err)
	}
	if err := (&NodeCert{DataDir: e.dir, NodeID: "someone-else"}).Load(); err == nil {
		t.Error("loaded a certificate of another node")
	}

	// Renewal after two thirds of the lifetime.
	e.clock = pki.RenewAt(first).Add(time.Minute)
	if err := again.EnsureSelf(e.ctx, e.auth, true, e.clock); err != nil {
		t.Fatal(err)
	}
	if pki.SerialString(again.Leaf()) == pki.SerialString(first) {
		t.Fatal("certificate not renewed")
	}

	// Revocation reaches handshakes after a reload (FA-PKI-04).
	rev := &Revocations{}
	if err := rev.Reload(e.ctx, e.state); err != nil {
		t.Fatal(err)
	}
	peer := &NodeCert{DataDir: t.TempDir(), NodeID: uuid.NewString()}
	if err := peer.EnsureSelf(e.ctx, e.auth, true, e.clock); err != nil {
		t.Fatal(err)
	}
	v := e.verifier(t, rev)
	v.Now = func() time.Time { return e.clock }
	server := pki.ServerConfig(&again.Holder, v, true, pki.OnlyVoters)
	client := pki.ClientConfig(&peer.Holder, v, nodeID, pki.OnlyVoters)
	if s, c := handshake(t, server, client); s != nil || c != nil {
		t.Fatalf("handshake before revocation: %v / %v", s, c)
	}
	if err := e.auth.Revoke(e.ctx, peer.Leaf()); err != nil {
		t.Fatal(err)
	}
	if err := rev.Reload(e.ctx, e.state); err != nil {
		t.Fatal(err)
	}
	if !rev.Revoked(pki.SerialString(peer.Leaf())) {
		t.Fatal("serial not in the revocation list")
	}
	if s, _ := handshake(t, server, client); !errors.Is(s, pki.ErrRevoked) {
		t.Errorf("handshake after revocation: server error = %v", s)
	}
}

func handshake(t *testing.T, server, client *tls.Config) (srvErr, cliErr error) {
	t.Helper()
	a, b := net.Pipe()
	defer func() { _ = a.Close() }()
	defer func() { _ = b.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	wg.Go(func() {
		srvErr = tls.Server(a, server).HandshakeContext(ctx)
		if srvErr != nil {
			_ = a.Close()
		}
	})
	c := tls.Client(b, client)
	if cliErr = c.HandshakeContext(ctx); cliErr != nil {
		_ = b.Close()
	} else {
		go func() { _, _ = io.Copy(io.Discard, c) }()
	}
	wg.Wait()
	return srvErr, cliErr
}
