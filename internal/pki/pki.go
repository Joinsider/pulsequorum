// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package pki implements the cluster's own certificate authority (ADR 0006,
// FA-PKI-01..04): an ECDSA P-256 CA created at bootstrap, node certificates
// issued from CSRs, peer verification against the CA bundle and the
// revocation list, and one-time join tokens that pin the CA fingerprint.
//
// Node identities are carried in a URI SAN
// "uc://<cluster-id>/<role>/<node-id>[?tenant=<tenant-id>]"; host names are
// not used, because nodes move and advertise addresses change (FA-CLU-11).
package pki

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"time"
)

// Validity periods (ADR 0006, FA-PKI-03).
const (
	CAValidity   = 10 * 365 * 24 * time.Hour
	LeafValidity = 90 * 24 * time.Hour
	// Backdate tolerates clock skew between nodes (PF-11).
	Backdate = 5 * time.Minute
)

// Role is the cluster role a certificate grants.
type Role string

// Roles (FA-PKI-02).
const (
	RoleVoter Role = "voter"
	RoleProbe Role = "probe"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool { return r == RoleVoter || r == RoleProbe }

const uriScheme = "uc"

// Identity is who a node certificate belongs to.
type Identity struct {
	ClusterID string
	NodeID    string
	Role      Role
	// TenantID is set for private probes of a tenant (FA-PRB-05).
	TenantID string
}

// URI returns the identity as URI SAN.
func (id Identity) URI() *url.URL {
	u := &url.URL{Scheme: uriScheme, Host: id.ClusterID, Path: "/" + string(id.Role) + "/" + id.NodeID}
	if id.TenantID != "" {
		u.RawQuery = url.Values{"tenant": {id.TenantID}}.Encode()
	}
	return u
}

func (id Identity) validate() error {
	switch {
	case !validName(id.ClusterID):
		return errors.New("invalid cluster ID")
	case !validName(id.NodeID):
		return errors.New("invalid node ID")
	case !id.Role.Valid():
		return fmt.Errorf("invalid role %q", id.Role)
	case id.TenantID != "" && (id.Role != RoleProbe || !validName(id.TenantID)):
		return errors.New("a tenant is only allowed for probes")
	}
	return nil
}

// validName accepts the IDs the cluster generates (UUIDs, tenant slugs).
func validName(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, c := range s {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
		if !ok {
			return false
		}
	}
	return true
}

// IdentityOf reads the identity from a node certificate. It does not verify
// the certificate; use Verifier for that.
func IdentityOf(cert *x509.Certificate) (Identity, error) {
	if len(cert.URIs) != 1 {
		return Identity{}, errors.New("certificate must carry exactly one identity URI")
	}
	u := cert.URIs[0]
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if u.Scheme != uriScheme || len(parts) != 2 || u.User != nil || u.Fragment != "" {
		return Identity{}, fmt.Errorf("invalid identity URI %q", u)
	}
	id := Identity{ClusterID: u.Host, Role: Role(parts[0]), NodeID: parts[1]}
	if u.RawQuery != "" {
		q, err := url.ParseQuery(u.RawQuery)
		if err != nil || len(q) != 1 || len(q["tenant"]) != 1 {
			return Identity{}, fmt.Errorf("invalid identity URI %q", u)
		}
		id.TenantID = q.Get("tenant")
	}
	if err := id.validate(); err != nil {
		return Identity{}, fmt.Errorf("identity URI %q: %w", u, err)
	}
	return id, nil
}

// Fingerprint identifies a CA by the SHA-256 of its public key info, as
// embedded in join tokens (FA-PKI-02). It survives re-signing the CA
// certificate with the same key.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// SerialString is the canonical form of a serial number in the revocation list.
func SerialString(cert *x509.Certificate) string { return cert.SerialNumber.Text(16) }

// CA is the cluster certificate authority.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
}

// NewCA creates the CA of a new cluster.
func NewCA(clusterID string, now time.Time) (*CA, error) {
	if !validName(clusterID) {
		return nil, errors.New("new CA: invalid cluster ID")
	}
	key, err := NewKey()
	if err != nil {
		return nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Pulsequorum CA " + clusterID},
		NotBefore:             now.Add(-Backdate),
		NotAfter:              now.Add(CAValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
		URIs:                  []*url.URL{{Scheme: uriScheme, Host: clusterID, Path: "/ca"}},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("new CA: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("new CA: %w", err)
	}
	return &CA{Cert: cert, Key: key}, nil
}

// ParseCA loads a CA from its certificate and PKCS#8 key (both DER).
func ParseCA(certDER, keyDER []byte) (*CA, error) {
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	k, err := x509.ParsePKCS8PrivateKey(keyDER)
	if err != nil {
		return nil, fmt.Errorf("parse CA key: %w", err)
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok || !cert.IsCA || !key.PublicKey.Equal(cert.PublicKey) {
		return nil, errors.New("CA key does not match the CA certificate")
	}
	return &CA{Cert: cert, Key: key}, nil
}

// ClusterID returns the cluster the CA belongs to.
func (ca *CA) ClusterID() string {
	if len(ca.Cert.URIs) == 1 {
		return ca.Cert.URIs[0].Host
	}
	return ""
}

// MarshalKey returns the CA key as PKCS#8 DER, for encryption (ADR 0011).
func (ca *CA) MarshalKey() ([]byte, error) { return x509.MarshalPKCS8PrivateKey(ca.Key) }

// Issue signs a node certificate for the public key of csrDER. The identity
// comes from the caller (join token or existing certificate), never from the
// CSR, whose subject and SANs are ignored.
func (ca *CA) Issue(csrDER []byte, id Identity, now time.Time) (*x509.Certificate, error) {
	if err := id.validate(); err != nil {
		return nil, fmt.Errorf("issue: %w", err)
	}
	if id.ClusterID != ca.ClusterID() {
		return nil, errors.New("issue: identity belongs to another cluster")
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, fmt.Errorf("issue: parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("issue: CSR signature: %w", err)
	}
	if err := checkLeafKey(csr.PublicKey); err != nil {
		return nil, fmt.Errorf("issue: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	notAfter := now.Add(LeafValidity)
	if notAfter.After(ca.Cert.NotAfter) {
		notAfter = ca.Cert.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: id.NodeID},
		NotBefore:    now.Add(-Backdate),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{id.URI()},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, csr.PublicKey, ca.Key)
	if err != nil {
		return nil, fmt.Errorf("issue: %w", err)
	}
	return x509.ParseCertificate(der)
}

// checkLeafKey accepts the key types nodes generate.
func checkLeafKey(pub any) error {
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		if k.Curve != elliptic.P256() {
			return errors.New("ECDSA keys must use P-256")
		}
		return nil
	case ed25519.PublicKey:
		return nil
	}
	return fmt.Errorf("unsupported key type %T", pub)
}

// NewKey creates a node or CA key (ECDSA P-256).
func NewKey() (*ecdsa.PrivateKey, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	return k, nil
}

// NewCSR creates a certificate signing request for key. The private key
// never leaves the node.
func NewCSR(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return nil, fmt.Errorf("create CSR: %w", err)
	}
	return der, nil
}

// RenewAt returns when a certificate should be renewed: after two thirds of
// its lifetime (ADR 0006).
func RenewAt(cert *x509.Certificate) time.Time {
	life := cert.NotAfter.Sub(cert.NotBefore)
	return cert.NotBefore.Add(life * 2 / 3)
}

// newSerial returns a random 128-bit serial number (RFC 5280: positive, ≤ 20 bytes).
func newSerial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("serial: %w", err)
	}
	return n.Add(n, big.NewInt(1)), nil
}
