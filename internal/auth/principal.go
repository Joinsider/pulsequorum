// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package auth defines the authenticated principal and the role model
// (Konzept 12.2, FA-AUTH-09). The tenant always comes from the principal,
// never from request parameters (FA-TEN-02).
package auth

import (
	"context"
	"errors"
	"net/http"
	"slices"
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

// Principal is the authenticated caller.
type Principal struct {
	UserID   string
	Username string
	TenantID string
	Roles    []Role
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
	return p, ok && p.TenantID != ""
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
