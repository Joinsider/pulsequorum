// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package auth defines the authenticated principal and the role model
// (Konzept 12.2, FA-AUTH-09). The tenant always comes from the principal,
// never from request parameters (FA-TEN-02).
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/joinsider/uptimecluster/internal/store"
)

// Role is a role from the role model.
type Role string

// Roles (Konzept 12.2).
const (
	RolePlatformAdmin Role = "platform-admin"
	RoleTenantAdmin   Role = "tenant-admin"
	RoleTenantEditor  Role = "tenant-editor"
	RoleTenantViewer  Role = "tenant-viewer"
)

// ErrUnauthenticated means no valid credentials were presented.
var ErrUnauthenticated = errors.New("authentication required")

// ErrForbidden means valid credentials that grant no access: no usable role,
// an organization without tenant, or a suspended tenant (FA-AUTH-06). Errors
// wrapping it are answered with 403; their message is shown to the caller and
// must not contain token contents.
var ErrForbidden = errors.New("access denied")

// ErrAuthUnavailable means credentials cannot be checked right now, e.g. the
// identity provider's keys were never loaded. It is answered with 503.
var ErrAuthUnavailable = errors.New("authentication is temporarily unavailable")

// ErrOpaqueToken rejects a bearer token that is not a JWT (FA-AUTH-05). It
// wraps ErrUnauthenticated, so it is answered with 401 and this message.
var ErrOpaqueToken = fmt.Errorf("%w: opaque access tokens are not supported; configure the identity provider to issue JWT access tokens", ErrUnauthenticated)

// Principal is the authenticated caller.
type Principal struct {
	UserID   string
	Username string
	TenantID string
	Roles    []Role
	// ExpiresAt is when the presented credentials expire; zero if unknown.
	ExpiresAt time.Time
}

// Has reports whether the principal holds one of the roles.
func (p Principal) Has(roles ...Role) bool {
	for _, r := range roles {
		if slices.Contains(p.Roles, r) {
			return true
		}
	}
	return false
}

// CanRead reports read access to the principal's tenant.
func (p Principal) CanRead() bool {
	return p.Has(RoleTenantViewer, RoleTenantEditor, RoleTenantAdmin, RolePlatformAdmin)
}

// CanEdit reports permission to change monitors and run checks.
func (p Principal) CanEdit() bool {
	return p.Has(RoleTenantEditor, RoleTenantAdmin, RolePlatformAdmin)
}

// CanAdmin reports permission to manage channels, users and tenant settings.
func (p Principal) CanAdmin() bool {
	return p.Has(RoleTenantAdmin, RolePlatformAdmin)
}

type ctxKey struct{}

// WithPrincipal stores the principal in the context.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the principal of the request.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	// The system tenant is not a tenant of anyone; its channels are reached
	// only through the platform admin API (ADR 0025).
	return p, ok && p.TenantID != "" && p.TenantID != store.SystemTenantID
}

// Authenticator verifies the credentials of a request.
type Authenticator interface {
	Authenticate(r *http.Request) (Principal, error)
}

// DenyAll rejects every request. It is used until local authentication
// (AP-1.12) is available, so protected endpoints are never open by accident.
type DenyAll struct{}

// Authenticate implements Authenticator.
func (DenyAll) Authenticate(*http.Request) (Principal, error) { return Principal{}, ErrUnauthenticated }
