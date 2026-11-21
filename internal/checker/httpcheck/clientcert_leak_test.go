// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package httpcheck

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/checker"
)

// leakIdentity is a CA-signed client certificate for the leak tests.
type leakIdentity struct {
	certPEM, keyPEM string
	pool            *x509.CertPool // the issuing CA
}

func newLeakIdentity(t *testing.T) leakIdentity {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "leak test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "leak test client"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return leakIdentity{
		certPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		keyPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
		pool:    pool,
	}
}

// assertNoKey fails if any 12-character piece of the key's base64 body
// appears in text (ADR 0042, section 8).
func assertNoKey(t *testing.T, keyPEM, what, text string) {
	t.Helper()
	block, _ := pem.Decode([]byte(keyPEM))
	body := strings.ReplaceAll(string(pem.EncodeToMemory(&pem.Block{Type: "X", Bytes: block.Bytes})), "\n", "")
	body = strings.TrimSuffix(strings.TrimPrefix(body, "-----BEGIN X-----"), "-----END X-----")
	for i := 0; i+12 <= len(body); i += 4 {
		if strings.Contains(text, body[i:i+12]) {
			t.Fatalf("%s contains private key material: %q", what, text)
		}
	}
	if strings.Contains(text, "PRIVATE KEY-----") {
		t.Fatalf("%s contains a PEM key header: %q", what, text)
	}
}

func resultText(t *testing.T, res checker.Result) string {
	t.Helper()
	details, err := json.Marshal(res.Details)
	if err != nil {
		t.Fatal(err)
	}
	return res.Message + " " + string(details)
}

func TestClientCertKeyNeverLeaks(t *testing.T) {
	trusted := newLeakIdentity(t)
	other := newLeakIdentity(t)

	t.Run("validation errors", func(t *testing.T) {
		cases := map[string]ClientCertificate{
			"key of another certificate": {Certificate: trusted.certPEM, PrivateKey: other.keyPEM},
			"trailing garbage":           {Certificate: trusted.certPEM, PrivateKey: trusted.keyPEM + "garbage"},
			"key in certificate field":   {Certificate: trusted.keyPEM, PrivateKey: trusted.keyPEM},
			"truncated key":              {Certificate: trusted.certPEM, PrivateKey: trusted.keyPEM[:len(trusted.keyPEM)/2]},
			"key as raw base64":          {Certificate: trusted.certPEM, PrivateKey: strings.Join(strings.Split(trusted.keyPEM, "\n")[1:3], "")},
		}
		for name, cc := range cases {
			t.Run(name, func(t *testing.T) {
				raw, err := json.Marshal(map[string]any{"url": "https://example.com", "clientCertificate": cc})
				if err != nil {
					t.Fatal(err)
				}
				err = validate(raw)
				if err == nil {
					t.Fatal("validate accepted an invalid client certificate")
				}
				assertNoKey(t, trusted.keyPEM, "validation error", err.Error())
				assertNoKey(t, other.keyPEM, "validation error", err.Error())
				res := (&Checker{}).Check(t.Context(), env, raw)
				expect(t, res, checker.StatusDown, checker.ClassInternal)
				assertNoKey(t, trusted.keyPEM, "check result", resultText(t, res))
				assertNoKey(t, other.keyPEM, "check result", resultText(t, res))
			})
		}
	})

	t.Run("handshake", func(t *testing.T) {
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		srv.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: trusted.pool, MinVersion: tls.VersionTLS12}
		srv.StartTLS()
		t.Cleanup(srv.Close)
		roots := x509.NewCertPool()
		roots.AddCert(srv.Certificate())
		c := &Checker{RootCAs: roots}

		ok := run(t, c, 5*time.Second, map[string]any{"url": srv.URL, "clientCertificate": map[string]any{
			"certificate": trusted.certPEM, "privateKey": trusted.keyPEM,
		}})
		expect(t, ok, checker.StatusUp, "")
		assertNoKey(t, trusted.keyPEM, "check result", resultText(t, ok))

		rejected := run(t, c, 5*time.Second, map[string]any{"url": srv.URL, "clientCertificate": map[string]any{
			"certificate": other.certPEM, "privateKey": other.keyPEM,
		}})
		expect(t, rejected, checker.StatusDown, checker.ClassTLS)
		if !strings.Contains(rejected.Message, "client certificate rejected by server") {
			t.Fatalf("message = %q, want the rejection explained", rejected.Message)
		}
		assertNoKey(t, other.keyPEM, "check result", resultText(t, rejected))

		none := run(t, c, 5*time.Second, map[string]any{"url": srv.URL})
		expect(t, none, checker.StatusDown, checker.ClassTLS)
		if !strings.Contains(none.Message, "none is configured") {
			t.Fatalf("message = %q, want the missing certificate named", none.Message)
		}
	})
}
