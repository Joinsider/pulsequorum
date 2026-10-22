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
	"sync/atomic"
	"time"

	"github.com/joinsider/uptimecluster/internal/pki"
	"github.com/joinsider/uptimecluster/internal/store"
)

// Revocations caches the revocation list for TLS handshakes (FA-PKI-04).
// The list is small (only unexpired certificates of removed nodes).
type Revocations struct {
	serials atomic.Pointer[map[string]struct{}]
}

// Revoked reports whether a serial is revoked. Before the first load
// nothing is revoked.
func (r *Revocations) Revoked(serial string) bool {
	m := r.serials.Load()
	if m == nil {
		return false
	}
	_, ok := (*m)[serial]
	return ok
}

// Reload reads the list from the state.
func (r *Revocations) Reload(ctx context.Context, state store.State) error {
	list, err := state.ListRevokedCerts(ctx)
	if err != nil {
		return err
	}
	m := make(map[string]struct{}, len(list))
	for _, c := range list {
		m[c.Serial] = struct{}{}
	}
	r.serials.Store(&m)
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

	key  *ecdsa.PrivateKey
	leaf *x509.Certificate
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
func (n *NodeCert) Leaf() *x509.Certificate { return n.leaf }

// NeedsRenewal reports whether there is no certificate or it reached its
// renewal time.
func (n *NodeCert) NeedsRenewal(now time.Time) bool {
	return n.leaf == nil || !now.Before(pki.RenewAt(n.leaf))
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
	n.key, n.leaf = key, chain[0]
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

// EnsureSelf gives the leader a valid certificate: it loads the stored one
// and issues a new one directly from the CA when there is none or it is due
// for renewal. Followers get theirs at join and renew through the leader
// (AP-2.2/2.3); for them EnsureSelf only loads.
func (n *NodeCert) EnsureSelf(ctx context.Context, a *Authority, isLeader bool, now time.Time) error {
	if n.leaf == nil {
		if err := n.Load(); err != nil {
			return err
		}
	}
	if !n.NeedsRenewal(now) || !isLeader {
		return nil
	}
	key, csr, err := n.NewCSR()
	if err != nil {
		return err
	}
	issued, err := a.IssueSelf(ctx, n.NodeID, csr)
	if err != nil {
		return err
	}
	return n.Install(key, issued)
}

// RunRenewal checks every interval whether the certificate is due and
// renews it (FA-PKI-03).
func (n *NodeCert) RunRenewal(ctx context.Context, a *Authority, isLeader func() bool, interval time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := n.EnsureSelf(ctx, a, isLeader(), time.Now()); err != nil && ctx.Err() == nil {
			log.Warn("renewing the node certificate failed", "err", err)
		}
	}
}
