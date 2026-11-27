// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/joinsider/pulsequorum/internal/publicnodes"
	"github.com/joinsider/pulsequorum/internal/store"
)

type failoverRegistry []store.Node

func (r failoverRegistry) ListNodes(context.Context) ([]store.Node, error) { return r, nil }

// failoverServer is a voter n1 (https://mon1.example.com) with two other
// voters in the registry (ADR 0036).
func failoverServer(t *testing.T) http.Handler {
	t.Helper()
	s := testServer()
	s.api.PublicNodes = publicnodes.New("n1", s.cfg.PublicURL, failoverRegistry{
		{ID: "n2", Role: store.NodeRoleVoter, PublicURL: "https://mon2.example.com", Healthy: true},
		{ID: "n3", Role: store.NodeRoleVoter, PublicURL: "http://localhost:8083", Healthy: true},
		{ID: "p1", Role: store.NodeRoleProbe, PublicURL: "https://probe.example.com", Healthy: true},
	}, nil, nil)
	return s.Handler()
}

func TestCORSFailoverOrigins(t *testing.T) {
	h := failoverServer(t)
	preflight := map[string]string{"Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "authorization"}
	tests := []struct {
		name, method, path, origin string
		hdr                        map[string]string
		wantAllow                  string
		wantCreds                  bool
	}{
		{"registry voter, bearer", http.MethodGet, "/api/v1/monitors", "https://mon2.example.com", nil, "https://mon2.example.com", false},
		{"registry voter, loopback", http.MethodGet, "/api/v1/monitors", "http://localhost:8083", nil, "http://localhost:8083", false},
		{"registry voter, refresh", http.MethodPost, "/api/v1/auth/refresh", "https://mon2.example.com", nil, "https://mon2.example.com", true},
		{"registry voter, password change", http.MethodPut, "/api/v1/me/password", "https://mon2.example.com", nil, "https://mon2.example.com", true},
		{"preflight auth", http.MethodOptions, "/api/v1/auth/login", "https://mon2.example.com", preflight, "https://mon2.example.com", true},
		{"preflight bearer", http.MethodOptions, "/api/v1/monitors", "https://mon2.example.com", preflight, "https://mon2.example.com", false},
		{"auth prefix lookalike", http.MethodPost, "/api/v1/authx", "https://mon2.example.com", nil, "https://mon2.example.com", false},
		{"probe origin", http.MethodGet, "/api/v1/monitors", "https://probe.example.com", nil, "", false},
		{"default port spelled out", http.MethodGet, "/api/v1/monitors", "https://mon2.example.com:443", nil, "", false},
		{"other port", http.MethodGet, "/api/v1/monitors", "http://localhost:8084", nil, "", false},
		{"null", http.MethodPost, "/api/v1/auth/refresh", "null", nil, "", false},
		{"suffix attack", http.MethodGet, "/api/v1/monitors", "https://mon2.example.com.evil.net", nil, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("Origin", tc.origin)
			for k, v := range tc.hdr {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != tc.wantAllow {
				t.Errorf("Allow-Origin = %q, want %q", got, tc.wantAllow)
			}
			if got := rec.Header().Get("Access-Control-Allow-Credentials") == "true"; got != tc.wantCreds {
				t.Errorf("Allow-Credentials = %v, want %v", got, tc.wantCreds)
			}
			if tc.method == http.MethodOptions && tc.wantAllow != "" {
				if got := rec.Header().Get("Access-Control-Max-Age"); got != "7200" {
					t.Errorf("Max-Age = %q, want 7200", got)
				}
			}
		})
	}
}

func TestSPACSPWithFailoverNodes(t *testing.T) {
	h := failoverServer(t)
	csp := get(t, h, "/").Header().Get("Content-Security-Policy")
	want := "connect-src 'self' https://mon1.example.com wss://mon1.example.com https://mon2.example.com wss://mon2.example.com http://localhost:8083 ws://localhost:8083;"
	if !strings.Contains(csp, want) {
		t.Fatalf("SPA CSP = %q, want %q", csp, want)
	}
	if strings.Contains(csp, "probe.example.com") {
		t.Fatalf("probe origin in CSP: %q", csp)
	}
	if got := get(t, h, "/api/v1/health").Header().Get("Content-Security-Policy"); got != cspAPI {
		t.Fatalf("API CSP = %q", got)
	}
}

func TestSPACSPRejectsUnsafeOrigins(t *testing.T) {
	for _, n := range []string{
		"https://a.example; script-src *",
		"https://a.example\r\nX-Evil: 1",
		"https://A.example",
		"https://a.example/",
		"http://a.example",
		"*",
		"'unsafe-inline'",
	} {
		if got := spaCSP("", []string{n}); got != cspSPA {
			t.Errorf("spaCSP(%q) = %q, want unchanged policy", n, got)
		}
	}
	got := spaCSP("https://id.example.test/realms/x", []string{"https://n1.example.com", "https://n1.example.com"})
	want := "connect-src 'self' https://n1.example.com wss://n1.example.com https://id.example.test;"
	if !strings.Contains(got, want) {
		t.Fatalf("spaCSP = %q, want %q", got, want)
	}
}

func TestSiteOf(t *testing.T) {
	for origin, want := range map[string]string{
		"https://mon1.example.com":     "https://example.com",
		"https://status.example.com":   "https://example.com",
		"https://a.example.co.uk:8443": "https://example.co.uk",
		"https://user.github.io":       "https://user.github.io",
		"http://localhost:8080":        "http://localhost",
		"https://192.0.2.1":            "https://192.0.2.1",
		"http://example.com":           "http://example.com",
	} {
		if got, ok := siteOf(origin); !ok || got != want {
			t.Errorf("siteOf(%q) = %q, %v; want %q", origin, got, ok, want)
		}
	}
}
