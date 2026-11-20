// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package server

import (
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/joinsider/pulsequorum/internal/api"
	"github.com/joinsider/pulsequorum/internal/ratelimit"
)

// Rate limits per client IP (FA-API-07). Authentication endpoints are also
// throttled per account (FA-AUTH-04); this limit caps the request volume of a
// single address. Limits are per node.
const (
	authRate, authBurst     = 1.0, 10
	publicRate, publicBurst = 10.0, 50
	rateLimitKeys           = 20_000
)

// limits holds the limiters for the unauthenticated endpoints.
type limits struct {
	auth, public *ratelimit.Limiter
}

func newLimits() *limits {
	l := &limits{auth: ratelimit.New(authRate, authBurst), public: ratelimit.New(publicRate, publicBurst)}
	l.auth.Limit, l.public.Limit = rateLimitKeys, rateLimitKeys
	return l
}

// setClock sets the clock of both limiters; nil keeps time.Now.
func (l *limits) setClock(now func() time.Time) {
	l.auth.Now, l.public.Now = now, now
}

// limiterFor returns the limiter of an unauthenticated endpoint, or nil.
// Liveness and readiness probes are never limited.
func (l *limits) limiterFor(r *http.Request) *ratelimit.Limiter {
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, api.BaseURL+"/auth/") && r.Method == http.MethodPost:
		return l.auth
	case p == api.BaseURL+"/health", p == api.BaseURL+"/stream", p == "/config.json":
		return l.public
	}
	return nil
}

func (l *limits) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if lim := l.limiterFor(r); lim != nil {
			if ok, wait := lim.Allow(peerIP(r)); !ok {
				w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
				api.WriteProblem(w, http.StatusTooManyRequests, "Too Many Requests", "rate limit exceeded, try again later")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// peerIP is the TCP peer address; proxy headers are not trusted (see
// api.clientIP).
func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
