// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package httpcheck

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/checker"
)

// Functional tests for client certificates (ADR 0042). All keys and
// certificates are generated at run time; no private key is committed.

// newSigner generates a key: p256, p384, p521, rsa1024, rsa2048 or ed25519.
func newSigner(t *testing.T, kind string) crypto.Signer {
	t.Helper()
	var (
		k   crypto.Signer
		err error
	)
	switch kind {
	case "p256":
		k, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case "p384":
		k, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	case "p521":
		k, err = ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	case "rsa1024":
		k, err = rsa.GenerateKey(rand.Reader, 1024) //nolint:gosec // G403: a 1024-bit key is the rejected input
	case "rsa2048":
		k, err = rsa.GenerateKey(rand.Reader, 2048)
	case "ed25519":
		_, k, err = ed25519.GenerateKey(rand.Reader)
	default:
		t.Fatalf("unknown key kind %q", kind)
	}
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// certSpec describes a certificate to issue.
type certSpec struct {
	cn                  string
	notBefore, notAfter time.Time
	ekus                []x509.ExtKeyUsage // nil: no extended key usage
	isCA                bool
}

// clientSpec is a valid client certificate for the next 90 days.
func clientSpec(cn string) certSpec {
	now := time.Now()
	return certSpec{
		cn:        cn,
		notBefore: now.Add(-time.Hour),
		notAfter:  now.Add(90 * 24 * time.Hour),
		ekus:      []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
}

// issueCert signs a certificate for subject. A nil issuer makes it
// self-signed with subject's key.
func issueCert(t *testing.T, subject crypto.Signer, issuer *x509.Certificate, issuerKey crypto.Signer, s certSpec) (*x509.Certificate, string) {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: s.cn},
		NotBefore:             s.notBefore,
		NotAfter:              s.notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           s.ekus,
		BasicConstraintsValid: true,
		IsCA:                  s.isCA,
	}
	if s.isCA {
		tmpl.KeyUsage = x509.KeyUsageCertSign
	}
	if issuer == nil {
		issuer, issuerKey = tmpl, subject
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, issuer, subject.Public(), issuerKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// curveOID returns the DER of the named-curve OID, as in "EC PARAMETERS".
func curveOID(t *testing.T, c elliptic.Curve) []byte {
	t.Helper()
	switch c.Params().Name {
	case "P-256":
		return []byte{0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07}
	case "P-384":
		return []byte{0x06, 0x05, 0x2b, 0x81, 0x04, 0x00, 0x22}
	}
	t.Fatalf("no OID for curve %s", c.Params().Name)
	return nil
}

// encodeKey returns the PEM of signer. format is "pkcs8", "pkcs1" or "sec1".
// With params, an EC PARAMETERS block precedes an EC key (openssl ecparam).
func encodeKey(t *testing.T, signer crypto.Signer, format string, params bool) string {
	t.Helper()
	var out []byte
	if ec, ok := signer.(*ecdsa.PrivateKey); ok && params {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "EC PARAMETERS", Bytes: curveOID(t, ec.Curve)})...)
	}
	var block *pem.Block
	switch format {
	case "sec1":
		ec, ok := signer.(*ecdsa.PrivateKey)
		if !ok {
			t.Fatal("SEC 1 needs an EC key")
		}
		der, err := x509.MarshalECPrivateKey(ec)
		if err != nil {
			t.Fatal(err)
		}
		block = &pem.Block{Type: "EC PRIVATE KEY", Bytes: der}
	case "pkcs1":
		rk, ok := signer.(*rsa.PrivateKey)
		if !ok {
			t.Fatal("PKCS#1 needs an RSA key")
		}
		block = &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rk)}
	case "pkcs8":
		der, err := x509.MarshalPKCS8PrivateKey(signer)
		if err != nil {
			t.Fatal(err)
		}
		block = &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	default:
		t.Fatalf("unknown format %q", format)
	}
	return string(append(out, pem.EncodeToMemory(block)...))
}

// pairBuild returns a certificate PEM and a key PEM issued by ca.
type pairBuild func(t *testing.T, ca *testCA) (certPEM, keyPEM string)

// leafPair builds a leaf of kind with its key in format.
func leafPair(kind, format string, params bool, s certSpec) pairBuild {
	return func(t *testing.T, ca *testCA) (string, string) {
		t.Helper()
		signer := newSigner(t, kind)
		_, certPEM := issueCert(t, signer, ca.cert, ca.key, s)
		return certPEM, encodeKey(t, signer, format, params)
	}
}

// chainPair builds a leaf and an intermediate; the chain is leaf first.
func chainPair(kind string, s certSpec) pairBuild {
	return func(t *testing.T, ca *testCA) (string, string) {
		t.Helper()
		interKey := newSigner(t, "p256")
		inter, interPEM := issueCert(t, interKey, ca.cert, ca.key, certSpec{
			cn: "intermediate", notBefore: s.notBefore, notAfter: s.notAfter, isCA: true,
		})
		signer := newSigner(t, kind)
		_, leafPEM := issueCert(t, signer, inter, interKey, s)
		return leafPEM + interPEM, encodeKey(t, signer, "pkcs8", false)
	}
}

// customPair builds a certificate of kind and a key given by key.
func customPair(kind string, key func(t *testing.T, signer crypto.Signer) string) pairBuild {
	return func(t *testing.T, ca *testCA) (string, string) {
		t.Helper()
		signer := newSigner(t, kind)
		_, certPEM := issueCert(t, signer, ca.cert, ca.key, clientSpec("custom"))
		return certPEM, key(t, signer)
	}
}

// validateClient runs validate on a monitor with the given client certificate.
func validateClient(t *testing.T, cc map[string]any) error {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"url": "https://example.com", "clientCertificate": cc})
	if err != nil {
		t.Fatal(err)
	}
	return validate(raw)
}

// expectValidationField fails unless err is a validation error on field.
func expectValidationField(t *testing.T, err error, field string) {
	t.Helper()
	var ve *checker.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want a validation error on %s", err, field)
	}
	for _, f := range ve.Fields {
		if f.Field == field {
			return
		}
	}
	t.Fatalf("fields = %+v, want %s", ve.Fields, field)
}

func TestClientCertValidateAccepts(t *testing.T) {
	ca := newCA(t)
	tests := []struct {
		name  string
		build pairBuild
	}{
		{"ECDSA P-256 PKCS#8", leafPair("p256", "pkcs8", false, clientSpec("a"))},
		{"ECDSA P-256 SEC 1", leafPair("p256", "sec1", false, clientSpec("a"))},
		{"ECDSA P-256 SEC 1 after EC PARAMETERS", leafPair("p256", "sec1", true, clientSpec("a"))},
		{"ECDSA P-384 PKCS#8", leafPair("p384", "pkcs8", false, clientSpec("a"))},
		{"ECDSA P-384 SEC 1 after EC PARAMETERS", leafPair("p384", "sec1", true, clientSpec("a"))},
		{"RSA 2048 PKCS#1", leafPair("rsa2048", "pkcs1", false, clientSpec("a"))},
		{"RSA 2048 PKCS#8", leafPair("rsa2048", "pkcs8", false, clientSpec("a"))},
		{"Ed25519 PKCS#8", leafPair("ed25519", "pkcs8", false, clientSpec("a"))},
		{"leaf and intermediate", chainPair("p256", clientSpec("leaf"))},
		{"leaf without extended key usage", leafPair("p256", "pkcs8", false, certSpec{
			cn: "no eku", notBefore: time.Now().Add(-time.Hour), notAfter: time.Now().Add(time.Hour),
		})},
		{"already expired leaf", leafPair("p256", "pkcs8", false, certSpec{
			cn: "expired", notBefore: time.Now().Add(-48 * time.Hour), notAfter: time.Now().Add(-time.Hour),
			ekus: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			certPEM, keyPEM := tc.build(t, ca)
			if err := validateClient(t, map[string]any{"certificate": certPEM, "privateKey": keyPEM}); err != nil {
				t.Fatalf("validate: %v", err)
			}
		})
	}
}

func TestClientCertValidateRejects(t *testing.T) {
	ca := newCA(t)
	otherSigner := newSigner(t, "p256")
	validKey := func(t *testing.T, signer crypto.Signer) string { return encodeKey(t, signer, "pkcs8", false) }
	serverOnly := clientSpec("server only")
	serverOnly.ekus = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}

	tests := []struct {
		name  string
		build pairBuild
		field string
	}{
		{"RSA 1024", leafPair("rsa1024", "pkcs8", false, clientSpec("a")), fieldPrivateKey},
		{"ECDSA P-521", leafPair("p521", "pkcs8", false, clientSpec("a")), fieldPrivateKey},
		{"encrypted PKCS#8 (ENCRYPTED PRIVATE KEY)", customPair("p256", func(_ *testing.T, _ crypto.Signer) string {
			return string(pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte("ciphertext")}))
		}), fieldPrivateKey},
		{"encrypted SEC 1 (legacy Proc-Type header)", customPair("p256", func(t *testing.T, signer crypto.Signer) string {
			ec := signer.(*ecdsa.PrivateKey)
			der, err := x509.MarshalECPrivateKey(ec)
			if err != nil {
				t.Fatal(err)
			}
			return string(pem.EncodeToMemory(&pem.Block{
				Type:    "EC PRIVATE KEY",
				Headers: map[string]string{"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-256-CBC,0102030405060708090A0B0C0D0E0F10"},
				Bytes:   der,
			}))
		}), fieldPrivateKey},
		{"two keys", customPair("p256", func(t *testing.T, signer crypto.Signer) string {
			return validKey(t, signer) + validKey(t, newSigner(t, "p256"))
		}), fieldPrivateKey},
		{"key of another certificate", customPair("p256", func(t *testing.T, _ crypto.Signer) string {
			return validKey(t, otherSigner)
		}), fieldPrivateKey},
		{"certificate with serverAuth only", leafPair("p256", "pkcs8", false, serverOnly), fieldCertificate},
		{"non-CERTIFICATE block in certificate", func(t *testing.T, ca *testCA) (string, string) {
			_, keyPEM := leafPair("p256", "pkcs8", false, clientSpec("a"))(t, ca)
			signer := newSigner(t, "p256")
			pub, err := x509.MarshalPKIXPublicKey(signer.Public())
			if err != nil {
				t.Fatal(err)
			}
			return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub})), keyPEM
		}, fieldCertificate},
		{"private key pasted into certificate", func(t *testing.T, ca *testCA) (string, string) {
			signer := newSigner(t, "p256")
			_, certPEM := issueCert(t, signer, ca.cert, ca.key, clientSpec("a"))
			return encodeKey(t, signer, "pkcs8", false), certPEM
		}, fieldCertificate},
		{"empty certificate", func(t *testing.T, ca *testCA) (string, string) {
			_, keyPEM := leafPair("p256", "pkcs8", false, clientSpec("a"))(t, ca)
			return "", keyPEM
		}, fieldCertificate},
		{"garbage certificate", func(t *testing.T, ca *testCA) (string, string) {
			_, keyPEM := leafPair("p256", "pkcs8", false, clientSpec("a"))(t, ca)
			return "not a certificate", keyPEM
		}, fieldCertificate},
		{"more than 8 certificates", func(t *testing.T, ca *testCA) (string, string) {
			certPEM, keyPEM := leafPair("p256", "pkcs8", false, clientSpec("a"))(t, ca)
			return strings.Repeat(certPEM, 9), keyPEM
		}, fieldCertificate},
		{"empty key", func(t *testing.T, ca *testCA) (string, string) {
			certPEM, _ := leafPair("p256", "pkcs8", false, clientSpec("a"))(t, ca)
			return certPEM, ""
		}, fieldPrivateKey},
		{"garbage key", func(t *testing.T, ca *testCA) (string, string) {
			certPEM, _ := leafPair("p256", "pkcs8", false, clientSpec("a"))(t, ca)
			return certPEM, "not a key"
		}, fieldPrivateKey},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			certPEM, keyPEM := tc.build(t, ca)
			err := validateClient(t, map[string]any{"certificate": certPEM, "privateKey": keyPEM})
			if err == nil {
				t.Fatal("validate accepted the client certificate")
			}
			expectValidationField(t, err, tc.field)
			if strings.Contains(err.Error(), "PRIVATE KEY-----") {
				t.Errorf("error contains PEM key material: %v", err)
			}
		})
	}
}

// A masked key (PUT keeps the stored key) is not validated here; only the
// certificate is (ADR 0042, section 3).
func TestClientCertMaskedKeyChecksCertificateOnly(t *testing.T) {
	ca := newCA(t)
	certPEM, _ := leafPair("p256", "pkcs8", false, clientSpec("masked"))(t, ca)

	if err := validateClient(t, map[string]any{"certificate": certPEM, "privateKey": "***"}); err != nil {
		t.Fatalf("valid certificate with masked key: %v", err)
	}
	err := validateClient(t, map[string]any{"certificate": "garbage", "privateKey": "***"})
	expectValidationField(t, err, fieldCertificate)
}

// clientIdentity is a client certificate and its PKCS#8 key, both PEM.
type clientIdentity struct {
	certPEM, keyPEM string
}

// newClientIdentity issues a client certificate of kind signed by ca.
func newClientIdentity(t *testing.T, ca *testCA, kind string, s certSpec) clientIdentity {
	t.Helper()
	signer := newSigner(t, kind)
	_, certPEM := issueCert(t, signer, ca.cert, ca.key, s)
	return clientIdentity{certPEM: certPEM, keyPEM: encodeKey(t, signer, "pkcs8", false)}
}

func (id clientIdentity) config() map[string]any {
	return map[string]any{"certificate": id.certPEM, "privateKey": id.keyPEM}
}

// mtlsServer starts a TLS server with cert that handles client certificates
// as auth says. hits counts the requests that reach the handler.
func mtlsServer(t *testing.T, cert tls.Certificate, auth tls.ClientAuthType, clientCAs *x509.CertPool, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		_, _ = w.Write([]byte("secure"))
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   auth,
		ClientCAs:    clientCAs,
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// closedURL returns the URL of a port that refuses connections.
func closedURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	return u
}

func TestClientCertHandshake(t *testing.T) {
	serverCA := newCA(t)
	clientCA := newCA(t)
	otherCA := newCA(t)
	trusted := newClientIdentity(t, clientCA, "p256", clientSpec("trusted client"))
	untrusted := newClientIdentity(t, otherCA, "p256", clientSpec("untrusted client"))
	serverCert := serverCA.leaf(t, time.Now().Add(90*24*time.Hour), "127.0.0.1", "localhost")

	tests := []struct {
		name       string
		auth       tls.ClientAuthType
		id         *clientIdentity
		wantStatus checker.Status
		wantClass  checker.ErrorClass
		msg        string
		wantSent   bool
	}{
		{name: "server does not ask", auth: tls.NoClientCert, id: &trusted,
			wantStatus: checker.StatusUp, wantSent: false},
		{name: "server asks and gets the certificate", auth: tls.RequireAndVerifyClientCert, id: &trusted,
			wantStatus: checker.StatusUp, wantSent: true},
		{name: "server rejects an untrusted certificate", auth: tls.RequireAndVerifyClientCert, id: &untrusted,
			wantStatus: checker.StatusDown, wantClass: checker.ClassTLS, msg: "client certificate rejected by server", wantSent: true},
		{name: "server asks, none configured", auth: tls.RequireAndVerifyClientCert, id: nil,
			wantStatus: checker.StatusDown, wantClass: checker.ClassTLS, msg: "none is configured", wantSent: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := mtlsServer(t, serverCert, tc.auth, clientCA.pool, nil)
			c := &Checker{RootCAs: serverCA.pool}
			cfg := map[string]any{"url": srv.URL}
			if tc.id != nil {
				cfg["clientCertificate"] = tc.id.config()
			}
			res := run(t, c, 5*time.Second, cfg)
			expect(t, res, tc.wantStatus, tc.wantClass)
			if tc.msg != "" && !strings.Contains(res.Message, tc.msg) {
				t.Errorf("message = %q, want it to contain %q", res.Message, tc.msg)
			}
			if tc.id == nil {
				return
			}
			if got := res.Details["clientCertSent"]; got != tc.wantSent {
				t.Errorf("clientCertSent = %v, want %v", got, tc.wantSent)
			}
			if tc.wantSent && tc.wantStatus == checker.StatusUp {
				if res.Details["clientCertSubject"] != "CN=trusted client" {
					t.Errorf("clientCertSubject = %v", res.Details["clientCertSubject"])
				}
				if res.Details["clientCertIssuer"] != clientCA.cert.Subject.String() {
					t.Errorf("clientCertIssuer = %v", res.Details["clientCertIssuer"])
				}
				notAfter, _ := res.Details["clientCertNotAfter"].(string)
				if _, err := time.Parse(time.RFC3339, notAfter); err != nil {
					t.Errorf("clientCertNotAfter = %v: %v", res.Details["clientCertNotAfter"], err)
				}
			}
			assertNoKey(t, tc.id.keyPEM, "check result", resultText(t, res))
		})
	}
}

// The expiry of the client certificate is judged by the node clock (ADR
// 0042, section 6). The clock is fixed through the test field now.
func TestClientCertExpiry(t *testing.T) {
	serverCA := newCA(t)
	clientCA := newCA(t)
	clock := time.Now()
	c := &Checker{RootCAs: serverCA.pool, now: func() time.Time { return clock }}
	serverCert := serverCA.leaf(t, clock.Add(90*24*time.Hour), "127.0.0.1", "localhost")
	srv := mtlsServer(t, serverCert, tls.RequireAndVerifyClientCert, clientCA.pool, nil)

	tests := []struct {
		name       string
		spec       certSpec
		url        string // empty: the mTLS server; otherwise a URL that must not be dialled
		wantStatus checker.Status
		wantClass  checker.ErrorClass
		msg        string
	}{
		{
			name:       "valid for 90 days",
			spec:       certSpec{cn: "c", notBefore: clock.Add(-time.Hour), notAfter: clock.Add(90 * 24 * time.Hour), ekus: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}},
			wantStatus: checker.StatusUp,
		},
		{
			name:       "expires within the warning window",
			spec:       certSpec{cn: "c", notBefore: clock.Add(-time.Hour), notAfter: clock.Add(5 * 24 * time.Hour), ekus: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}},
			wantStatus: checker.StatusDegraded,
			msg:        "client certificate expires in",
		},
		{
			name: "expired, not connected",
			spec: certSpec{cn: "c", notBefore: clock.Add(-48 * time.Hour), notAfter: clock.Add(-time.Hour), ekus: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}},
			url:  "closed", wantStatus: checker.StatusDown, wantClass: checker.ClassTLS, msg: "client certificate expired",
		},
		{
			name: "not yet valid, not connected",
			spec: certSpec{cn: "c", notBefore: clock.Add(time.Hour), notAfter: clock.Add(30 * 24 * time.Hour), ekus: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}},
			url:  "closed", wantStatus: checker.StatusDown, wantClass: checker.ClassTLS, msg: "not valid before",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			signer := newSigner(t, "p256")
			_, certPEM := issueCert(t, signer, clientCA.cert, clientCA.key, tc.spec)
			id := clientIdentity{certPEM: certPEM, keyPEM: encodeKey(t, signer, "pkcs8", false)}
			target := srv.URL
			if tc.url == "closed" {
				target = closedURL(t)
			}
			res := run(t, c, 5*time.Second, map[string]any{"url": target, "clientCertificate": id.config()})
			expect(t, res, tc.wantStatus, tc.wantClass)
			if tc.msg != "" && !strings.Contains(res.Message, tc.msg) {
				t.Errorf("message = %q, want it to contain %q", res.Message, tc.msg)
			}
		})
	}
}

// A client certificate is sent only to the host of the monitor URL (ADR 0042,
// section 5). The redirect target on another host name must not get it.
func TestClientCertRedirectBinding(t *testing.T) {
	serverCA := newCA(t)
	clientCA := newCA(t)
	id := newClientIdentity(t, clientCA, "p256", clientSpec("redirect client"))
	serverCert := serverCA.leaf(t, time.Now().Add(90*24*time.Hour), "127.0.0.1", "localhost")
	c := &Checker{RootCAs: serverCA.pool}

	// target requires a client certificate on 127.0.0.1:<port>.
	target := mtlsServer(t, serverCert, tls.RequireAndVerifyClientCert, clientCA.pool, nil)
	sameHostURL := target.URL
	otherHostURL := strings.Replace(target.URL, "127.0.0.1", "localhost", 1)

	// monitor lives on another port of 127.0.0.1 and redirects to the target.
	mux := http.NewServeMux()
	mux.HandleFunc("/other-host", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, otherHostURL, http.StatusFound)
	})
	mux.HandleFunc("/same-host", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sameHostURL, http.StatusFound)
	})
	monitor := httptest.NewUnstartedServer(mux)
	monitor.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12}
	monitor.StartTLS()
	t.Cleanup(monitor.Close)

	t.Run("other host name gets no certificate", func(t *testing.T) {
		res := run(t, c, 5*time.Second, map[string]any{
			"url": monitor.URL + "/other-host", "clientCertificate": id.config(),
		})
		expect(t, res, checker.StatusDown, checker.ClassTLS)
		if !strings.Contains(res.Message, "it is sent only to") {
			t.Errorf("message = %q, want the host binding named", res.Message)
		}
		if res.Details["clientCertSent"] != false {
			t.Errorf("clientCertSent = %v, want false", res.Details["clientCertSent"])
		}
		assertNoKey(t, id.keyPEM, "check result", resultText(t, res))
	})

	t.Run("same host on another port gets the certificate", func(t *testing.T) {
		res := run(t, c, 5*time.Second, map[string]any{
			"url": monitor.URL + "/same-host", "clientCertificate": id.config(),
		})
		expect(t, res, checker.StatusUp, checker.ClassNone)
		if res.Details["clientCertSent"] != true {
			t.Errorf("clientCertSent = %v, want true", res.Details["clientCertSent"])
		}
		if res.Details["redirects"] != 1 {
			t.Errorf("redirects = %v, want 1", res.Details["redirects"])
		}
	})
}

func TestRequiredClusterCapabilities(t *testing.T) {
	tests := map[string]struct {
		raw  string
		want []string
	}{
		"no field":   {`{"url":"https://example.com"}`, nil},
		"null field": {`{"url":"https://example.com","clientCertificate":null}`, nil},
		"with field": {`{"url":"https://example.com","clientCertificate":{"certificate":"x","privateKey":"***"}}`, []string{"http-client-cert"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := (&Checker{}).RequiredClusterCapabilities(json.RawMessage(tc.raw))
			if len(got) != len(tc.want) || (len(got) == 1 && got[0] != tc.want[0]) {
				t.Errorf("capabilities = %v, want %v", got, tc.want)
			}
		})
	}
	if ClientCertCapability != "http-client-cert" {
		t.Errorf("ClientCertCapability = %q", ClientCertCapability)
	}
}
