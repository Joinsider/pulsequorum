// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package api

import (
	"context"
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
			return next
		}
		return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
			p, err := authn.Authenticate(r)
			if err != nil {
				return nil, &Problem{Status: http.StatusUnauthorized, Title: "Unauthorized", Detail: err.Error()}
			}
			return next(auth.WithPrincipal(ctx, p), w, r, request)
		}
	}
}

func lowerFirst(s string) string {
	if s == "" || s[0] < 'A' || s[0] > 'Z' {
		return s
	}
	return string(s[0]+('a'-'A')) + s[1:]
}
