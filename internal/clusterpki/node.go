// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package clusterpki

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/joinsider/uptimecluster/internal/pki"
	"github.com/joinsider/uptimecluster/internal/store"
)

// Revocations caches the revocation list for TLS handshakes (FA-PKI-04):
// revoked serials and removed nodes. Both lists are small.
type Revocations struct {
	list atomic.Pointer[revocationList]
}

type revocationList struct {
	serials map[string]struct{}
	nodes   map[string]time.Time
}

// Revoked reports whether a serial is revoked. Before the first load
// nothing is revoked.
func (r *Revocations) Revoked(serial string) bool {
	l := r.list.Load()
	if l == nil {
		return false
	}
	_, ok := l.serials[serial]
	return ok
}

// RevokedNode returns when a node was revoked (pki.Verifier.RevokedNode).
func (r *Revocations) RevokedNode(nodeID string) (time.Time, bool) {
	l := r.list.Load()
	if l == nil {
		return time.Time{}, false
	}
	at, ok := l.nodes[nodeID]
	return at, ok
}

// Reload reads the lists from the state.
func (r *Revocations) Reload(ctx context.Context, state store.State) error {
	certs, err := state.ListRevokedCerts(ctx)
	if err != nil {
		return err
	}
	nodes, err := state.ListRevokedNodes(ctx)
	if err != nil {
		return err
	}
	l := &revocationList{serials: make(map[string]struct{}, len(certs)), nodes: make(map[string]time.Time, len(nodes))}
	for _, c := range certs {
		l.serials[c.Serial] = struct{}{}
	}
	for _, n := range nodes {
		l.nodes[n.NodeID] = time.UnixMilli(n.RevokedMs)
	}
	r.list.Store(l)
	return nil
}

// Run reloads the list every interval, so revocations replicated through
// Raft reach the handshakes of every node within that time.
func (r *Revocations) Run(ctx context.Context, state store.State, interval time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := r.Reload(ctx, state); err != nil && ctx.Err() == nil {
			log.Warn("reloading the certificate revocation list failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// File names in DATA_DIR/tls. The key never leaves this node (FA-PKI-01).
const (
	tlsDir   = "tls"
	keyFile  = "node-key.pem"
	certFile = "node-cert.pem"
)

// NodeCert manages this node's key and certificate and serves them through
// Holder; a renewed certificate applies from the next handshake (FA-PKI-03).
type NodeCert struct {
	DataDir string
	NodeID  string
	Holder  pki.Holder
	// Now is the wall clock for renewal decisions; nil uses time.Now. Tests
	// set it to simulate clock skew between nodes (ADR 0020).
	Now func() time.Time

	// mu guards key, leaf and ca: handshakes read them while renewal
	// replaces them.
	mu   sync.RWMutex
	key  *ecdsa.PrivateKey
	leaf *x509.Certificate
	ca   *x509.Certificate
}

// Load reads the stored key and certificate, if any. A certificate whose key
// does not match is an error, not a reason to silently start over.
func (n *NodeCert) Load() error {
	keyPEM, err := os.ReadFile(filepath.Join(n.DataDir, tlsDir, keyFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read node key: %w", err)
	}
	certPEM, err := os.ReadFile(filepath.Join(n.DataDir, tlsDir, certFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read node certificate: %w", err)
	}
	key, err := parseKey(keyPEM)
	if err != nil {
		return err
	}
	chain, err := parseChain(certPEM)
	if err != nil {
		return err
	}
	if !key.PublicKey.Equal(chain[0].PublicKey) {
		return errors.New("node key does not match the node certificate")
	}
	id, err := pki.IdentityOf(chain[0])
	if err != nil {
		return err
	}
	if id.NodeID != n.NodeID {
		return fmt.Errorf("node certificate belongs to node %s, not %s", id.NodeID, n.NodeID)
	}
	n.set(key, chain)
	return nil
}

// Leaf returns the current certificate, or nil.
func (n *NodeCert) Leaf() *x509.Certificate {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.leaf
}

// CA returns the CA certificate the node certificate was issued by, or nil.
// It is the trust anchor of a joined node until the replicated state with
// the CA bundle has arrived.
func (n *NodeCert) CA() *x509.Certificate {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.ca
}

// NeedsRenewal reports whether there is no certificate or it reached its
// renewal time.
func (n *NodeCert) NeedsRenewal(now time.Time) bool {
	leaf := n.Leaf()
	return leaf == nil || !now.Before(pki.RenewAt(leaf))
}

// NewCSR creates a fresh key for the next certificate and returns its CSR;
// Install activates key and certificate together.
func (n *NodeCert) NewCSR() (*ecdsa.PrivateKey, []byte, error) {
	key, err := pki.NewKey()
	if err != nil {
		return nil, nil, err
	}
	csr, err := pki.NewCSR(key)
	if err != nil {
		return nil, nil, err
	}
	return key, csr, nil
}

// Install stores key and certificate (key file 0600, written atomically) and
// makes them the active certificate.
func (n *NodeCert) Install(key *ecdsa.PrivateKey, issued Issued) error {
	chain := make([]*x509.Certificate, 0, len(issued.ChainDER))
	for _, der := range issued.ChainDER {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return fmt.Errorf("issued certificate: %w", err)
		}
		chain = append(chain, c)
	}
	if len(chain) < 2 || !key.PublicKey.Equal(chain[0].PublicKey) {
		return errors.New("issued certificate does not match the node key")
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal node key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	clear(keyDER)
	var certPEM []byte
	for _, c := range chain {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	dir := filepath.Join(n.DataDir, tlsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	// Key first: a crash between the two writes leaves a mismatch that Load
	// reports instead of a certificate without its key.
	if err := writeAtomic(filepath.Join(dir, keyFile), keyPEM, 0o600); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, certFile), certPEM, 0o644); err != nil {
		return err
	}
	n.set(key, chain)
	return nil
}

func (n *NodeCert) set(key *ecdsa.PrivateKey, chain []*x509.Certificate) {
	n.mu.Lock()
	n.key, n.leaf, n.ca = key, chain[0], chain[1]
	n.mu.Unlock()
	n.Holder.Set(pki.Leaf(chain[0], key, chain[1]))
}

func writeAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func parseKey(data []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("node key: no PKCS#8 PEM block")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("node key: %w", err)
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("node key: unsupported type %T", k)
	}
	return key, nil
}

func parseChain(data []byte) ([]*x509.Certificate, error) {
	var chain []*x509.Certificate
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("node certificate: %w", err)
		}
		chain = append(chain, c)
	}
	if len(chain) < 2 {
		return nil, errors.New("node certificate: expected leaf and CA certificate")
	}
	return chain, nil
}

// IssueFunc obtains a certificate for a CSR: from the CA on the leader,
// through RenewCertificate on the leader for followers.
type IssueFunc func(ctx context.Context, csrDER []byte) (Issued, error)

// Renew loads the stored certificate if needed and, when there is none or it
// is due (FA-PKI-03), obtains a new one with a fresh key through issue. A nil
// issue only loads.
func (n *NodeCert) Renew(ctx context.Context, now time.Time, issue IssueFunc) error {
	if n.Leaf() == nil {
		if err := n.Load(); err != nil {
			return err
		}
	}
	if !n.NeedsRenewal(now) || issue == nil {
		return nil
	}
	key, csr, err := n.NewCSR()
	if err != nil {
		return err
	}
	issued, err := issue(ctx, csr)
	if err != nil {
		return err
	}
	return n.Install(key, issued)
}

// EnsureSelf gives the leader a valid certificate issued directly by the CA
// (bootstrap node, leader renewing its own); on followers it only loads.
func (n *NodeCert) EnsureSelf(ctx context.Context, a *Authority, isLeader bool, now time.Time) error {
	var issue IssueFunc
	if isLeader {
		issue = func(ctx context.Context, csr []byte) (Issued, error) { return a.IssueSelf(ctx, n.NodeID, csr) }
	}
	return n.Renew(ctx, now, issue)
}

func (n *NodeCert) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now()
}

// RunRenewal checks every interval whether the certificate is due and
// renews it through the issuer chosen at that moment (FA-PKI-03).
func (n *NodeCert) RunRenewal(ctx context.Context, issuer func() IssueFunc, interval time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := n.Renew(ctx, n.now(), issuer()); err != nil && ctx.Err() == nil {
			log.Warn("renewing the node certificate failed", "err", err)
		}
	}
}
