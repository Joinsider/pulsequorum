// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package pki

import (
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"crypto/rand"
)

var now = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

type node struct {
	key  *ecdsa.PrivateKey
	cert *x509.Certificate
}

func newCA(t *testing.T, cluster string) *CA {
	t.Helper()
	ca, err := NewCA(cluster, now)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func issue(t *testing.T, ca *CA, id Identity, at time.Time) node {
	t.Helper()
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := NewCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.Issue(csr, id, at)
	if err != nil {
		t.Fatal(err)
	}
	return node{key: key, cert: cert}
}

func voter(cluster, id string) Identity {
	return Identity{ClusterID: cluster, NodeID: id, Role: RoleVoter}
}

func TestCARoundTripAndIssue(t *testing.T) {
	ca := newCA(t, "c1")
	if ca.ClusterID() != "c1" || !ca.Cert.IsCA {
		t.Fatalf("CA = %+v", ca.Cert.URIs)
	}
	keyDER, err := ca.MarshalKey()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := ParseCA(ca.Cert.Raw, keyDER)
	if err != nil || Fingerprint(loaded.Cert) != Fingerprint(ca.Cert) {
		t.Fatalf("ParseCA = %v", err)
	}
	other := newCA(t, "c1")
	otherKey, _ := other.MarshalKey()
	if _, err := ParseCA(ca.Cert.Raw, otherKey); err == nil {
		t.Error("ParseCA accepted a key of another CA")
	}

	n := issue(t, ca, Identity{ClusterID: "c1", NodeID: "probe-1", Role: RoleProbe, TenantID: "t1"}, now)
	id, err := IdentityOf(n.cert)
	if err != nil || id != (Identity{ClusterID: "c1", NodeID: "probe-1", Role: RoleProbe, TenantID: "t1"}) {
		t.Fatalf("identity = %+v, %v", id, err)
	}
	if got := n.cert.NotBefore; !got.Equal(now.Add(-Backdate)) {
		t.Errorf("NotBefore = %v, want backdated by %v", got, Backdate)
	}
	if got := n.cert.NotAfter.Sub(now); got != LeafValidity {
		t.Errorf("validity = %v", got)
	}
	if want := n.cert.NotBefore.Add(n.cert.NotAfter.Sub(n.cert.NotBefore) * 2 / 3); !RenewAt(n.cert).Equal(want) {
		t.Errorf("RenewAt = %v", RenewAt(n.cert))
	}
}

func TestIssueRejects(t *testing.T) {
	ca := newCA(t, "c1")
	key, _ := NewKey()
	csr, _ := NewCSR(key)

	// The CSR's own subject and SANs never become the identity.
	evil, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "admin"}, DNSNames: []string{"leader"},
	}, key)
	c, err := ca.Issue(evil, voter("c1", "n1"), now)
	if err != nil || c.Subject.CommonName != "n1" || len(c.DNSNames) != 0 {
		t.Errorf("CSR fields leaked into the certificate: %+v, %v", c, err)
	}

	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	rsaCSR, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, rsaKey)
	tampered := append([]byte(nil), csr...)
	tampered[len(tampered)-5] ^= 0xff

	tests := []struct {
		name string
		csr  []byte
		id   Identity
	}{
		{"other cluster", csr, voter("c2", "n1")},
		{"bad role", csr, Identity{ClusterID: "c1", NodeID: "n1", Role: "admin"}},
		{"tenant on voter", csr, Identity{ClusterID: "c1", NodeID: "n1", Role: RoleVoter, TenantID: "t1"}},
		{"bad node ID", csr, voter("c1", "n1/../x")},
		{"RSA key", rsaCSR, voter("c1", "n1")},
		{"bad signature", tampered, voter("c1", "n1")},
		{"garbage", []byte("x"), voter("c1", "n1")},
	}
	for _, tc := range tests {
		if _, err := ca.Issue(tc.csr, tc.id, now); err == nil {
			t.Errorf("%s: issued", tc.name)
		}
	}
}

// handshake runs a TLS connection over an in-memory pipe and returns the
// errors of both sides and the identity the server saw.
func handshake(t *testing.T, server, client *tls.Config) (srvErr, cliErr error) {
	t.Helper()
	a, b := net.Pipe()
	defer func() { _ = a.Close() }()
	defer func() { _ = b.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	wg.Go(func() {
		s := tls.Server(a, server)
		srvErr = s.HandshakeContext(ctx)
		if srvErr != nil {
			_ = a.Close()
		}
	})
	c := tls.Client(b, client)
	cliErr = c.HandshakeContext(ctx)
	if cliErr != nil {
		_ = b.Close()
	} else {
		// TLS 1.3: the client finishes before the server verified the
		// client certificate; a read surfaces the server's rejection.
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		go func() { _, _ = io.Copy(io.Discard, c) }()
	}
	wg.Wait()
	return srvErr, cliErr
}

type cluster struct {
	ca          *CA
	revoked     sync.Map
	revokedNode map[string]time.Time
	now         time.Time
}

func (c *cluster) verifier() *Verifier {
	return &Verifier{
		ClusterID: c.ca.ClusterID(),
		Roots:     func() []*x509.Certificate { return []*x509.Certificate{c.ca.Cert} },
		Revoked:   func(s string) bool { _, ok := c.revoked.Load(s); return ok },
		RevokedNode: func(id string) (time.Time, bool) {
			at, ok := c.revokedNode[id]
			return at, ok
		},
		Now: func() time.Time { return c.now },
	}
}

func holder(n node, ca *CA) *Holder {
	h := &Holder{}
	h.Set(Leaf(n.cert, n.key, ca.Cert))
	return h
}

// FA-PKI-01/04: mTLS with the cluster CA; no, foreign, expired, revoked or
// wrong-role certificates are rejected.
func TestMutualTLS(t *testing.T) {
	c := &cluster{ca: newCA(t, "c1"), now: now}
	srv := issue(t, c.ca, voter("c1", "srv"), now)
	cli := issue(t, c.ca, voter("c1", "cli"), now)
	probe := issue(t, c.ca, Identity{ClusterID: "c1", NodeID: "p1", Role: RoleProbe}, now)
	foreignCA := newCA(t, "c1") // same cluster ID, different key
	foreign := issue(t, foreignCA, voter("c1", "cli"), now)
	otherCluster := newCA(t, "c2")
	stranger := issue(t, otherCluster, voter("c2", "cli"), now)

	serverCfg := ServerConfig(holder(srv, c.ca), c.verifier(), true, OnlyVoters)
	clientCfg := func(n node, ca *CA) *tls.Config {
		return ClientConfig(holder(n, ca), c.verifier(), "srv", OnlyVoters)
	}

	if s, cl := handshake(t, serverCfg, clientCfg(cli, c.ca)); s != nil || cl != nil {
		t.Fatalf("valid voters: server %v, client %v", s, cl)
	}
	noCert := ClientConfig(&Holder{}, c.verifier(), "srv", nil)
	noCert.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &tls.Certificate{}, nil }
	for name, cfg := range map[string]*tls.Config{
		"no client certificate": noCert,
		"foreign CA":            clientCfg(foreign, foreignCA),
		"other cluster":         clientCfg(stranger, otherCluster),
		"probe on voter port":   clientCfg(probe, c.ca),
	} {
		if s, _ := handshake(t, serverCfg, cfg); s == nil {
			t.Errorf("%s: server accepted", name)
		}
	}

	// Revocation applies to the next handshake (FA-PKI-04).
	c.revoked.Store(SerialString(cli.cert), true)
	if s, _ := handshake(t, serverCfg, clientCfg(cli, c.ca)); !errors.Is(s, ErrRevoked) {
		t.Errorf("revoked client: server error = %v", s)
	}
	c.revoked.Delete(SerialString(cli.cert))

	// Revocation of the node rejects its older certificates, not newer ones.
	revokedAt := now.Add(time.Minute)
	c.revokedNode = map[string]time.Time{"cli": revokedAt}
	if s, _ := handshake(t, serverCfg, clientCfg(cli, c.ca)); !errors.Is(s, ErrRevoked) {
		t.Errorf("client of a revoked node: server error = %v", s)
	}
	rejoined := issue(t, c.ca, voter("c1", "cli"), revokedAt.Add(time.Second))
	c.now = revokedAt.Add(2 * time.Second)
	if s, cl := handshake(t, serverCfg, clientCfg(rejoined, c.ca)); s != nil || cl != nil {
		t.Errorf("re-joined node: server %v, client %v", s, cl)
	}
	c.revokedNode, c.now = nil, now

	// Expiry, with the clock past NotAfter.
	c.now = cli.cert.NotAfter.Add(time.Minute)
	if s, _ := handshake(t, serverCfg, clientCfg(cli, c.ca)); s == nil {
		t.Error("expired client: server accepted")
	}
	c.now = now

	// The client checks the server, too: wrong node and revoked server.
	if _, cl := handshake(t, serverCfg, ClientConfig(holder(cli, c.ca), c.verifier(), "someone-else", nil)); !errors.Is(cl, ErrWrongPeer) {
		t.Errorf("wrong server node: client error = %v", cl)
	}
	c.revoked.Store(SerialString(srv.cert), true)
	if _, cl := handshake(t, serverCfg, clientCfg(cli, c.ca)); !errors.Is(cl, ErrRevoked) {
		t.Errorf("revoked server: client error = %v", cl)
	}
}

// The Join RPC is reachable without a client certificate, but a presented
// one is still verified.
func TestOptionalClientCertificate(t *testing.T) {
	c := &cluster{ca: newCA(t, "c1"), now: now}
	srv := issue(t, c.ca, voter("c1", "srv"), now)
	serverCfg := ServerConfig(holder(srv, c.ca), c.verifier(), false, nil)

	var pinned atomic.Pointer[x509.Certificate]
	if s, cl := handshake(t, serverCfg, JoinClientConfig(Fingerprint(c.ca.Cert), &pinned)); s != nil || cl != nil {
		t.Fatalf("join handshake: server %v, client %v", s, cl)
	}
	if pinned.Load() == nil || Fingerprint(pinned.Load()) != Fingerprint(c.ca.Cert) {
		t.Error("pinned CA not returned")
	}
	foreignCA := newCA(t, "c1")
	foreign := issue(t, foreignCA, voter("c1", "x"), now)
	if s, _ := handshake(t, serverCfg, ClientConfig(holder(foreign, foreignCA), c.verifier(), "", nil)); s == nil {
		t.Error("foreign client certificate accepted on the optional port")
	}
}

// FA-PKI-02: the joining node only talks to a server whose CA matches the
// fingerprint in the token.
func TestJoinPinsCAFingerprint(t *testing.T) {
	c := &cluster{ca: newCA(t, "c1"), now: now}
	srv := issue(t, c.ca, voter("c1", "srv"), now)
	serverCfg := ServerConfig(holder(srv, c.ca), c.verifier(), false, nil)

	attacker := newCA(t, "c1")
	if _, cl := handshake(t, serverCfg, JoinClientConfig(Fingerprint(attacker.Cert), nil)); cl == nil {
		t.Error("join accepted a server of another CA")
	}

	// A server that does not send its CA cannot be pinned.
	leafOnly := &Holder{}
	leafOnly.Set(&tls.Certificate{Certificate: [][]byte{srv.cert.Raw}, PrivateKey: srv.key, Leaf: srv.cert})
	if _, cl := handshake(t, ServerConfig(leafOnly, c.verifier(), false, nil), JoinClientConfig(Fingerprint(c.ca.Cert), nil)); cl == nil {
		t.Error("join accepted a server without CA certificate")
	}

	// A probe certificate signed by the right CA must not pose as a voter.
	probe := issue(t, c.ca, Identity{ClusterID: "c1", NodeID: "p", Role: RoleProbe}, now)
	if _, cl := handshake(t, ServerConfig(holder(probe, c.ca), c.verifier(), false, nil), JoinClientConfig(Fingerprint(c.ca.Cert), nil)); cl == nil {
		t.Error("join accepted a probe as server")
	}
}

// FA-PKI-03: a renewed certificate is used from the next handshake on.
func TestRotationWithoutInterruption(t *testing.T) {
	c := &cluster{ca: newCA(t, "c1"), now: now}
	srv := issue(t, c.ca, voter("c1", "srv"), now)
	h := holder(srv, c.ca)
	var seen atomic.Pointer[x509.Certificate]
	serverCfg := ServerConfig(h, c.verifier(), false, nil)
	cli := issue(t, c.ca, voter("c1", "cli"), now)
	clientCfg := ClientConfig(holder(cli, c.ca), c.verifier(), "srv", nil)
	inner := clientCfg.VerifyConnection
	clientCfg.VerifyConnection = func(cs tls.ConnectionState) error {
		seen.Store(cs.PeerCertificates[0])
		return inner(cs)
	}

	if s, cl := handshake(t, serverCfg, clientCfg); s != nil || cl != nil {
		t.Fatal(s, cl)
	}
	first := SerialString(seen.Load())
	c.now = RenewAt(srv.cert)
	renewed := issue(t, c.ca, voter("c1", "srv"), c.now)
	h.Set(Leaf(renewed.cert, renewed.key, c.ca.Cert))
	if s, cl := handshake(t, serverCfg, clientCfg); s != nil || cl != nil {
		t.Fatal(s, cl)
	}
	if got := SerialString(seen.Load()); got == first || got != SerialString(renewed.cert) {
		t.Errorf("serial after rotation = %s, first %s", got, first)
	}
}

func TestServerWithoutCertificate(t *testing.T) {
	c := &cluster{ca: newCA(t, "c1"), now: now}
	cli := issue(t, c.ca, voter("c1", "cli"), now)
	if s, _ := handshake(t, ServerConfig(&Holder{}, c.verifier(), true, nil), ClientConfig(holder(cli, c.ca), c.verifier(), "", nil)); s == nil {
		t.Error("server without certificate completed a handshake")
	}
}

func TestJoinToken(t *testing.T) {
	ca := newCA(t, "c1")
	tok, err := NewJoinToken(Fingerprint(ca.Cert))
	if err != nil {
		t.Fatal(err)
	}
	s := tok.String()
	if !strings.HasPrefix(s, "uc1.") {
		t.Fatalf("token = %s", s)
	}
	parsed, err := ParseJoinToken(" " + s + "\n")
	if err != nil || parsed != tok {
		t.Fatalf("parse = %+v, %v", parsed, err)
	}
	hash := HashSecret(tok.Secret)
	if strings.Contains(hash, tok.Secret) || !SecretMatches(tok.Secret, hash) {
		t.Error("hash does not match its secret")
	}
	other, _ := NewJoinToken(Fingerprint(ca.Cert))
	if SecretMatches(other.Secret, hash) || SecretMatches(tok.Secret, "") {
		t.Error("foreign secret matched")
	}
	if tok.ID == other.ID || tok.Secret == other.Secret {
		t.Error("tokens are not random")
	}

	parts := strings.Split(s, ".")
	for name, bad := range map[string]string{
		"empty":           "",
		"wrong prefix":    "uc2." + strings.Join(parts[1:], "."),
		"missing part":    strings.Join(parts[:3], "."),
		"extra part":      s + ".x",
		"short secret":    parts[0] + "." + parts[1] + ".abc." + parts[3],
		"bad fingerprint": parts[0] + "." + parts[1] + "." + parts[2] + "." + strings.Repeat("!", 43),
		"padding":         parts[0] + "." + parts[1] + "=." + parts[2] + "." + parts[3],
	} {
		if _, err := ParseJoinToken(bad); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := NewJoinToken("short"); err == nil {
		t.Error("token for an invalid fingerprint")
	}
}

func TestIdentityOfRejectsMalformedURIs(t *testing.T) {
	ca := newCA(t, "c1")
	key, _ := NewKey()
	for _, uri := range []string{"https://c1/voter/n1", "uc://c1/voter", "uc://c1/admin/n1", "uc://c1/voter/n1?tenant=t", "uc://c1/probe/n1?tenant=t&x=1", "uc://u@c1/voter/n1"} {
		tmpl := &x509.Certificate{SerialNumber: ca.Cert.SerialNumber, NotBefore: now, NotAfter: now.Add(time.Hour)}
		u, _ := ca.Cert.URIs[0].Parse(uri)
		tmpl.URIs = append(tmpl.URIs, u)
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
		if err != nil {
			t.Fatal(err)
		}
		cert, _ := x509.ParseCertificate(der)
		if id, err := IdentityOf(cert); err == nil {
			t.Errorf("%s accepted as %+v", uri, id)
		}
	}
}
