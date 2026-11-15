// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package httpcheck

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/joinsider/uptimecluster/internal/checker"
)

// TestMain fails if any check leaves goroutines behind (FA-CHK-02, PF-16).
func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

var env = &checker.Env{Dialer: &checker.Dialer{}, UserAgent: "UptimeCluster/test"}

func run(t *testing.T, c *Checker, timeout time.Duration, cfg map[string]any) checker.Result {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.Check(ctx, env, raw)
}

func expect(t *testing.T, res checker.Result, status checker.Status, class checker.ErrorClass) {
	t.Helper()
	if res.Status != status || res.ErrorClass != class {
		t.Fatalf("result = %s/%q (%s), want %s/%q", res.Status, res.ErrorClass, res.Message, status, class)
	}
}

func newServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestStatusAndDetails(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fail":
			w.WriteHeader(http.StatusInternalServerError)
		case "/moved":
			http.Redirect(w, r, "/", http.StatusMovedPermanently)
		default:
			_, _ = w.Write([]byte("ok"))
		}
	})
	c := &Checker{}

	res := run(t, c, 5*time.Second, map[string]any{"url": srv.URL})
	expect(t, res, checker.StatusUp, checker.ClassNone)
	if res.Details["statusCode"] != 200 || res.Details["resolvedIp"] != "127.0.0.1" {
		t.Errorf("details = %v", res.Details)
	}
	if _, ok := res.Details["timings"]; !ok {
		t.Error("phase timings missing")
	}

	expect(t, run(t, c, 5*time.Second, map[string]any{"url": srv.URL + "/fail"}), checker.StatusDown, checker.ClassHTTPStatus)
	expect(t, run(t, c, 5*time.Second, map[string]any{"url": srv.URL + "/fail", "expectedStatus": "500"}), checker.StatusUp, checker.ClassNone)

	followed := run(t, c, 5*time.Second, map[string]any{"url": srv.URL + "/moved"})
	expect(t, followed, checker.StatusUp, checker.ClassNone)
	if followed.Details["redirects"] != 1 {
		t.Errorf("redirects = %v, want 1", followed.Details["redirects"])
	}
	expect(t, run(t, c, 5*time.Second, map[string]any{"url": srv.URL + "/moved", "followRedirects": false}),
		checker.StatusDown, checker.ClassHTTPStatus)
	expect(t, run(t, c, 5*time.Second, map[string]any{"url": srv.URL + "/moved", "followRedirects": false, "expectedStatus": "200-299,301"}),
		checker.StatusUp, checker.ClassNone)
}

func TestRedirectLimit(t *testing.T) {
	// Each response redirects to a longer path, so the chain never ends.
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path+"x", http.StatusFound) //nolint:gosec // G710: test server redirecting to itself
	})
	res := run(t, &Checker{}, 5*time.Second, map[string]any{"url": srv.URL + "/", "maxRedirects": 3})
	expect(t, res, checker.StatusDown, checker.ClassAssertion)
}

func TestKeywordAndJSONPath(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json" {
			_, _ = w.Write([]byte(`{"status":"green","items":[{"n":1.5},{"ok":true}],"meta":null}`))
			return
		}
		_, _ = w.Write([]byte("Service is Healthy"))
	})
	c := &Checker{}
	tests := []struct {
		name  string
		cfg   map[string]any
		want  checker.Status
		class checker.ErrorClass
	}{
		{"contains", map[string]any{"keyword": map[string]any{"value": "healthy"}}, checker.StatusUp, ""},
		{"case sensitive miss", map[string]any{"keyword": map[string]any{"value": "healthy", "caseSensitive": true}}, checker.StatusDown, checker.ClassAssertion},
		{"missing", map[string]any{"keyword": map[string]any{"value": "degraded"}}, checker.StatusDown, checker.ClassAssertion},
		{"absent ok", map[string]any{"keyword": map[string]any{"value": "error", "mode": "notContains"}}, checker.StatusUp, ""},
		{"absent violated", map[string]any{"keyword": map[string]any{"value": "service", "mode": "notContains"}}, checker.StatusDown, checker.ClassAssertion},
		{"json string", map[string]any{"path": "/json", "jsonPath": map[string]any{"path": "$.status", "expected": "green"}}, checker.StatusUp, ""},
		{"json number", map[string]any{"path": "/json", "jsonPath": map[string]any{"path": "$.items[0].n", "expected": "1.5"}}, checker.StatusUp, ""},
		{"json bool", map[string]any{"path": "/json", "jsonPath": map[string]any{"path": "items[1].ok", "expected": "true"}}, checker.StatusUp, ""},
		{"json null", map[string]any{"path": "/json", "jsonPath": map[string]any{"path": "$.meta", "expected": "null"}}, checker.StatusUp, ""},
		{"json mismatch", map[string]any{"path": "/json", "jsonPath": map[string]any{"path": "$.status", "expected": "red"}}, checker.StatusDown, checker.ClassAssertion},
		{"json missing key", map[string]any{"path": "/json", "jsonPath": map[string]any{"path": "$.nope", "expected": "x"}}, checker.StatusDown, checker.ClassAssertion},
		{"not json", map[string]any{"jsonPath": map[string]any{"path": "$.status", "expected": "green"}}, checker.StatusDown, checker.ClassAssertion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, _ := tt.cfg["path"].(string)
			delete(tt.cfg, "path")
			tt.cfg["url"] = srv.URL + path
			expect(t, run(t, c, 5*time.Second, tt.cfg), tt.want, tt.class)
		})
	}
}

func TestRequestShape(t *testing.T) {
	var got *http.Request
	var body string
	srv := newServer(t, func(_ http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		b := new(strings.Builder)
		buf := make([]byte, 512)
		n, _ := r.Body.Read(buf)
		b.Write(buf[:n])
		body = b.String()
	})
	c := &Checker{}

	expect(t, run(t, c, 5*time.Second, map[string]any{
		"url": srv.URL, "method": "POST", "body": `{"ping":1}`,
		"headers": map[string]any{"X-Probe": "1", "Host": "example.org"},
		"auth":    map[string]any{"type": "basic", "username": "u", "password": "p"},
	}), checker.StatusUp, "")
	if got.Method != http.MethodPost || body != `{"ping":1}` || got.Header.Get("X-Probe") != "1" || got.Host != "example.org" {
		t.Errorf("request = %s %s host=%s body=%q", got.Method, got.URL, got.Host, body)
	}
	if u, p, ok := got.BasicAuth(); !ok || u != "u" || p != "p" {
		t.Error("basic auth missing")
	}
	if ua := got.Header.Get("User-Agent"); ua != "UptimeCluster/test" {
		t.Errorf("User-Agent = %q", ua)
	}

	run(t, c, 5*time.Second, map[string]any{"url": srv.URL, "auth": map[string]any{"type": "bearer", "token": "t0k"}, "userAgent": "custom/1"})
	if got.Header.Get("Authorization") != "Bearer t0k" || got.Header.Get("User-Agent") != "custom/1" {
		t.Errorf("headers = %v", got.Header)
	}
}

func TestTimeouts(t *testing.T) {
	release := make(chan struct{})
	srv := newServer(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)
	c := &Checker{}
	expect(t, run(t, c, 300*time.Millisecond, map[string]any{"url": srv.URL}), checker.StatusDown, checker.ClassTimeout)
	expect(t, run(t, c, 5*time.Second, map[string]any{"url": srv.URL, "responseHeaderTimeoutMs": 100}), checker.StatusDown, checker.ClassTimeout)

	fast := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte("ok"))
	})
	expect(t, run(t, c, 5*time.Second, map[string]any{"url": fast.URL, "maxResponseMs": 10}), checker.StatusDown, checker.ClassAssertion)
}

func TestBodyLimit(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", 2<<20) + "NEEDLE"))
	})
	res := run(t, &Checker{}, 5*time.Second, map[string]any{"url": srv.URL, "keyword": map[string]any{"value": "NEEDLE"}})
	expect(t, res, checker.StatusDown, checker.ClassAssertion)
	if res.Details["bodyTruncated"] != true {
		t.Error("bodyTruncated not reported")
	}
}

func TestIgnoresProxyEnvironment(t *testing.T) {
	srv := newServer(t, func(http.ResponseWriter, *http.Request) {})
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	expect(t, run(t, &Checker{}, 5*time.Second, map[string]any{"url": srv.URL}), checker.StatusUp, "")
}

// TestFreshConnectionPerCheck covers FA-CHK-11 / PF-13.
func TestFreshConnectionPerCheck(t *testing.T) {
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	for range 3 {
		expect(t, run(t, &Checker{}, 5*time.Second, map[string]any{"url": srv.URL}), checker.StatusUp, "")
	}
	if n := conns.Load(); n != 3 {
		t.Errorf("server saw %d connections for 3 checks, want 3", n)
	}

	addr := srv.Listener.Addr().String()
	srv.Close() // new connections are refused now
	expect(t, run(t, &Checker{}, 5*time.Second, map[string]any{"url": "http://" + addr}), checker.StatusDown, checker.ClassConnect)
}

func TestIPFamily(t *testing.T) {
	srv := newServer(t, func(http.ResponseWriter, *http.Request) {})
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	url := "http://localhost:" + port
	expect(t, run(t, &Checker{}, 5*time.Second, map[string]any{"url": url, "ipFamily": "ipv4"}), checker.StatusUp, "")
	res := run(t, &Checker{}, 5*time.Second, map[string]any{"url": url, "ipFamily": "ipv6"})
	if res.Status != checker.StatusDown {
		t.Errorf("ipv6 to an IPv4-only server = %s, want DOWN", res.Status)
	}
}

func TestSSRFBlocked(t *testing.T) {
	srv := newServer(t, func(http.ResponseWriter, *http.Request) {})
	guarded := &checker.Env{Dialer: &checker.Dialer{Policy: checker.BlockPrivate{}}, UserAgent: "x"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, _ := json.Marshal(map[string]any{"url": srv.URL})
	expect(t, (&Checker{}).Check(ctx, guarded, raw), checker.StatusDown, checker.ClassSSRFBlocked)
}

// --- TLS ----------------------------------------------------------------------

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newCA(t *testing.T) *testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert: cert, key: key, pool: pool}
}

func (ca *testCA) leaf(t *testing.T, notAfter time.Time, hosts ...string) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "leaf"},
		NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func tlsServer(t *testing.T, cert tls.Certificate) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("secure")) }))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// ADR 0020: TLS expiry is judged by the node clock (checker.Env.Now). A node
// whose clock is 88 days ahead sees a 90-day certificate with 2 days left.
func TestTLSExpiryUsesNodeClock(t *testing.T) {
	ca := newCA(t)
	c := &Checker{RootCAs: ca.pool}
	srv := tlsServer(t, ca.leaf(t, time.Now().Add(90*24*time.Hour), "127.0.0.1"))
	raw, err := json.Marshal(map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	ahead := &checker.Env{Dialer: env.Dialer, Now: func() time.Time { return time.Now().Add(88 * 24 * time.Hour) }}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := c.Check(ctx, ahead, raw)
	expect(t, res, checker.StatusDegraded, "")
	if !strings.Contains(res.Message, "expires in") {
		t.Errorf("message = %q", res.Message)
	}
	expect(t, run(t, c, 5*time.Second, map[string]any{"url": srv.URL}), checker.StatusUp, "")
}

func TestTLS(t *testing.T) {
	ca := newCA(t)
	c := &Checker{RootCAs: ca.pool}
	now := time.Now()

	valid := tlsServer(t, ca.leaf(t, now.Add(90*24*time.Hour), "127.0.0.1"))
	res := run(t, c, 5*time.Second, map[string]any{"url": valid.URL})
	expect(t, res, checker.StatusUp, "")
	if res.Details["tlsExpiresAt"] == nil || res.Details["tlsVersion"] == nil {
		t.Errorf("TLS details missing: %v", res.Details)
	}

	expiring := tlsServer(t, ca.leaf(t, now.Add(5*24*time.Hour), "127.0.0.1"))
	res = run(t, c, 5*time.Second, map[string]any{"url": expiring.URL})
	expect(t, res, checker.StatusDegraded, "")
	if !strings.Contains(res.Message, "expires in") {
		t.Errorf("message = %q", res.Message)
	}
	expect(t, run(t, c, 5*time.Second, map[string]any{"url": expiring.URL, "tlsExpiryWarningDays": 0}), checker.StatusUp, "")

	expired := tlsServer(t, ca.leaf(t, now.Add(-time.Hour), "127.0.0.1"))
	expect(t, run(t, c, 5*time.Second, map[string]any{"url": expired.URL}), checker.StatusDown, checker.ClassTLS)

	wrongHost := tlsServer(t, ca.leaf(t, now.Add(90*24*time.Hour), "other.example"))
	expect(t, run(t, c, 5*time.Second, map[string]any{"url": wrongHost.URL}), checker.StatusDown, checker.ClassTLS)

	// Unknown CA: DOWN, unless errors are ignored; then the chain problem is
	// still reported and the expiry still evaluated (FA-CHK-14).
	untrusted := &Checker{}
	expect(t, run(t, untrusted, 5*time.Second, map[string]any{"url": valid.URL}), checker.StatusDown, checker.ClassTLS)
	res = run(t, untrusted, 5*time.Second, map[string]any{"url": valid.URL, "ignoreTlsErrors": true})
	expect(t, res, checker.StatusUp, "")
	if res.Details["tlsError"] == nil || res.Details["tlsExpiresAt"] == nil {
		t.Errorf("ignored TLS error not reported: %v", res.Details)
	}
	res = run(t, untrusted, 5*time.Second, map[string]any{"url": expiring.URL, "ignoreTlsErrors": true})
	expect(t, res, checker.StatusDegraded, "")
}

// --- Validation ------------------------------------------------------------------

func TestValidation(t *testing.T) {
	reg := checker.NewRegistry()
	reg.Register(&Checker{})
	valid := []string{
		`{"url":"https://example.com"}`,
		`{"url":"http://10.0.0.1:8080/health","method":"POST","body":"x","expectedStatus":"200-299,301","keyword":{"value":"ok"},"jsonPath":{"path":"$.a[0]","expected":"1"}}`,
		`{"url":"https://[2001:db8::1]/","auth":{"type":"bearer","token":"t"},"ipFamily":"ipv6"}`,
	}
	for _, cfg := range valid {
		if err := reg.Validate(Type, json.RawMessage(cfg)); err != nil {
			t.Errorf("%s: %v", cfg, err)
		}
	}
	invalid := map[string]string{
		`{}`:                                                   "/url",
		`{"url":"ftp://example.com"}`:                          "/url",
		`{"url":"https://user:pw@example.com"}`:                "/url",
		`{"url":"http://2852039166/"}`:                         "/url",
		`{"url":"http://0177.0.0.1/"}`:                         "/url",
		`{"url":"http://0x7f000001/"}`:                         "/url",
		`{"url":"https://example.com","extra":1}`:              "",
		`{"url":"https://example.com","expectedStatus":"700"}`: "/expectedStatus",
		`{"url":"https://example.com","jsonPath":{"path":"$.a[","expected":"x"}}`: "/jsonPath/path",
		`{"url":"https://example.com","auth":{"type":"basic"}}`:                   "/auth/username",
		`{"url":"https://example.com","method":"GET","body":"x"}`:                 "/body",
		`{"url":"https://example.com","method":"HEAD","keyword":{"value":"x"}}`:   "/method",
		`{"url":"https://example.com","headers":{"Bad Header":"x"}}`:              "/headers/Bad Header",
		`{"url":"https://example.com","maxRedirects":100}`:                        "/maxRedirects",
	}
	for cfg, field := range invalid {
		err := reg.Validate(Type, json.RawMessage(cfg))
		var verr *checker.ValidationError
		if !errors.As(err, &verr) {
			t.Errorf("%s: error = %v, want ValidationError", cfg, err)
			continue
		}
		if field != "" && !hasField(verr, field) {
			t.Errorf("%s: fields = %+v, want %s", cfg, verr.Fields, field)
		}
	}
}

func hasField(err *checker.ValidationError, field string) bool {
	for _, f := range err.Fields {
		if f.Field == field {
			return true
		}
	}
	return false
}

func TestDefaultRegistry(t *testing.T) {
	c, err := checker.Default.Lookup(Type)
	if err != nil || c.Name() != "HTTP(S)" {
		t.Fatalf("Lookup = %v, %v", c, err)
	}
	if _, err := checker.Default.Lookup("nope"); !errors.Is(err, checker.ErrUnknownType) {
		t.Errorf("unknown type error = %v", err)
	}
}
