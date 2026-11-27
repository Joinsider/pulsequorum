// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package server

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/joinsider/pulsequorum/internal/api"
	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/config"
	"github.com/joinsider/pulsequorum/internal/publicnodes"
	"golang.org/x/net/publicsuffix"
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

// spaCSP returns the SPA policy. connect-src allows this origin, the public
// origins of the voters the SPA may fail over to, with their WebSocket
// counterparts (ADR 0036), and with OIDC the issuer's origin for the token
// endpoint (ADR 0027). Every origin is checked again, so that no value can
// inject a directive; nothing else is widened.
func spaCSP(issuer string, nodes []string) string {
	sources := make([]string, 0, 2*len(nodes)+1)
	add := func(src string) {
		if !slices.Contains(sources, src) {
			sources = append(sources, src)
		}
	}
	for _, n := range nodes {
		o, err := cluster.NormalizePublicOrigin(n)
		if err != nil || o != n {
			continue
		}
		add(o)
		if rest, ok := strings.CutPrefix(o, "https://"); ok {
			add("wss://" + rest)
		} else if rest, ok := strings.CutPrefix(o, "http://"); ok {
			add("ws://" + rest)
		}
	}
	if o, ok := issuerOrigin(issuer); ok {
		add(o)
	}
	if len(sources) == 0 {
		return cspSPA
	}
	return strings.Replace(cspSPA, "connect-src 'self';", "connect-src 'self' "+strings.Join(sources, " ")+";", 1)
}

// issuerOrigin returns the origin of the OIDC issuer if it is safe to put
// into a CSP source list.
func issuerOrigin(issuer string) (string, bool) {
	if issuer == "" {
		return "", false
	}
	u, err := url.Parse(issuer)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
		strings.ContainsAny(u.Host, " ;,'\"\r\n\t") {
		return "", false
	}
	return u.Scheme + "://" + u.Host, true
}

// securityHeaders sets headers that are safe for every response. issuer is
// the OIDC issuer, empty in local mode; nodes lists the voter origins for the
// SPA's connect-src (nil lists none).
func securityHeaders(next http.Handler, issuer string, nodes *publicnodes.Source) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		if isAPIPath(r.URL.Path) {
			h.Set("Content-Security-Policy", cspAPI)
		} else {
			h.Set("Content-Security-Policy", spaCSP(issuer, nodes.Origins(r.Context())))
		}
		next.ServeHTTP(w, r)
	})
}

// CORS for external frontends and failover between voters (FA-API-05,
// PF-34, ADR 0036): only origins on the allow list are reflected exactly,
// never "*" and never "null", and responses vary by Origin. The allow list is
// CORS_ORIGINS (with PUBLIC_URL) plus the public origins of the voters from
// the registry, which is protected by mTLS and Raft (ADR 0036, O2).
//
// Credentials (the refresh cookie) are allowed only on the paths that read or
// rotate it; bearer requests run without credentials.
var (
	corsMethods = "GET, POST, PUT, PATCH, DELETE"
	corsHeaders = "Authorization, Content-Type, Idempotency-Key, X-Requested-With"
	corsExpose  = "Retry-After, Location"
	corsMaxAge  = "7200"
)

func cors(allowed []string, nodes *publicnodes.Source, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isAPIPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Add("Vary", "Origin")
		origin := r.Header.Get("Origin")
		ok := corsOriginAllowed(r.Context(), origin, allowed, nodes)
		credentials := ok && isCredentialPath(r.URL.Path)
		preflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
		if preflight {
			h.Add("Vary", "Access-Control-Request-Method")
			h.Add("Vary", "Access-Control-Request-Headers")
			if ok {
				h.Set("Access-Control-Allow-Origin", origin)
				if credentials {
					h.Set("Access-Control-Allow-Credentials", "true")
				}
				h.Set("Access-Control-Allow-Methods", corsMethods)
				h.Set("Access-Control-Allow-Headers", corsHeaders)
				h.Set("Access-Control-Max-Age", corsMaxAge)
			}
			// A rejected preflight gets no CORS headers; the browser blocks
			// the actual request.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if ok {
			h.Set("Access-Control-Allow-Origin", origin)
			if credentials {
				h.Set("Access-Control-Allow-Credentials", "true")
			}
			h.Set("Access-Control-Expose-Headers", corsExpose)
		}
		next.ServeHTTP(w, r)
	})
}

// corsOriginAllowed reports whether a browser Origin may read API responses.
// "null" (sandboxed frames, file:// pages, some redirects) is never allowed.
func corsOriginAllowed(ctx context.Context, origin string, allowed []string, nodes *publicnodes.Source) bool {
	if origin == "" || origin == "null" {
		return false
	}
	return slices.Contains(allowed, strings.ToLower(origin)) || nodes.Contains(ctx, origin)
}

// isCredentialPath reports whether the refresh cookie is sent on p: the
// local auth endpoints and the password change, which rotates the cookie.
func isCredentialPath(p string) bool {
	return p == api.BaseURL+"/auth" || strings.HasPrefix(p, api.BaseURL+"/auth/") || p == api.BaseURL+"/me/password"
}

func isAPIPath(p string) bool {
	return p == "/api" || strings.HasPrefix(p, "/api/")
}

// warnCrossSiteOrigins logs every CORS origin that is cross-site to
// PUBLIC_URL while AUTH_MODE=local (ADR 0036, 4A). The refresh cookie is
// SameSite=Strict, so a frontend on another site cannot keep a local session;
// such hosts need OIDC or a domain of the same site.
func warnCrossSiteOrigins(cfg config.Config, log *slog.Logger) {
	if cfg.AuthMode != config.AuthLocal || cfg.PublicURL == "" {
		return
	}
	self, ok := siteOf(cfg.PublicURL)
	if !ok {
		return
	}
	for _, o := range cfg.CORSOrigins {
		if site, ok := siteOf(o); ok && site != self {
			log.Warn("CORS origin is cross-site to PUBLIC_URL; local login will not work there, use OIDC or a same-site domain",
				"origin", o, "public_url", cfg.PublicURL)
		}
	}
}

// siteOf returns the scheme and registrable domain (eTLD+1) of an origin,
// or the host itself for IP addresses and single-label names.
func siteOf(origin string) (string, bool) {
	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if net.ParseIP(host) != nil {
		return u.Scheme + "://" + host, true
	}
	site, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		site = host
	}
	return u.Scheme + "://" + site, true
}
