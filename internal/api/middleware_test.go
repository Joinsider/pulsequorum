// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/joinsider/pulsequorum/internal/auth"
)

func TestAuthProblem(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		err        error
		status     int
		detail     string
		notInclude string
	}{
		{"unauthenticated", auth.ErrUnauthenticated, http.StatusUnauthorized, "authentication required", ""},
		{"opaque token", auth.ErrOpaqueToken, http.StatusUnauthorized, "opaque access tokens are not supported", ""},
		{"forbidden with reason", fmt.Errorf("%w: organization is not provisioned", auth.ErrForbidden), http.StatusForbidden, "organization is not provisioned", ""},
		{"unavailable hides the cause", fmt.Errorf("%w: tenant lookup: disk on fire", auth.ErrAuthUnavailable), http.StatusServiceUnavailable, "temporarily unavailable", "disk"},
		{"unknown error is a plain 401", errors.New("secret internals"), http.StatusUnauthorized, "authentication required", "secret"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := authProblem(tc.err)
			if p.Status != tc.status || !strings.Contains(p.Detail, tc.detail) {
				t.Fatalf("problem = %d %q, want %d containing %q", p.Status, p.Detail, tc.status, tc.detail)
			}
			if tc.notInclude != "" && strings.Contains(p.Detail, tc.notInclude) {
				t.Fatalf("detail %q leaks %q", p.Detail, tc.notInclude)
			}
		})
	}
}
