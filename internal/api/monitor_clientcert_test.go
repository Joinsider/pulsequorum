// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/api/gen"
	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/configsecret"
	"github.com/joinsider/pulsequorum/internal/store"
)

// clientCapability is the node capability of binaries that understand
// clientCertificate (ADR 0042, section 10).
const clientCapability = "http-client-cert"

// clientIdentityPEM returns a fresh client certificate and its PKCS#8 key.
func clientIdentityPEM(t *testing.T) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "api test client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}

// keyBodyLine is the first line of the base64 body of a PEM key. It is
// contiguous in JSON, so it is a usable needle for leak checks.
func keyBodyLine(keyPEM string) string {
	return strings.Split(keyPEM, "\n")[1]
}

func clientCertMonitor(name, certPEM, keyPEM string) map[string]any {
	return map[string]any{"name": name, "type": "http", "intervalSeconds": 60, "config": map[string]any{
		"url":               "https://example.com/",
		"clientCertificate": map[string]any{"certificate": certPEM, "privateKey": keyPEM},
	}}
}

func registerClusterNode(t *testing.T, e *env, id, role string, caps ...string) {
	t.Helper()
	cmd := cluster.NewRegisterNode("register-"+id, store.Node{ID: id, Name: id, Role: role, Capabilities: caps})
	if _, err := e.node.Apply(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
}

// ADR 0042, section 1 and 8: the key is stored encrypted and shown masked;
// the certificate is shown in clear.
func TestClientCertCreateMasksKey(t *testing.T) {
	e := newEnv(t)
	certPEM, keyPEM := clientIdentityPEM(t)

	res := e.do("POST", "/api/v1/monitors", editor, clientCertMonitor("mtls", certPEM, keyPEM))
	if res.code != http.StatusCreated {
		t.Fatalf("create = %d %s", res.code, res.body)
	}
	assertNoPlaintext(t, "create response", res.body, keyBodyLine(keyPEM))

	m := decode[gen.Monitor](t, res)
	cc, _ := m.Config["clientCertificate"].(map[string]any)
	if cc == nil || cc["privateKey"] != configsecret.Mask || cc["certificate"] != certPEM {
		t.Errorf("clientCertificate = %v, want the certificate in clear and the key masked", m.Config["clientCertificate"])
	}
}

// ADR 0042, section 10 as amended: while a voter lacks the capability, the
// API refuses the configuration and names the voter.
func TestClientCertCapabilityGate(t *testing.T) {
	certPEM, keyPEM := clientIdentityPEM(t)

	t.Run("voter without the capability blocks", func(t *testing.T) {
		e := newEnv(t)
		registerClusterNode(t, e, "voter-new", store.NodeRoleVoter, clientCapability)
		registerClusterNode(t, e, "voter-old", store.NodeRoleVoter, "icmp")

		res := e.do("POST", "/api/v1/monitors", editor, clientCertMonitor("mtls", certPEM, keyPEM))
		if res.code != http.StatusUnprocessableEntity {
			t.Fatalf("create = %d %s, want 422", res.code, res.body)
		}
		p := decode[gen.Problem](t, res)
		if p.Errors == nil || len(*p.Errors) == 0 {
			t.Fatalf("no field errors: %s", res.body)
		}
		field := (*p.Errors)[0]
		if field.Field != "/config" || !strings.Contains(field.Message, "voter-old") || strings.Contains(field.Message, "voter-new") {
			t.Errorf("error = %+v, want /config naming voter-old only", field)
		}
		assertNoPlaintext(t, "gate response", res.body, keyBodyLine(keyPEM))
	})

	t.Run("probes without the capability do not block", func(t *testing.T) {
		e := newEnv(t)
		registerClusterNode(t, e, "voter-new", store.NodeRoleVoter, clientCapability)
		registerClusterNode(t, e, "probe-old", store.NodeRoleProbe)

		if res := e.do("POST", "/api/v1/monitors", editor, clientCertMonitor("mtls", certPEM, keyPEM)); res.code != http.StatusCreated {
			t.Fatalf("create = %d %s, want 201", res.code, res.body)
		}
	})

	t.Run("all voters with the capability accept it", func(t *testing.T) {
		e := newEnv(t)
		registerClusterNode(t, e, "voter-a", store.NodeRoleVoter, clientCapability, "icmp")
		registerClusterNode(t, e, "voter-b", store.NodeRoleVoter, clientCapability)

		if res := e.do("POST", "/api/v1/monitors", editor, clientCertMonitor("mtls", certPEM, keyPEM)); res.code != http.StatusCreated {
			t.Fatalf("create = %d %s, want 201", res.code, res.body)
		}
	})

	t.Run("monitors without a client certificate are unaffected", func(t *testing.T) {
		e := newEnv(t)
		registerClusterNode(t, e, "voter-old", store.NodeRoleVoter, "icmp")

		if res := e.do("POST", "/api/v1/monitors", editor, httpMonitor("plain", "https://example.com/")); res.code != http.StatusCreated {
			t.Fatalf("create = %d %s, want 201", res.code, res.body)
		}
	})
}

// ADR 0015 and 0042, section 3: a masked key on update keeps the stored key,
// and the stored key is checked against the new certificate.
func TestClientCertUpdateKeepsOrReplacesKey(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	certPEM, keyPEM := clientIdentityPEM(t)
	m := decode[gen.Monitor](t, e.do("POST", "/api/v1/monitors", editor, clientCertMonitor("mtls", certPEM, keyPEM)))
	path := "/api/v1/monitors/" + m.Id.String()

	// Same certificate, masked key: the stored key is kept.
	if res := e.do("PUT", path, editor, clientCertMonitor("mtls", certPEM, configsecret.Mask)); res.code != http.StatusOK {
		t.Fatalf("update with kept key = %d %s, want 200", res.code, res.body)
	}
	stored, err := e.state.GetMonitor(ctx, "default", m.Id.String())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := configsecret.ResolveMonitor(ctx, e.keyring, stored)
	if err != nil || !strings.Contains(string(cfg), keyBodyLine(keyPEM)) {
		t.Fatalf("stored key no longer resolves to the original key: %v", err)
	}

	// Another certificate with the masked key: the key does not match.
	otherCertPEM, _ := clientIdentityPEM(t)
	res := e.do("PUT", path, editor, clientCertMonitor("mtls", otherCertPEM, configsecret.Mask))
	if res.code != http.StatusUnprocessableEntity {
		t.Fatalf("update with mismatched key = %d %s, want 422", res.code, res.body)
	}
	p := decode[gen.Problem](t, res)
	found := false
	if p.Errors != nil {
		for _, f := range *p.Errors {
			found = found || f.Field == "/config/clientCertificate/privateKey"
		}
	}
	if !found {
		t.Errorf("errors do not name /config/clientCertificate/privateKey: %s", res.body)
	}
	assertNoPlaintext(t, "mismatch response", res.body, keyBodyLine(keyPEM))
}
