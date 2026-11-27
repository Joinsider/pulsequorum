// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// FA-API-07: login and public endpoints are limited per client IP.
func TestRateLimit(t *testing.T) {
	h := testServer().Handler()
	do := func(method, path, ip string) int {
		req := httptest.NewRequest(method, path, nil)
		req.RemoteAddr = ip + ":1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests && rec.Header().Get("Retry-After") == "" {
			t.Errorf("%s %s: 429 without Retry-After", method, path)
		}
		return rec.Code
	}
	count := func(method, path, ip string, n int) (limited int) {
		for range n {
			if do(method, path, ip) == http.StatusTooManyRequests {
				limited++
			}
		}
		return limited
	}

	if got := count(http.MethodPost, "/api/v1/auth/login", "192.0.2.1", authBurst+5); got != 5 {
		t.Errorf("login: %d of %d requests limited, want 5", got, authBurst+5)
	}
	// Another address has its own budget.
	if code := do(http.MethodPost, "/api/v1/auth/login", "192.0.2.2"); code == http.StatusTooManyRequests {
		t.Error("second address limited")
	}
	if got := count(http.MethodGet, "/api/v1/health", "192.0.2.3", publicBurst+3); got != 3 {
		t.Errorf("health: %d limited, want 3", got)
	}
	// Probes and authenticated endpoints are not limited here.
	if got := count(http.MethodGet, "/healthz", "192.0.2.3", publicBurst+3); got != 0 {
		t.Errorf("healthz: %d limited", got)
	}
	if got := count(http.MethodGet, "/api/v1/cluster/nodes", "192.0.2.4", publicBurst+3); got != 3 {
		t.Errorf("cluster/nodes: %d limited, want 3", got)
	}
	if got := count(http.MethodGet, "/api/v1/monitors", "192.0.2.3", publicBurst+3); got != 0 {
		t.Errorf("monitors: %d limited", got)
	}
}
