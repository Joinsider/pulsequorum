// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/joinsider/uptimecluster/internal/auth"
	"github.com/joinsider/uptimecluster/internal/store"
)

// Identity is what a profile derives from verified claims.
type Identity struct {
	TenantID string
	Roles    []auth.Role
}

// Profile maps verified claims to tenant and roles (ADR 0027). Errors wrap
// auth.ErrForbidden or auth.ErrAuthUnavailable.
type Profile interface {
	Map(ctx context.Context, claims map[string]json.RawMessage) (Identity, error)
}

var errNoRole = fmt.Errorf("%w: the token grants no role for this application", auth.ErrForbidden)

// Generic is the profile for any OIDC provider: one tenant, roles from a
// configurable claim.
type Generic struct {
	// TenantID is the single tenant (default).
	TenantID string
	// RolesClaim is a dot path into the claims, e.g. realm_access.roles.
	RolesClaim string
	// RoleMap maps provider values to roles. If empty, only values equal to
	// a role name count.
	RoleMap map[string]auth.Role
	// DefaultRole is given when the claim yields no valid role; empty means
	// none. It is never platform-admin (ADR 0028).
	DefaultRole auth.Role
}

// Map implements Profile.
func (g Generic) Map(_ context.Context, claims map[string]json.RawMessage) (Identity, error) {
	values, err := claimStrings(claims, g.RolesClaim)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: roles claim %q: %w", auth.ErrForbidden, g.RolesClaim, err)
	}
	var roles []auth.Role
	for _, v := range values {
		r := auth.Role(v)
		if len(g.RoleMap) > 0 {
			var ok bool
			if r, ok = g.RoleMap[v]; !ok {
				continue
			}
		}
		roles = addRole(roles, r)
	}
	if len(roles) == 0 && g.DefaultRole != "" && g.DefaultRole != auth.RolePlatformAdmin {
		roles = addRole(roles, g.DefaultRole)
	}
	if len(roles) == 0 {
		return Identity{}, errNoRole
	}
	return Identity{TenantID: g.TenantID, Roles: roles}, nil
}

// claimStrings follows a dot path and returns a string or an array of
// strings. A missing claim is an empty result; any other type is an error.
func claimStrings(claims map[string]json.RawMessage, path string) ([]string, error) {
	parts := strings.Split(path, ".")
	raw, ok := claims[parts[0]]
	for _, p := range parts[1:] {
		if !ok {
			break
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, errors.New("is not an object")
		}
		raw, ok = obj[p]
	}
	if !ok {
		return nil, nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, errors.New("is neither a string nor an array of strings")
	}
	return many, nil
}

// addRole appends a valid role once.
func addRole(roles []auth.Role, r auth.Role) []auth.Role {
	if !auth.ValidRole(r) {
		return roles
	}
	for _, have := range roles {
		if have == r {
			return roles
		}
	}
	return append(roles, r)
}

// Zitadel claim names.
const (
	claimResourceOwner     = "urn:zitadel:iam:user:resourceowner:id"
	claimResourceOwnerName = "urn:zitadel:iam:user:resourceowner:name"
)

func zitadelRolesClaim(projectID string) string {
	return "urn:zitadel:iam:org:project:" + projectID + ":roles"
}

// TenantByOrg finds the tenant bound to an organization.
type TenantByOrg interface {
	GetTenantByOrg(ctx context.Context, orgID string) (store.Tenant, error)
}

// Zitadel is the profile for Zitadel: the tenant comes from the user's
// organization, roles from the project roles claim and only where granted in
// that organization (FA-AUTH-06, PF-31).
type Zitadel struct {
	ProjectID     string
	PlatformOrgID string
	// Multi selects OIDC_TENANCY=multi; otherwise only the platform
	// organization is admitted and maps to DefaultTenantID.
	Multi           bool
	DefaultTenantID string
	Tenants         TenantByOrg
	// Provisioner creates the tenant of an unknown organization on its first
	// login (FA-TEN-03, ADR 0029). Nil disables automatic provisioning; it is
	// only used with Multi.
	Provisioner Provisioner
}

// Provisioner creates the tenant of an organization (ADR 0029). It returns
// the tenant ID; if the organization was provisioned concurrently, the ID of
// that tenant. Errors wrapping auth.ErrForbidden are shown to the caller.
type Provisioner interface {
	Provision(ctx context.Context, orgID, orgName string) (string, error)
}

// Map implements Profile.
func (z Zitadel) Map(ctx context.Context, claims map[string]json.RawMessage) (Identity, error) {
	var org string
	if raw, ok := claims[claimResourceOwner]; ok {
		if err := json.Unmarshal(raw, &org); err != nil {
			return Identity{}, fmt.Errorf("%w: malformed organization claim", auth.ErrForbidden)
		}
	}
	if org == "" {
		return Identity{}, fmt.Errorf("%w: the token names no organization; request the scope urn:zitadel:iam:user:resourceowner", auth.ErrForbidden)
	}

	// Roles first: automatic provisioning needs a role granted in the
	// organization, which only exists after the Zitadel operator granted the
	// project to it (ADR 0029). A self-registered organization alone gets no tenant.
	var granted map[string]map[string]json.RawMessage
	if raw, ok := claims[zitadelRolesClaim(z.ProjectID)]; ok {
		if err := json.Unmarshal(raw, &granted); err != nil {
			return Identity{}, fmt.Errorf("%w: malformed roles claim", auth.ErrForbidden)
		}
	}
	var roles []auth.Role
	for name, orgs := range granted {
		if _, ok := orgs[org]; !ok {
			continue // granted in another organization
		}
		r := auth.Role(name)
		if r == auth.RolePlatformAdmin && org != z.PlatformOrgID {
			continue
		}
		roles = addRole(roles, r)
	}

	var tenantID string
	switch {
	case org == z.PlatformOrgID && !z.Multi:
		tenantID = z.DefaultTenantID
	case !z.Multi:
		return Identity{}, fmt.Errorf("%w: organization is not admitted", auth.ErrForbidden)
	default:
		t, err := z.Tenants.GetTenantByOrg(ctx, org)
		switch {
		case errors.Is(err, store.ErrNotFound) && z.Provisioner != nil && len(roles) > 0:
			id, perr := z.Provisioner.Provision(ctx, org, orgName(claims))
			if perr != nil {
				return Identity{}, perr
			}
			tenantID = id
		case errors.Is(err, store.ErrNotFound):
			return Identity{}, fmt.Errorf("%w: organization is not provisioned", auth.ErrForbidden)
		case err != nil:
			return Identity{}, fmt.Errorf("%w: tenant lookup: %w", auth.ErrAuthUnavailable, err)
		case t.ZitadelOrgID != org:
			// Defense in depth: the store returns the bound tenant only.
			return Identity{}, fmt.Errorf("%w: organization is not provisioned", auth.ErrForbidden)
		default:
			tenantID = t.ID
		}
	}

	if len(roles) == 0 {
		return Identity{}, errNoRole
	}
	slices.Sort(roles)
	return Identity{TenantID: tenantID, Roles: roles}, nil
}

// orgName returns the organization name claim, or "" if it is missing or
// not a string. The provisioner sanitizes it.
func orgName(claims map[string]json.RawMessage) string {
	var name string
	if raw, ok := claims[claimResourceOwnerName]; ok {
		_ = json.Unmarshal(raw, &name)
	}
	return name
}
