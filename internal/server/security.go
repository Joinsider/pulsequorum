// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package server

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// Content security policies (NFA-SEC-05). The SPA needs inline styles
// (Angular component styles, ECharts); scripts only come from this origin and
// eval is never allowed. API responses are data and get the strictest policy.
const (
	cspSPA = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; " +
		"base-uri 'self'; form-action 'self'; frame-ancestors 'none'"
	cspAPI = "default-src 'none'; frame-ancestors 'none'"
)

// spaCSP returns the SPA policy. With OIDC the SPA calls the token endpoint
// of the identity provider, so connect-src also allows the issuer's origin
// (ADR 0027); nothing else is widened.
func spaCSP(issuer string) string {
	if issuer == "" {
		return cspSPA
	}
	u, err := url.Parse(issuer)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
		strings.ContainsAny(u.Host, " ;,'\"") {
		return cspSPA
	}
	origin := u.Scheme + "://" + u.Host
	return strings.Replace(cspSPA, "connect-src 'self';", "connect-src 'self' "+origin+";", 1)
}

// securityHeaders sets headers that are safe for every response. issuer is
// the OIDC issuer, empty in local mode.
func securityHeaders(next http.Handler, issuer string) http.Handler {
	csp := spaCSP(issuer)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		if isAPIPath(r.URL.Path) {
			h.Set("Content-Security-Policy", cspAPI)
		} else {
			h.Set("Content-Security-Policy", csp)
		}
		next.ServeHTTP(w, r)
	})
}

// CORS for external frontends (FA-API-05, PF-34): only origins on the allow
// list are reflected exactly, never "*", and responses vary by Origin.
var (
	corsMethods = "GET, POST, PUT, PATCH, DELETE"
	corsHeaders = "Authorization, Content-Type, Idempotency-Key, X-Requested-With"
	corsExpose  = "Retry-After, Location"
)

func cors(allowed []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isAPIPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Add("Vary", "Origin")
		origin := r.Header.Get("Origin")
		ok := origin != "" && slices.Contains(allowed, strings.ToLower(origin))
		preflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
		if preflight {
			h.Add("Vary", "Access-Control-Request-Method")
			h.Add("Vary", "Access-Control-Request-Headers")
			if ok {
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Credentials", "true")
				h.Set("Access-Control-Allow-Methods", corsMethods)
				h.Set("Access-Control-Allow-Headers", corsHeaders)
				h.Set("Access-Control-Max-Age", "600")
			}
			// A rejected preflight gets no CORS headers; the browser blocks
			// the actual request.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if ok {
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Set("Access-Control-Expose-Headers", corsExpose)
		}
		next.ServeHTTP(w, r)
	})
}

func isAPIPath(p string) bool {
	return p == "/api" || strings.HasPrefix(p, "/api/")
}
