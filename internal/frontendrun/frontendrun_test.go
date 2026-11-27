// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package frontendrun

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/joinsider/pulsequorum/internal/frontendcfg"
	"github.com/joinsider/pulsequorum/internal/webfront"
	"github.com/joinsider/pulsequorum/internal/webui"
)

func testConfig(t *testing.T, extra map[string]string) frontendcfg.Config {
	t.Helper()
	m := map[string]string{"FRONTEND_NODES": "https://n1.example.com,https://n2.example.com"}
	for k, v := range extra {
		m[k] = v
	}
	c, err := frontendcfg.Load(func(key, def string) string {
		if v := m[key]; v != "" {
			return v
		}
		return def
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

func testSPA() http.Handler {
	return webui.NewHandler(fstest.MapFS{
		"index.html":       {Data: []byte("<!doctype html><title>app</title>")},
		"main-ABCDEFGH.js": {Data: []byte("console.log(1)")},
	})
}

func serve(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestHandlerHeaders(t *testing.T) {
	cfg := testConfig(t, map[string]string{"FRONTEND_TLS_CERT": "/c.pem", "FRONTEND_TLS_KEY": "/k.pem"})
	h := Handler(cfg, testSPA())

	for _, path := range []string{"/", "/config.json", "/healthz", "/api/v1/health", "/missing.js", "/some/route"} {
		rec := serve(t, h, http.MethodGet, path)
		for _, want := range cfg.Headers() {
			if got := rec.Header().Get(want.Name); got != want.Value {
				t.Errorf("%s: header %s = %q, want %q", path, want.Name, got, want.Value)
			}
		}
	}
	if got := serve(t, h, http.MethodGet, "/").Header().Get("Strict-Transport-Security"); got != webfront.HSTS {
		t.Errorf("HSTS on / = %q, want %q (TLS configured)", got, webfront.HSTS)
	}
}

func TestHandlerCSPContainsNodes(t *testing.T) {
	cfg := testConfig(t, nil)
	csp := serve(t, Handler(cfg, testSPA()), http.MethodGet, "/").Header().Get("Content-Security-Policy")
	for _, want := range []string{"https://n1.example.com", "wss://n1.example.com", "https://n2.example.com", "wss://n2.example.com"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q does not contain %q", csp, want)
		}
	}
	if strings.Contains(csp, "Strict-Transport-Security") {
		t.Error("unexpected HSTS in CSP")
	}
	if got := serve(t, Handler(cfg, testSPA()), http.MethodGet, "/").Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS = %q without TLS in auto mode, want none", got)
	}
}

func TestHandlerConfigJSON(t *testing.T) {
	cfg := testConfig(t, map[string]string{
		"AUTH_MODE":      "oidc",
		"OIDC_ISSUER":    "https://idp.example.com",
		"OIDC_CLIENT_ID": "spa",
	})
	rec := serve(t, Handler(cfg, testSPA()), http.MethodGet, "/config.json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var got webfront.Config
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, rec.Body)
	}
	if want := cfg.FrontendConfig(); !slices.Equal(got.Nodes, want.Nodes) || got.Auth.ClientID != "spa" || got.Auth.Issuer != "https://idp.example.com" {
		t.Errorf("config.json = %+v, want %+v", got, want)
	}
}

func TestHandlerRoutes(t *testing.T) {
	h := Handler(testConfig(t, nil), testSPA())

	rec := serve(t, h, http.MethodGet, "/healthz")
	if rec.Code != http.StatusOK || rec.Body.String() != `{"status":"ok"}`+"\n" {
		t.Errorf("/healthz = %d %q", rec.Code, rec.Body)
	}
	for _, path := range []string{"/api/v1/health", "/api/v1/auth/login", "/api/"} {
		if rec := serve(t, h, http.MethodGet, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
	if rec := serve(t, h, http.MethodGet, "/"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>app</title>") {
		t.Errorf("GET / = %d, want index.html", rec.Code)
	}
	if rec := serve(t, h, http.MethodGet, "/missing.js"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /missing.js = %d, want 404", rec.Code)
	}
	if rec := serve(t, h, http.MethodPost, "/config.json"); rec.Code == http.StatusOK {
		t.Errorf("POST /config.json = %d, want non-200", rec.Code)
	}
}

func TestRunShutsDownOnCancel(t *testing.T) {
	cfg := testConfig(t, map[string]string{"FRONTEND_LISTEN": "127.0.0.1:0"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
