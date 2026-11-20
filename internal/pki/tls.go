// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package pki

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

// Errors of peer verification.
var (
	ErrNoPeerCertificate = errors.New("peer presented no certificate")
	ErrRevoked           = errors.New("peer certificate is revoked")
	ErrWrongCluster      = errors.New("peer belongs to another cluster")
	ErrWrongPeer         = errors.New("peer is not the expected node")
)

// Verifier checks peer certificates against the trusted CA bundle and the
// revocation list (FA-PKI-01, FA-PKI-04). Its functions are read on every
// handshake, so updates (CA rotation, new revocations) apply immediately.
type Verifier struct {
	ClusterID string
	// ClusterIDFunc, if set, replaces ClusterID; nodes learn the cluster ID
	// only after bootstrap or join.
	ClusterIDFunc func() string
	// Roots returns the trusted CA certificates (several during CA rotation).
	Roots func() []*x509.Certificate
	// Revoked reports whether a serial (SerialString) is revoked.
	Revoked func(serial string) bool
	// RevokedNode returns when a node was revoked (removed from the cluster);
	// its certificates issued before that time are rejected, a later re-join
	// gets a valid one.
	RevokedNode func(nodeID string) (time.Time, bool)
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
}

// Verify checks a peer chain (leaf first) for the given usage and returns the
// peer's identity. Only certificates issued directly by a trusted CA are
// accepted.
func (v *Verifier) Verify(chain []*x509.Certificate, usage x509.ExtKeyUsage) (Identity, error) {
	if len(chain) == 0 {
		return Identity{}, ErrNoPeerCertificate
	}
	leaf := chain[0]
	roots := x509.NewCertPool()
	for _, c := range v.Roots() {
		roots.AddCert(c)
	}
	now := time.Now()
	if v.Now != nil {
		now = v.Now()
	}
	// No intermediates: the CA signs leaves directly (MaxPathLenZero).
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{usage}}); err != nil {
		return Identity{}, fmt.Errorf("verify peer certificate: %w", err)
	}
	if v.Revoked != nil && v.Revoked(SerialString(leaf)) {
		return Identity{}, ErrRevoked
	}
	id, err := IdentityOf(leaf)
	if err != nil {
		return Identity{}, err
	}
	if v.RevokedNode != nil {
		// NotBefore is backdated; the certificate was issued Backdate later.
		if at, ok := v.RevokedNode(id.NodeID); ok && !leaf.NotBefore.Add(Backdate).After(at) {
			return Identity{}, ErrRevoked
		}
	}
	want := v.ClusterID
	if v.ClusterIDFunc != nil {
		want = v.ClusterIDFunc()
	}
	if want == "" || id.ClusterID != want {
		return Identity{}, ErrWrongCluster
	}
	return id, nil
}

// Holder serves the node's current certificate to TLS callbacks; replacing it
// takes effect at the next handshake (FA-PKI-03).
type Holder struct {
	cert atomic.Pointer[tls.Certificate]
}

// Set replaces the certificate.
func (h *Holder) Set(c *tls.Certificate) { h.cert.Store(c) }

// Get returns the current certificate, or nil.
func (h *Holder) Get() *tls.Certificate { return h.cert.Load() }

func (h *Holder) get() (*tls.Certificate, error) {
	if c := h.cert.Load(); c != nil {
		return c, nil
	}
	return nil, errors.New("node certificate is not available yet")
}

// PeerCheck may restrict which identities are accepted (e.g. only voters).
type PeerCheck func(Identity) error

// ServerConfig returns the TLS configuration of the cluster port. With
// requireClient false a client may connect without a certificate (only the
// Join RPC allows that); a presented certificate is always verified, and
// callers must check ConnectionState().PeerCertificates per RPC.
//
// Session tickets are disabled and all checks run in VerifyConnection, which
// also runs for resumed sessions, so a revoked certificate cannot reconnect
// through resumption.
func ServerConfig(h *Holder, v *Verifier, requireClient bool, check PeerCheck, nextProtos ...string) *tls.Config {
	auth := tls.RequestClientCert
	if requireClient {
		auth = tls.RequireAnyClientCert
	}
	return &tls.Config{
		MinVersion:             tls.VersionTLS13,
		NextProtos:             nextProtos,
		SessionTicketsDisabled: true,
		ClientAuth:             auth, // chain verification happens in VerifyConnection
		GetCertificate:         func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return h.get() },
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				if requireClient {
					return ErrNoPeerCertificate
				}
				return nil
			}
			id, err := v.Verify(cs.PeerCertificates, x509.ExtKeyUsageClientAuth)
			if err == nil && check != nil {
				err = check(id)
			}
			return err
		},
	}
}

// ClientConfig returns the TLS configuration for connecting to another node.
// The server is verified against the CA bundle instead of a host name; with
// expectNodeID set, only that node is accepted.
func ClientConfig(h *Holder, v *Verifier, expectNodeID string, check PeerCheck, nextProtos ...string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		NextProtos: nextProtos,
		// Host names are not part of node identities; VerifyConnection does
		// the full chain, revocation and identity check instead.
		InsecureSkipVerify:   true, //nolint:gosec // G402: verified in VerifyConnection
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return h.get() },
		VerifyConnection: func(cs tls.ConnectionState) error {
			id, err := v.Verify(cs.PeerCertificates, x509.ExtKeyUsageServerAuth)
			if err != nil {
				return err
			}
			if expectNodeID != "" && id.NodeID != expectNodeID {
				return ErrWrongPeer
			}
			if check != nil {
				return check(id)
			}
			return nil
		},
	}
}

// OnlyVoters accepts voter certificates only (Raft, replication between voters).
func OnlyVoters(id Identity) error {
	if id.Role != RoleVoter {
		return fmt.Errorf("role %s is not allowed here", id.Role)
	}
	return nil
}

// JoinClientConfig is used by a joining node that has no certificate yet. It
// trusts only a voter certificate issued by a CA whose fingerprint matches
// the join token (FA-PKI-02); the server must send the CA certificate after
// its leaf. It returns the pinned CA through ca once the handshake succeeded.
func JoinClientConfig(fingerprint string, ca *atomic.Pointer[x509.Certificate]) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{"h2"},
		InsecureSkipVerify: true, //nolint:gosec // G402: pinned to the CA fingerprint in VerifyConnection
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) < 2 {
				return errors.New("server did not send its CA certificate")
			}
			var pinned *x509.Certificate
			for _, c := range cs.PeerCertificates[1:] {
				if c.IsCA && Fingerprint(c) == fingerprint {
					pinned = c
					break
				}
			}
			if pinned == nil {
				return errors.New("server CA does not match the fingerprint in the join token")
			}
			v := Verifier{
				ClusterID: clusterIDOfCA(pinned),
				Roots:     func() []*x509.Certificate { return []*x509.Certificate{pinned} },
			}
			id, err := v.Verify(cs.PeerCertificates[:1], x509.ExtKeyUsageServerAuth)
			if err != nil {
				return err
			}
			if err := OnlyVoters(id); err != nil {
				return err
			}
			if ca != nil {
				ca.Store(pinned)
			}
			return nil
		},
	}
}

func clusterIDOfCA(cert *x509.Certificate) string {
	if len(cert.URIs) == 1 {
		return cert.URIs[0].Host
	}
	return ""
}

// Leaf builds the TLS certificate of a node: its leaf followed by the CA, so
// joining nodes can pin the CA (JoinClientConfig).
func Leaf(leaf *x509.Certificate, key any, ca *x509.Certificate) *tls.Certificate {
	return &tls.Certificate{Certificate: [][]byte{leaf.Raw, ca.Raw}, PrivateKey: key, Leaf: leaf}
}
