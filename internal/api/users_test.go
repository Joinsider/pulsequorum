// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/pulsequorum/internal/api/gen"
	"github.com/joinsider/pulsequorum/internal/auth"
	"github.com/joinsider/pulsequorum/internal/cluster"
)

// assertNoHash fails if a response body leaks a password hash (FA-AUTH-01).
func assertNoHash(t *testing.T, res response) {
	t.Helper()
	if strings.Contains(strings.ToLower(string(res.body)), "argon2") {
		t.Errorf("response leaks a password hash: %s", res.body)
	}
}

// TestUserManagement covers the user endpoints with real local tokens: the
// role rules of FA-AUTH-09, the self and last-admin protections, password
// resets and the response shape.
func TestUserManagement(t *testing.T) {
	t.Parallel()
	e, admin := newLocalAdminEnv(t)
	adminID := decode[gen.Me](t, e.do("GET", "/api/v1/me", "", nil, bearer(admin)...)).Id

	tenantAdminID, tenantAdmin := e.addUser(admin, "tadmin", "tenant-admin")
	editorID, editor := e.addUser(tenantAdmin, "editor", "tenant-editor")
	viewerID, viewer := e.addUser(admin, "viewer", "tenant-viewer")

	t.Run("tenant admin cannot create platform admins", func(t *testing.T) {
		if res := e.createUserRes(tenantAdmin, "sneaky", "platform-admin"); res.code != http.StatusForbidden {
			t.Errorf("tenant admin creates platform admin = %d, want 403", res.code)
		}
	})

	t.Run("tenant admin can create editors", func(t *testing.T) {
		res := e.createUserRes(tenantAdmin, "editor2", "tenant-editor")
		if res.code != http.StatusCreated {
			t.Fatalf("create editor = %d %s", res.code, res.body)
		}
		assertNoHash(t, res)
	})

	t.Run("editors and viewers cannot list or create users", func(t *testing.T) {
		for name, token := range map[string]string{"editor": editor, "viewer": viewer} {
			if res := e.do("GET", "/api/v1/users", "", nil, bearer(token)...); res.code != http.StatusForbidden {
				t.Errorf("%s lists users = %d, want 403", name, res.code)
			}
			if res := e.createUserRes(token, "nope-"+name, "tenant-viewer"); res.code != http.StatusForbidden {
				t.Errorf("%s creates user = %d, want 403", name, res.code)
			}
		}
	})

	t.Run("duplicate username in another case is a conflict", func(t *testing.T) {
		if res := e.createUserRes(admin, "EDITOR", "tenant-viewer"); res.code != http.StatusConflict {
			t.Errorf("duplicate = %d %s, want 409", res.code, res.body)
		}
	})

	t.Run("weak password and short username are rejected", func(t *testing.T) {
		res := e.do("POST", "/api/v1/users", "", map[string]any{
			"username": "weakpw", "password": "short", "role": "tenant-viewer",
		}, bearer(admin)...)
		if res.code != http.StatusUnprocessableEntity {
			t.Errorf("weak password = %d, want 422", res.code)
		}
		res = e.do("POST", "/api/v1/users", "", map[string]any{
			"username": "ab", "password": testPassword, "role": "tenant-viewer",
		}, bearer(admin)...)
		if res.code != http.StatusUnprocessableEntity {
			t.Errorf("short username = %d, want 422", res.code)
		}
	})

	t.Run("unknown role is rejected", func(t *testing.T) {
		if res := e.createUserRes(admin, "rolex", "superuser"); res.code != http.StatusUnprocessableEntity {
			t.Errorf("unknown role = %d, want 422", res.code)
		}
	})

	t.Run("list returns users without hashes", func(t *testing.T) {
		res := e.do("GET", "/api/v1/users", "", nil, bearer(admin)...)
		if res.code != http.StatusOK {
			t.Fatalf("list = %d %s", res.code, res.body)
		}
		assertNoHash(t, res)
		users := decode[[]gen.User](t, res)
		if len(users) != 5 {
			t.Errorf("list has %d users, want 5", len(users))
		}
	})

	t.Run("update role, display name and disabled flag", func(t *testing.T) {
		before := decode[[]gen.User](t, e.do("GET", "/api/v1/users", "", nil, bearer(admin)...))
		var createdBefore time.Time
		for _, u := range before {
			if u.Id.String() == viewerID {
				createdBefore = u.CreatedAt
			}
		}

		res := e.do("PUT", "/api/v1/users/"+viewerID, "", map[string]any{
			"displayName": "Vera Viewer", "role": "tenant-editor",
		}, bearer(admin)...)
		if res.code != http.StatusOK {
			t.Fatalf("update = %d %s", res.code, res.body)
		}
		assertNoHash(t, res)
		u := decode[gen.User](t, res)
		if u.Role != "tenant-editor" || u.DisplayName == nil || *u.DisplayName != "Vera Viewer" || u.Username != "viewer" {
			t.Errorf("updated user = %+v", u)
		}
		if !u.CreatedAt.Equal(createdBefore) {
			t.Errorf("createdAt changed on update: %v -> %v", createdBefore, u.CreatedAt)
		}
		// Role changes apply to the next request of the existing token.
		if res := e.do("POST", "/api/v1/monitors", "", httpMonitor("by-vera", "https://example.com"), bearer(viewer)...); res.code != http.StatusCreated {
			t.Errorf("promoted user creates monitor = %d, want 201", res.code)
		}

		res = e.do("PUT", "/api/v1/users/"+viewerID, "", map[string]any{"disabled": true}, bearer(admin)...)
		if res.code != http.StatusOK || !decode[gen.User](t, res).Disabled {
			t.Fatalf("disable = %d %s", res.code, res.body)
		}
		if res := e.login("viewer", testPassword); res.code != http.StatusUnauthorized {
			t.Errorf("disabled user logs in = %d, want 401", res.code)
		}
		if res := e.do("GET", "/api/v1/monitors", "", nil, bearer(viewer)...); res.code != http.StatusUnauthorized {
			t.Errorf("disabled user's token = %d, want 401", res.code)
		}

		res = e.do("PUT", "/api/v1/users/"+viewerID, "", map[string]any{"disabled": false}, bearer(admin)...)
		if res.code != http.StatusOK || decode[gen.User](t, res).Disabled {
			t.Fatalf("re-enable = %d %s", res.code, res.body)
		}
		e.mustLogin("viewer", testPassword)
	})

	t.Run("admin password reset revokes the user's sessions", func(t *testing.T) {
		_, oldCookie := e.mustLogin("editor", testPassword)
		resetTo := "reset by admin 1"
		res := e.do("PUT", "/api/v1/users/"+editorID, "", map[string]any{"password": resetTo}, bearer(admin)...)
		if res.code != http.StatusOK {
			t.Fatalf("reset = %d %s", res.code, res.body)
		}
		assertNoHash(t, res)
		if res := e.do("POST", "/api/v1/auth/refresh", "", nil, append(requestedWith, cookieHeader(oldCookie)...)...); res.code != http.StatusUnauthorized {
			t.Errorf("old refresh cookie after reset = %d, want 401", res.code)
		}
		e.mustLogin("editor", resetTo)
		if res := e.login("editor", testPassword); res.code != http.StatusUnauthorized {
			t.Errorf("old password after reset = %d, want 401", res.code)
		}
	})

	t.Run("nobody can demote, disable or delete themselves", func(t *testing.T) {
		self := map[string]any{"role": "tenant-viewer"}
		if res := e.do("PUT", "/api/v1/users/"+tenantAdminID, "", self, bearer(tenantAdmin)...); res.code != http.StatusUnprocessableEntity {
			t.Errorf("tenant admin demotes self = %d, want 422", res.code)
		}
		if res := e.do("PUT", "/api/v1/users/"+tenantAdminID, "", map[string]any{"disabled": true}, bearer(tenantAdmin)...); res.code != http.StatusUnprocessableEntity {
			t.Errorf("tenant admin disables self = %d, want 422", res.code)
		}
		if res := e.do("DELETE", "/api/v1/users/"+tenantAdminID, "", nil, bearer(tenantAdmin)...); res.code != http.StatusUnprocessableEntity {
			t.Errorf("tenant admin deletes self = %d, want 422", res.code)
		}
		if res := e.do("PUT", "/api/v1/users/"+adminID, "", self, bearer(admin)...); res.code != http.StatusUnprocessableEntity {
			t.Errorf("platform admin demotes self = %d, want 422", res.code)
		}
		if res := e.do("DELETE", "/api/v1/users/"+adminID, "", nil, bearer(admin)...); res.code != http.StatusUnprocessableEntity {
			t.Errorf("platform admin deletes self = %d, want 422", res.code)
		}
		// A display name change is not a lockout and stays allowed.
		if res := e.do("PUT", "/api/v1/users/"+tenantAdminID, "", map[string]any{"displayName": "Tina"}, bearer(tenantAdmin)...); res.code != http.StatusOK {
			t.Errorf("tenant admin renames self = %d, want 200", res.code)
		}
	})

	t.Run("tenant admin cannot modify platform admins", func(t *testing.T) {
		if res := e.do("PUT", "/api/v1/users/"+adminID, "", map[string]any{"displayName": "x"}, bearer(tenantAdmin)...); res.code != http.StatusForbidden {
			t.Errorf("update platform admin = %d, want 403", res.code)
		}
		if res := e.do("DELETE", "/api/v1/users/"+adminID, "", nil, bearer(tenantAdmin)...); res.code != http.StatusForbidden {
			t.Errorf("delete platform admin = %d, want 403", res.code)
		}
	})

	t.Run("second platform admin makes demotion possible", func(t *testing.T) {
		res := e.createUserRes(admin, "root2", "platform-admin")
		if res.code != http.StatusCreated {
			t.Fatalf("create platform admin = %d %s", res.code, res.body)
		}
		root2ID := decode[gen.User](t, res).Id.String()
		if res := e.do("PUT", "/api/v1/users/"+root2ID, "", map[string]any{"role": "tenant-viewer"}, bearer(admin)...); res.code != http.StatusOK {
			t.Errorf("demote second platform admin = %d %s, want 200", res.code, res.body)
		}
	})

	t.Run("last platform admin cannot be removed", func(t *testing.T) {
		// Through the API the actor must be an enabled platform admin, so a
		// different actor always leaves one behind. The rule itself is checked
		// with a fabricated second actor.
		h := &Handlers{deps: Deps{State: e.state}}
		actor := auth.Principal{UserID: uuid.NewString(), TenantID: cluster.DefaultTenantID, Roles: []auth.Role{auth.RolePlatformAdmin}}
		target, err := e.state.GetUser(context.Background(), cluster.DefaultTenantID, adminID)
		if err != nil {
			t.Fatal(err)
		}
		for name, c := range map[string]userChange{
			"demote":  {Target: &target, NewRole: auth.RoleTenantAdmin},
			"disable": {Target: &target, Disable: true},
			"delete":  {Target: &target, Delete: true},
		} {
			err := h.authorizeUserChange(context.Background(), actor, c)
			var p *Problem
			if !errors.As(err, &p) || p.Status != http.StatusConflict {
				t.Errorf("%s of the last platform admin: %v, want 409", name, err)
			}
		}
	})

	t.Run("admins cannot reset their own password via users", func(t *testing.T) {
		for name, c := range map[string]struct{ id, token, username string }{
			"platform admin": {adminID, admin, "root-admin"},
			"tenant admin":   {tenantAdminID, tenantAdmin, "tadmin"},
		} {
			attempt := "self reset attempt 1"
			res := e.do("PUT", "/api/v1/users/"+c.id, "", map[string]any{"password": attempt}, bearer(c.token)...)
			if res.code != http.StatusUnprocessableEntity {
				t.Errorf("%s self reset = %d %s, want 422", name, res.code, res.body)
			}
			assertNoHash(t, res)
			// The rejected reset changed nothing: the old password still works.
			e.mustLogin(c.username, testPassword)
		}
	})

	t.Run("unknown user is not found", func(t *testing.T) {
		missing := uuid.NewString()
		if res := e.do("PUT", "/api/v1/users/"+missing, "", map[string]any{"displayName": "x"}, bearer(admin)...); res.code != http.StatusNotFound {
			t.Errorf("update unknown = %d, want 404", res.code)
		}
		if res := e.do("DELETE", "/api/v1/users/"+missing, "", nil, bearer(admin)...); res.code != http.StatusNotFound {
			t.Errorf("delete unknown = %d, want 404", res.code)
		}
	})

	t.Run("delete removes the user", func(t *testing.T) {
		id, token := e.addUser(admin, "doomed", "tenant-viewer")
		if res := e.do("DELETE", "/api/v1/users/"+id, "", nil, bearer(admin)...); res.code != http.StatusNoContent {
			t.Fatalf("delete = %d %s", res.code, res.body)
		}
		if res := e.do("GET", "/api/v1/monitors", "", nil, bearer(token)...); res.code != http.StatusUnauthorized {
			t.Errorf("deleted user's token = %d, want 401", res.code)
		}
		if res := e.login("doomed", testPassword); res.code != http.StatusUnauthorized {
			t.Errorf("deleted user logs in = %d, want 401", res.code)
		}
	})

	t.Run("last login is recorded", func(t *testing.T) {
		id, _ := e.addUser(admin, "lastlogin", "tenant-viewer")
		users := decode[[]gen.User](t, e.do("GET", "/api/v1/users", "", nil, bearer(admin)...))
		for _, u := range users {
			if u.Id.String() == id && u.LastLoginAt == nil {
				// addUser logged the user in, so the timestamp must already be set.
				t.Errorf("lastLoginAt not set after login: %+v", u)
			}
		}
	})
}
