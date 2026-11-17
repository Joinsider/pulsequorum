// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/joinsider/uptimecluster/internal/api/gen"
	"github.com/joinsider/uptimecluster/internal/auth"
)

// publicOperations returns the operation IDs the contract marks with an empty
// security requirement (`security: []`). Deriving them from the embedded spec
// keeps the contract the single source of truth.
func publicOperations() (map[string]bool, error) {
	spec, err := gen.GetSpec()
	if err != nil {
		return nil, fmt.Errorf("load embedded OpenAPI spec: %w", err)
	}
	public := map[string]bool{}
	for _, item := range spec.Paths.Map() {
		for _, op := range item.Operations() {
			if op.Security != nil && len(*op.Security) == 0 {
				public[lowerFirst(op.OperationID)] = true
			}
		}
	}
	return public, nil
}

// authenticate requires a principal for every non-public operation. Operation
// IDs are compared with a lower-case first letter, since the generator
// capitalizes them.
func authenticate(public map[string]bool, authn auth.Authenticator) gen.StrictMiddlewareFunc {
	return func(next gen.StrictHandlerFunc, operationID string) gen.StrictHandlerFunc {
		if public[lowerFirst(operationID)] {
			return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
				return next(withHTTP(ctx, w, r), w, r, request)
			}
		}
		return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
			p, err := authn.Authenticate(r)
			if err != nil {
				return nil, authProblem(err)
			}
			return next(withHTTP(auth.WithPrincipal(ctx, p), w, r), w, r, request)
		}
	}
}

// authProblem maps an authentication error to a problem response. The
// authenticators only return errors whose messages are safe to show.
func authProblem(err error) *Problem {
	switch {
	case errors.Is(err, auth.ErrAuthUnavailable):
		return &Problem{Status: http.StatusServiceUnavailable, Title: "Service Unavailable", Detail: auth.ErrAuthUnavailable.Error()}
	case errors.Is(err, auth.ErrForbidden):
		return &Problem{Status: http.StatusForbidden, Title: "Forbidden", Detail: err.Error()}
	case errors.Is(err, auth.ErrUnauthenticated):
		return &Problem{Status: http.StatusUnauthorized, Title: "Unauthorized", Detail: err.Error()}
	}
	return &Problem{Status: http.StatusUnauthorized, Title: "Unauthorized", Detail: auth.ErrUnauthenticated.Error()}
}

type httpKey struct{}

type httpPair struct {
	w http.ResponseWriter
	r *http.Request
}

// withHTTP gives handlers access to cookies, the client address and response
// headers that the strict handler types do not model (auth endpoints).
func withHTTP(ctx context.Context, w http.ResponseWriter, r *http.Request) context.Context {
	return context.WithValue(ctx, httpKey{}, httpPair{w: w, r: r})
}

func httpFrom(ctx context.Context) (http.ResponseWriter, *http.Request, bool) {
	p, ok := ctx.Value(httpKey{}).(httpPair)
	return p.w, p.r, ok
}

func lowerFirst(s string) string {
	if s == "" || s[0] < 'A' || s[0] > 'Z' {
		return s
	}
	return string(s[0]+('a'-'A')) + s[1:]
}
