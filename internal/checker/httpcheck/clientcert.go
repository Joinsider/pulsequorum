// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package httpcheck

// Client certificates for mTLS targets (ADR 0042, FA-CHK-16).
//
// Security rules for this file:
//   - The private key is a secret (ADR 0015). No error, message or log line
//     built here may contain any part of it; errors are fixed texts.
//   - The certificate is sent only to the host of the monitor URL, never to a
//     host reached through a redirect.

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/joinsider/pulsequorum/internal/checker"
)

// ClientCertCapability is the node capability of binaries that understand
// clientCertificate. The API accepts the field only when every voter has it
// (ADR 0042, section 10).
const ClientCertCapability = "http-client-cert"

const (
	// maskedValue is the placeholder of a stored secret (configsecret.Mask).
	maskedValue   = "***"
	maxChainCerts = 8
	minRSABits    = 2048

	fieldCertificate = "/clientCertificate/certificate"
	fieldPrivateKey  = "/clientCertificate/privateKey"

	pemPrivateKey = "PRIVATE KEY"

	opensslHint = "decrypt or convert it first, e.g. openssl pkey -in key.pem -out key-plain.pem"
)

// ClientCertificate is a PEM certificate chain and its PEM private key. The
// key is marked writeOnly in the schema and therefore stored encrypted.
type ClientCertificate struct {
	Certificate string `json:"certificate"`
	PrivateKey  string `json:"privateKey"`
}

// clientCertError is a validation error of a client certificate field. msg
// is a fixed text and never contains key material.
type clientCertError struct{ field, msg string }

func (e *clientCertError) Error() string { return "client certificate: " + e.msg }

func certErr(field, msg string) *clientCertError { return &clientCertError{field: field, msg: msg} }

// parseChain parses the certificate field: one or more CERTIFICATE blocks,
// the leaf first, and nothing else.
func parseChain(data string) ([]*x509.Certificate, error) {
	rest := []byte(data)
	var chain []*x509.Certificate
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		switch {
		case strings.Contains(block.Type, pemPrivateKey):
			return nil, certErr(fieldCertificate, "contains a private key; put it into privateKey")
		case block.Type != "CERTIFICATE":
			return nil, certErr(fieldCertificate, "only PEM blocks of type CERTIFICATE are allowed")
		}
		if len(chain) == maxChainCerts {
			return nil, certErr(fieldCertificate, fmt.Sprintf("at most %d certificates are allowed", maxChainCerts))
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, certErr(fieldCertificate, fmt.Sprintf("certificate %d cannot be parsed", len(chain)+1))
		}
		chain = append(chain, cert)
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		return nil, certErr(fieldCertificate, "must be PEM; unexpected data after the certificates")
	}
	if len(chain) == 0 {
		return nil, certErr(fieldCertificate, "no PEM certificate found (PKCS#12 is not supported; convert it with openssl pkcs12 -in cert.p12 -clcerts -nokeys)")
	}
	leaf := chain[0]
	if len(leaf.ExtKeyUsage) > 0 && !hasClientAuth(leaf.ExtKeyUsage) {
		return nil, certErr(fieldCertificate, "the certificate does not allow client authentication (extended key usage)")
	}
	return chain, nil
}

func hasClientAuth(usages []x509.ExtKeyUsage) bool {
	for _, u := range usages {
		if u == x509.ExtKeyUsageClientAuth || u == x509.ExtKeyUsageAny {
			return true
		}
	}
	return false
}

// parseKey parses the privateKey field: exactly one unencrypted PKCS#8,
// PKCS#1 or SEC 1 key. An EC PARAMETERS block (openssl ecparam output) is
// skipped. Parser errors are replaced by fixed texts.
func parseKey(data string) (crypto.Signer, error) {
	rest := []byte(data)
	var key *pem.Block
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		switch {
		case block.Type == "EC PARAMETERS":
			continue
		case block.Type == "ENCRYPTED PRIVATE KEY" || strings.Contains(block.Headers["Proc-Type"], "ENCRYPTED"):
			return nil, certErr(fieldPrivateKey, "encrypted private keys are not supported; "+opensslHint)
		case !strings.HasSuffix(block.Type, pemPrivateKey):
			return nil, certErr(fieldPrivateKey, "only a PEM private key is allowed")
		case key != nil:
			return nil, certErr(fieldPrivateKey, "must contain exactly one private key")
		}
		key = block
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		return nil, certErr(fieldPrivateKey, "must be PEM; unexpected data after the key")
	}
	if key == nil {
		return nil, certErr(fieldPrivateKey, "no PEM private key found (PKCS#12 is not supported; "+opensslHint+")")
	}

	var (
		parsed any
		err    error
	)
	switch key.Type {
	case pemPrivateKey:
		parsed, err = x509.ParsePKCS8PrivateKey(key.Bytes)
	case "RSA PRIVATE KEY":
		parsed, err = x509.ParsePKCS1PrivateKey(key.Bytes)
	case "EC PRIVATE KEY":
		parsed, err = x509.ParseECPrivateKey(key.Bytes)
	default:
		return nil, certErr(fieldPrivateKey, "unsupported key format; use PKCS#8, PKCS#1 or SEC 1")
	}
	if err != nil {
		return nil, certErr(fieldPrivateKey, "the private key cannot be parsed")
	}
	return checkKey(parsed)
}

// checkKey enforces the allowed key types: RSA >= 2048 bit, ECDSA P-256 or
// P-384, Ed25519.
func checkKey(parsed any) (crypto.Signer, error) {
	switch k := parsed.(type) {
	case *rsa.PrivateKey:
		if k.N.BitLen() < minRSABits {
			return nil, certErr(fieldPrivateKey, fmt.Sprintf("RSA keys need at least %d bits", minRSABits))
		}
		if k.Validate() != nil {
			return nil, certErr(fieldPrivateKey, "the RSA key is inconsistent")
		}
		return k, nil
	case *ecdsa.PrivateKey:
		if k.Curve != elliptic.P256() && k.Curve != elliptic.P384() {
			return nil, certErr(fieldPrivateKey, "ECDSA keys must use P-256 or P-384")
		}
		return k, nil
	case ed25519.PrivateKey:
		return k, nil
	default:
		return nil, certErr(fieldPrivateKey, "unsupported key type; use RSA, ECDSA or Ed25519")
	}
}

// loadClientCertificate parses and checks cc and returns the certificate to
// present. It fails with a *clientCertError.
func loadClientCertificate(cc *ClientCertificate) (*tls.Certificate, error) {
	chain, err := parseChain(cc.Certificate)
	if err != nil {
		return nil, err
	}
	signer, err := parseKey(cc.PrivateKey)
	if err != nil {
		return nil, err
	}
	pub, ok := chain[0].PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(signer.Public()) {
		return nil, certErr(fieldPrivateKey, "the private key does not belong to the certificate")
	}
	ders := make([][]byte, len(chain))
	for i, c := range chain {
		ders[i] = c.Raw
	}
	return &tls.Certificate{Certificate: ders, PrivateKey: signer, Leaf: chain[0]}, nil
}

// validateClientCertificate reports the problems of cc through add. With a
// masked key (PUT keeps the stored key) only the certificate is checked here;
// the API validates again on the resolved configuration (ADR 0042, section 3).
// An expired certificate is accepted, so a monitor stays editable; the check
// reports it.
func validateClientCertificate(cc *ClientCertificate, add func(field, msg string)) {
	var err error
	if cc.PrivateKey == maskedValue {
		_, err = parseChain(cc.Certificate)
	} else {
		_, err = loadClientCertificate(cc)
	}
	var ce *clientCertError
	switch {
	case errors.As(err, &ce):
		add(ce.field, ce.msg)
	case err != nil:
		add("/clientCertificate", "invalid client certificate")
	}
}

// clientCertValidity checks the leaf against now. It returns a DOWN error
// when the certificate is not valid now, and a warning text when it expires
// within warnDays.
func clientCertValidity(leaf *x509.Certificate, now time.Time, warnDays int) (warning string, err error) {
	switch {
	case now.Before(leaf.NotBefore):
		return "", &checker.TLSError{Err: fmt.Errorf("client certificate is not valid before %s", leaf.NotBefore.UTC().Format(time.DateOnly))}
	case now.After(leaf.NotAfter):
		return "", &checker.TLSError{Err: fmt.Errorf("client certificate expired on %s", leaf.NotAfter.UTC().Format(time.DateOnly))}
	}
	left := leaf.NotAfter.Sub(now).Hours() / 24
	if warnDays > 0 && left < float64(warnDays) {
		return fmt.Sprintf("client certificate expires in %.0f days (%s)", left, leaf.NotAfter.UTC().Format(time.DateOnly)), nil
	}
	return "", nil
}

// clientCertSession tracks, for one check, whether a server asked for a
// client certificate and whether it got one. Redirects run handshakes one
// after another; the mutex guards against transport internals anyway.
type clientCertSession struct {
	cert *tls.Certificate // nil: none configured
	host string           // host of the monitor URL; the only host that gets cert

	mu        sync.Mutex
	requested string // host that asked last, empty if none
	sent      bool   // the last request was answered with cert
	delivered bool   // cert was presented in any handshake of this check
}

// hook returns the GetClientCertificate callback for a handshake with host.
// It answers only when the server asks. The CAs the server names are
// ignored, so a certificate from another chain is not silently withheld.
func (s *clientCertSession) hook(host string) func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.requested = host
		if s.cert != nil && sameHost(host, s.host) {
			s.sent, s.delivered = true, true
			return s.cert, nil
		}
		s.sent = false
		return &tls.Certificate{}, nil // send no certificate
	}
}

// sameHost compares host names as the URL and the dialer write them.
func sameHost(a, b string) bool {
	return strings.EqualFold(strings.TrimSuffix(a, "."), strings.TrimSuffix(b, "."))
}

// explain turns a failed request into a clearer error when a client
// certificate was involved (ADR 0042, section 7). Other errors stay as they
// are. Only the TLS alert text of the server is used, never key material.
func (s *clientCertSession) explain(err error) error {
	s.mu.Lock()
	requested, sent := s.requested, s.sent
	s.mu.Unlock()
	if err == nil || requested == "" {
		return err
	}
	alert := remoteAlert(err)
	switch {
	case sent && alert != "":
		return &checker.TLSError{Err: fmt.Errorf("client certificate rejected by server: %s", alert)}
	case sent:
		return err
	case s.cert != nil:
		return &checker.TLSError{Err: fmt.Errorf("server %s requested a client certificate; it is sent only to %s (%w)", requested, s.host, err)}
	default:
		return &checker.TLSError{Err: fmt.Errorf("server requested a client certificate, but none is configured (%w)", err)}
	}
}

// remoteAlert returns the text of a TLS alert sent by the server, e.g.
// "certificate required", or "" if err is none.
func remoteAlert(err error) string {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "remote error" && opErr.Err != nil {
		return strings.TrimPrefix(opErr.Err.Error(), "tls: ")
	}
	var alert tls.AlertError
	if errors.As(err, &alert) {
		return strings.TrimPrefix(alert.Error(), "tls: ")
	}
	return ""
}

// RequiredClusterCapabilities implements checker.ClusterCapabilityRequirer:
// a configuration with clientCertificate needs every voter to understand it.
func (*Checker) RequiredClusterCapabilities(raw json.RawMessage) []string {
	var probe struct {
		ClientCertificate json.RawMessage `json:"clientCertificate"`
	}
	if json.Unmarshal(raw, &probe) != nil || len(probe.ClientCertificate) == 0 || string(probe.ClientCertificate) == "null" {
		return nil
	}
	return []string{ClientCertCapability}
}

// urlHost returns the host name of the monitor URL, or "" if it has none.
func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
