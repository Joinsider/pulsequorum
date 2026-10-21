// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSecurityHeaders(t *testing.T) {
	h := testServer().Handler()
	for _, tc := range []struct {
		path, csp string
	}{
		{"/api/v1/health", cspAPI},
		{"/", cspSPA},
		{"/monitors/123", cspSPA},
	} {
		rec := get(t, h, tc.path)
		hdr := rec.Header()
		if got := hdr.Get("Content-Security-Policy"); got != tc.csp {
			t.Errorf("%s: CSP = %q, want %q", tc.path, got, tc.csp)
		}
		if strings.Contains(hdr.Get("Content-Security-Policy"), "unsafe-eval") {
			t.Errorf("%s: CSP allows unsafe-eval", tc.path)
		}
		if !strings.Contains(hdr.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Errorf("%s: CSP lacks frame-ancestors 'none'", tc.path)
		}
		for k, v := range map[string]string{"X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer", "X-Frame-Options": "DENY"} {
			if hdr.Get(k) != v {
				t.Errorf("%s: %s = %q, want %q", tc.path, k, hdr.Get(k), v)
			}
		}
	}
}

func TestCORS(t *testing.T) {
	cfg := testConfig()
	cfg.CORSOrigins = []string{"https://mon1.example.com", "https://status.example.com"}
	s := testServer()
	s.cfg = cfg
	h := s.Handler()

	do := func(method, path, origin string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	preflight := map[string]string{"Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "authorization"}

	tests := []struct {
		name, method, path, origin string
		hdr                        map[string]string
		wantStatus                 int
		wantAllow                  string
	}{
		{"allowed simple", http.MethodGet, "/api/v1/health", "https://status.example.com", nil, http.StatusOK, "https://status.example.com"},
		{"allowed case-insensitive", http.MethodGet, "/api/v1/health", "https://STATUS.example.com", nil, http.StatusOK, "https://STATUS.example.com"},
		{"foreign origin", http.MethodGet, "/api/v1/health", "https://evil.example.net", nil, http.StatusOK, ""},
		{"null origin", http.MethodGet, "/api/v1/health", "null", nil, http.StatusOK, ""},
		{"no origin", http.MethodGet, "/api/v1/health", "", nil, http.StatusOK, ""},
		{"allowed preflight", http.MethodOptions, "/api/v1/monitors", "https://mon1.example.com", preflight, http.StatusNoContent, "https://mon1.example.com"},
		{"rejected preflight", http.MethodOptions, "/api/v1/monitors", "https://evil.example.net", preflight, http.StatusNoContent, ""},
		{"spa path untouched", http.MethodGet, "/", "https://status.example.com", nil, http.StatusOK, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(tc.method, tc.path, tc.origin, tc.hdr)
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			got := rec.Header().Get("Access-Control-Allow-Origin")
			if got != tc.wantAllow {
				t.Errorf("Allow-Origin = %q, want %q", got, tc.wantAllow)
			}
			if got == "*" {
				t.Error("wildcard origin")
			}
			creds := rec.Header().Get("Access-Control-Allow-Credentials")
			if (tc.wantAllow != "") != (creds == "true") {
				t.Errorf("Allow-Credentials = %q", creds)
			}
			if strings.HasPrefix(tc.path, "/api/") && !strings.Contains(strings.Join(rec.Header().Values("Vary"), ","), "Origin") {
				t.Errorf("missing Vary: Origin, got %v", rec.Header().Values("Vary"))
			}
			if tc.method == http.MethodOptions && tc.wantAllow != "" {
				if !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "Authorization") {
					t.Errorf("Allow-Headers = %q", rec.Header().Get("Access-Control-Allow-Headers"))
				}
			}
		})
	}
}
