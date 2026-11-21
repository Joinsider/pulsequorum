// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"context"
	"net/http"

	"github.com/joinsider/pulsequorum/internal/auth"
	"github.com/joinsider/pulsequorum/internal/store"
)

// User management rules (FA-AUTH-09). The handlers in users.go call these
// checks before proposing a change; the tenant always comes from the actor.

// userChange describes an intended change to a user.
type userChange struct {
	Target  *store.User // nil on create
	NewRole auth.Role   // "" = unchanged
	Disable bool
	Delete  bool
	// Password is an admin password reset.
	Password bool
}

func forbidden(detail string) *Problem {
	return &Problem{Status: http.StatusForbidden, Title: "Forbidden", Detail: detail}
}

func isPlatformRole(r string) bool {
	return auth.Role(r) == auth.RolePlatformAdmin
}

// authorizeUserChange returns nil if actor may make the change:
//   - only admins (tenant-admin, platform-admin) manage users, within their tenant;
//   - only platform admins create, change or delete platform admins;
//   - nobody demotes, disables or deletes themselves (no accidental lockout);
//   - nobody resets their own password here (that needs the current one);
//   - the last enabled platform admin cannot be demoted, disabled or deleted.
func (h *Handlers) authorizeUserChange(ctx context.Context, actor auth.Principal, c userChange) error {
	if !actor.CanAdmin() {
		return forbidden("your role does not allow this action")
	}
	if c.NewRole != "" && !auth.CanAssignRole(actor, c.NewRole) {
		return forbidden("you cannot assign this role")
	}
	if c.Target == nil {
		return nil
	}
	if c.Target.TenantID != actor.TenantID {
		return notFound("user")
	}
	if isPlatformRole(c.Target.Role) && !actor.Has(auth.RolePlatformAdmin) {
		return forbidden("only platform admins can change platform admins")
	}
	demote := c.NewRole != "" && c.NewRole != auth.Role(c.Target.Role)
	if err := checkSelfUserChange(actor, c, demote); err != nil {
		return err
	}
	if isPlatformRole(c.Target.Role) && !c.Target.Disabled && (demote || c.Disable || c.Delete) {
		return h.ensureOtherPlatformAdmin(ctx, c.Target)
	}
	return nil
}

func checkSelfUserChange(actor auth.Principal, c userChange, demote bool) error {
	if c.Target.ID != actor.UserID {
		return nil
	}
	if demote || c.Disable || c.Delete {
		return &Problem{Status: http.StatusUnprocessableEntity, Title: problemTitleValidationFailed,
			Detail: "you cannot demote, disable or delete yourself"}
	}
	// Changing one's own password needs the current one (PUT /me/password);
	// otherwise a stolen access token could take over the account for good.
	if c.Password {
		return &Problem{Status: http.StatusUnprocessableEntity, Title: problemTitleValidationFailed,
			Detail: "change your own password with PUT /me/password"}
	}
	return nil
}

func (h *Handlers) ensureOtherPlatformAdmin(ctx context.Context, target *store.User) error {
	users, err := h.deps.State.ListUsers(ctx, target.TenantID)
	if err != nil {
		return err
	}
	for _, u := range users {
		if u.ID != target.ID && isPlatformRole(u.Role) && !u.Disabled {
			return nil
		}
	}
	return &Problem{Status: http.StatusConflict, Title: "Conflict", Detail: "the last platform admin cannot be removed"}
}
