// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/api/gen"
	"github.com/joinsider/pulsequorum/internal/auth"
	"github.com/joinsider/pulsequorum/internal/store"
)

const (
	apiTokensPath = "/api/v1/api-tokens" //nolint:gosec // G101: route path, not a credential
	day           = 24 * time.Hour
)

// raiseCommandLevel sets the stored command level. API tokens and the audit
// log need level 9 (ADR 0031).
func raiseCommandLevel(t *testing.T, e *env, level uint32) {
	t.Helper()
	ctx := context.Background()
	if err := e.state.Update(ctx, func(tx store.StateTx) error { return tx.SetCommandLevel(ctx, level) }); err != nil {
		t.Fatal(err)
	}
}

// tokenRequest is the request body that creates a token expiring after expiresIn.
func tokenRequest(name, role string, expiresIn time.Duration) map[string]any {
	return map[string]any{"name": name, "role": role, "expiresAt": time.Now().Add(expiresIn).UTC().Format(time.RFC3339)}
}

// createToken creates a token as principal and fails on any status but 201.
func createToken(t *testing.T, e *env, principal, name, role string) gen.ApiTokenCreated {
	t.Helper()
	res := e.do("POST", apiTokensPath, principal, tokenRequest(name, role, 30*day))
	if res.code != http.StatusCreated {
		t.Fatalf("create token %s = %d %s", name, res.code, res.body)
	}
	return decode[gen.ApiTokenCreated](t, res)
}

// listTokens lists the tokens as principal and fails on any status but 200.
func listTokens(t *testing.T, e *env, principal string) []gen.ApiToken {
	t.Helper()
	res := e.do("GET", apiTokensPath, principal, nil)
	if res.code != http.StatusOK {
		t.Fatalf("list tokens = %d %s", res.code, res.body)
	}
	return decode[[]gen.ApiToken](t, res)
}

// fieldPointers returns the JSON pointers of the field errors of a problem.
func fieldPointers(t *testing.T, r response) []string {
	t.Helper()
	p := decode[gen.Problem](t, r)
	if p.Errors == nil {
		return nil
	}
	var out []string
	for _, f := range *p.Errors {
		out = append(out, f.Field)
	}
	return out
}

// TestAPITokenLifecycle covers create, list, duplicate names and revocation.
func TestAPITokenLifecycle(t *testing.T) {
	e := newEnv(t)
	raiseCommandLevel(t, e, 9)

	res := e.do("POST", apiTokensPath, tenantAdmin, tokenRequest("ci", "tenant-editor", 30*day))
	if res.code != http.StatusCreated {
		t.Fatalf("create = %d %s", res.code, res.body)
	}
	if cc := res.hdr.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	created := decode[gen.ApiTokenCreated](t, res)
	if !strings.HasPrefix(created.Token, "uct_") {
		t.Errorf("secret %q lacks the uct_ prefix", created.Token)
	}
	if !strings.HasPrefix(created.Id, "tok_") || created.Role != "tenant-editor" || created.Expired {
		t.Errorf("created = %+v", created)
	}
	if created.CreatedBy.Kind != gen.AuditUser {
		t.Errorf("createdBy.kind = %q, want user", created.CreatedBy.Kind)
	}

	// The list shows the token but never its secret, hash or any field that holds one.
	list := e.do("GET", apiTokensPath, tenantAdmin, nil)
	if list.code != http.StatusOK {
		t.Fatalf("list = %d %s", list.code, list.body)
	}
	if strings.Contains(string(list.body), created.Token) {
		t.Fatalf("list leaks the secret: %s", list.body)
	}
	var raw []map[string]any
	if err := json.Unmarshal(list.body, &raw); err != nil || len(raw) != 1 {
		t.Fatalf("list = %s (err %v), want one token", list.body, err)
	}
	for _, key := range []string{"token", "secret", "secretHash", "secret_hash"} {
		if _, ok := raw[0][key]; ok {
			t.Errorf("list item has field %q", key)
		}
	}

	t.Run("duplicate name", func(t *testing.T) {
		res := e.do("POST", apiTokensPath, tenantAdmin, tokenRequest("ci", "tenant-viewer", 30*day))
		if res.code != http.StatusConflict {
			t.Fatalf("= %d, want 409: %s", res.code, res.body)
		}
	})

	t.Run("revoke", func(t *testing.T) {
		if res := e.do("DELETE", apiTokensPath+"/"+created.Id, tenantAdmin, nil); res.code != http.StatusNoContent {
			t.Fatalf("revoke = %d %s", res.code, res.body)
		}
		if res := e.do("DELETE", apiTokensPath+"/"+created.Id, tenantAdmin, nil); res.code != http.StatusNotFound {
			t.Fatalf("second revoke = %d, want 404", res.code)
		}
		if n := len(listTokens(t, e, tenantAdmin)); n != 0 {
			t.Fatalf("%d tokens left after revoke", n)
		}
	})
}

// TestAPITokenValidation checks the field errors of the create request.
func TestAPITokenValidation(t *testing.T) {
	e := newEnv(t)
	raiseCommandLevel(t, e, 9)
	tests := []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"platform-admin role", tokenRequest("x", "platform-admin", day), "/role"},
		{"unknown role", tokenRequest("x", "owner", day), "/role"},
		{"lifetime below one hour", tokenRequest("x", "tenant-viewer", 30*time.Minute), "/expiresAt"},
		{"lifetime above 366 days", tokenRequest("x", "tenant-viewer", 367*day), "/expiresAt"},
		{"empty name", tokenRequest("", "tenant-viewer", day), "/name"},
		{"name too long", tokenRequest(strings.Repeat("n", 65), "tenant-viewer", day), "/name"},
		{"name with control character", tokenRequest("a\nb", "tenant-viewer", day), "/name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := e.do("POST", apiTokensPath, tenantAdmin, tt.body)
			if res.code != http.StatusUnprocessableEntity {
				t.Fatalf("= %d, want 422: %s", res.code, res.body)
			}
			if fields := fieldPointers(t, res); !slices.Contains(fields, tt.field) {
				t.Fatalf("fields = %v, want %s", fields, tt.field)
			}
		})
	}
	if n := len(listTokens(t, e, tenantAdmin)); n != 0 {
		t.Fatalf("%d tokens created by invalid requests", n)
	}
}

// TestAPITokenRoles: only tenant admins and platform admins manage tokens.
func TestAPITokenRoles(t *testing.T) {
	e := newEnv(t)
	raiseCommandLevel(t, e, 9)
	existing := createToken(t, e, tenantAdmin, "existing", "tenant-viewer")

	for _, principal := range []string{viewer, editor} {
		if res := e.do("GET", apiTokensPath, principal, nil); res.code != http.StatusForbidden {
			t.Errorf("list as %s = %d, want 403", principal, res.code)
		}
		if res := e.do("POST", apiTokensPath, principal, tokenRequest("x", "tenant-viewer", day)); res.code != http.StatusForbidden {
			t.Errorf("create as %s = %d, want 403", principal, res.code)
		}
		if res := e.do("DELETE", apiTokensPath+"/"+existing.Id, principal, nil); res.code != http.StatusForbidden {
			t.Errorf("revoke as %s = %d, want 403", principal, res.code)
		}
	}
	if res := e.do("GET", apiTokensPath, "", nil); res.code != http.StatusUnauthorized {
		t.Errorf("anonymous list = %d, want 401", res.code)
	}
	// A platform admin manages the tokens of the own tenant.
	if res := e.do("POST", apiTokensPath, platformAdmin, tokenRequest("by-platform", "tenant-viewer", day)); res.code != http.StatusCreated {
		t.Errorf("create as platform admin = %d %s", res.code, res.body)
	}
}

// TestAPITokenNeedsCommandLevel: below level 9 tokens are refused with 409.
func TestAPITokenNeedsCommandLevel(t *testing.T) {
	e := newEnv(t)
	raiseCommandLevel(t, e, 8)
	res := e.do("POST", apiTokensPath, tenantAdmin, tokenRequest("early", "tenant-viewer", day))
	if res.code != http.StatusConflict {
		t.Fatalf("= %d, want 409: %s", res.code, res.body)
	}
	if d := problemDetail(t, res); !strings.Contains(d, "command level 9") {
		t.Errorf("detail = %q, want the command level", d)
	}
}

// TestAPITokenTenantIsolation: a tenant admin never sees or revokes another tenant's tokens.
func TestAPITokenTenantIsolation(t *testing.T) {
	e := newEnv(t)
	raiseCommandLevel(t, e, 9)
	mine := createToken(t, e, tenantAdmin, "mine", "tenant-viewer")

	if n := len(listTokens(t, e, other)); n != 0 {
		t.Errorf("tenant-b sees %d tokens of default", n)
	}
	if res := e.do("DELETE", apiTokensPath+"/"+mine.Id, other, nil); res.code != http.StatusNotFound {
		t.Errorf("tenant-b revoke = %d, want 404", res.code)
	}
	if n := len(listTokens(t, e, tenantAdmin)); n != 1 {
		t.Errorf("default lost its token: %d left", n)
	}
}

// TestAPITokenPrincipalsViaTokenAreRefused: a principal that authenticated with
// a token gets 403 on every token and admin operation, from the middleware.
func TestAPITokenPrincipalsViaTokenAreRefused(t *testing.T) {
	e := newEnvTweaked(t, false, func(d *Deps) { d.Authenticator = viaTokenAuth{} })
	raiseCommandLevel(t, e, 9)

	if res := e.do("GET", "/api/v1/monitors", tenantAdmin, nil); res.code != http.StatusOK {
		t.Errorf("monitors via token = %d, want 200", res.code)
	}
	checks := []struct {
		method, path string
		body         any
	}{
		{"GET", apiTokensPath, nil},
		{"POST", apiTokensPath, tokenRequest("x", "tenant-viewer", day)},
		{"DELETE", apiTokensPath + "/tok_aaaaaaaaaaaaaaaa", nil},
		{"GET", "/api/v1/admin/tenants", nil},
		{"GET", "/api/v1/admin/audit", nil},
		{"GET", "/api/v1/me", nil},
	}
	for _, c := range checks {
		if res := e.do(c.method, c.path, tenantAdmin, c.body); res.code != http.StatusForbidden {
			t.Errorf("%s %s via token = %d, want 403: %s", c.method, c.path, res.code, res.body)
		}
	}
}

// viaTokenAuth authenticates like headerAuth but marks the principal as an API token.
type viaTokenAuth struct{}

func (viaTokenAuth) Authenticate(r *http.Request) (auth.Principal, error) {
	p, err := headerAuth{}.Authenticate(r)
	p.ViaToken = true
	return p, err
}

// TestAPITokenAccess uses real local logins and API tokens end to end, through
// the real middleware.
func TestAPITokenAccess(t *testing.T) {
	// clock is read by the token authenticator; zero means the real time.
	var clock time.Time
	now := func() time.Time {
		if clock.IsZero() {
			return time.Now()
		}
		return clock
	}
	e := newEnvTweaked(t, true, func(d *Deps) {
		d.Authenticator = auth.WithAPITokens(d.Authenticator, d.State, now)
	})
	res := e.setup("root-admin", testPassword)
	if res.code != http.StatusCreated {
		t.Fatalf("setup = %d %s", res.code, res.body)
	}
	admin := bearer(decode[gen.TokenResponse](t, res).AccessToken)
	raiseCommandLevel(t, e, 9)

	editorRes := e.do("POST", apiTokensPath, "", tokenRequest("ci-editor", "tenant-editor", 30*day), admin...)
	if editorRes.code != http.StatusCreated {
		t.Fatalf("create editor token = %d %s", editorRes.code, editorRes.body)
	}
	editorTok := decode[gen.ApiTokenCreated](t, editorRes)
	editor := bearer(editorTok.Token)
	adminTok := createTokenRes(t, e, admin, "ci-admin", "tenant-admin")
	tenantAdminToken := bearer(adminTok.Token)

	t.Run("editor token works on monitors", func(t *testing.T) {
		if res := e.do("POST", "/api/v1/monitors", "", httpMonitor("by-token", "https://example.com"), editor...); res.code != http.StatusCreated {
			t.Fatalf("create monitor = %d %s", res.code, res.body)
		}
		if res := e.do("GET", "/api/v1/monitors", "", nil, editor...); res.code != http.StatusOK {
			t.Fatalf("list monitors = %d %s", res.code, res.body)
		}
		if res := e.do("GET", "/api/v1/audit", "", nil, tenantAdminToken...); res.code != http.StatusOK {
			t.Fatalf("tenant-admin token reads audit = %d %s", res.code, res.body)
		}
	})

	t.Run("tokens cannot manage tokens, admin or own user", func(t *testing.T) {
		refused := []struct {
			name, method, path string
			body               any
			headers            []string
		}{
			{"create token as editor", "POST", apiTokensPath, tokenRequest("x", "tenant-viewer", day), editor},
			{"create token as tenant-admin", "POST", apiTokensPath, tokenRequest("x", "tenant-viewer", day), tenantAdminToken},
			{"list tokens as tenant-admin", "GET", apiTokensPath, nil, tenantAdminToken},
			{"revoke token as tenant-admin", "DELETE", apiTokensPath + "/" + editorTok.Id, nil, tenantAdminToken},
			{"admin tenants as tenant-admin", "GET", "/api/v1/admin/tenants", nil, tenantAdminToken},
			{"admin audit as tenant-admin", "GET", "/api/v1/admin/audit", nil, tenantAdminToken},
			{"own identity as tenant-admin", "GET", "/api/v1/me", nil, tenantAdminToken},
			{"list users as tenant-admin", "GET", "/api/v1/users", nil, tenantAdminToken},
		}
		for _, c := range refused {
			if res := e.do(c.method, c.path, "", c.body, c.headers...); res.code != http.StatusForbidden {
				t.Errorf("%s = %d, want 403: %s", c.name, res.code, res.body)
			}
		}
	})

	t.Run("unknown token is unauthorized", func(t *testing.T) {
		bogus := bearer("uct_" + strings.Repeat("a", 43))
		if res := e.do("GET", "/api/v1/monitors", "", nil, bogus...); res.code != http.StatusUnauthorized {
			t.Fatalf("= %d, want 401", res.code)
		}
	})

	t.Run("revoked token stops working", func(t *testing.T) {
		if res := e.do("DELETE", apiTokensPath+"/"+editorTok.Id, "", nil, admin...); res.code != http.StatusNoContent {
			t.Fatalf("revoke = %d %s", res.code, res.body)
		}
		if res := e.do("GET", "/api/v1/monitors", "", nil, editor...); res.code != http.StatusUnauthorized {
			t.Fatalf("revoked token = %d, want 401", res.code)
		}
	})

	t.Run("expired token is unauthorized", func(t *testing.T) {
		short := e.do("POST", apiTokensPath, "", tokenRequest("short", "tenant-viewer", 90*time.Minute), admin...)
		if short.code != http.StatusCreated {
			t.Fatalf("create = %d %s", short.code, short.body)
		}
		tok := decode[gen.ApiTokenCreated](t, short)
		if res := e.do("GET", "/api/v1/monitors", "", nil, bearer(tok.Token)...); res.code != http.StatusOK {
			t.Fatalf("before expiry = %d, want 200", res.code)
		}
		clock = time.Now().Add(3 * time.Hour)
		defer func() { clock = time.Time{} }()
		if res := e.do("GET", "/api/v1/monitors", "", nil, bearer(tok.Token)...); res.code != http.StatusUnauthorized {
			t.Fatalf("after expiry = %d, want 401", res.code)
		}
	})
}

// createTokenRes creates a token through the local login and returns the body.
func createTokenRes(t *testing.T, e *env, admin []string, name, role string) gen.ApiTokenCreated {
	t.Helper()
	res := e.do("POST", apiTokensPath, "", tokenRequest(name, role, 30*day), admin...)
	if res.code != http.StatusCreated {
		t.Fatalf("create token %s = %d %s", name, res.code, res.body)
	}
	return decode[gen.ApiTokenCreated](t, res)
}

// wantTokenOperations are the operations API tokens may call (ADR 0031): all
// operations of the tags system, monitors, notifications, maintenance,
// incidents, stream, tenant and audit.
var wantTokenOperations = []string{
	"bulkMonitors", "cloneMonitor", "createMaintenanceWindow", "createMonitor",
	"createNotificationChannel", "createStreamTicket", "deleteMaintenanceWindow",
	"deleteMonitor", "deleteNotificationChannel", "getHealth", "getIncident",
	"getMaintenanceWindow", "getMonitor", "getNotificationChannel", "getOwnTenant",
	"getUptime", "listAssignableNodes", "listAudit", "listHeartbeats", "listIncidents",
	"listMaintenanceWindows", "listMonitorTypes", "listMonitors",
	"listNotificationChannels", "pauseMonitor", "resumeMonitor", "testMonitor",
	"testNotificationChannel", "updateMaintenanceWindow", "updateMonitor",
	"updateNotificationChannel",
}

// TestTokenOperationSet pins the operations API tokens may call. A new
// operation stays closed to tokens until its tag is added to tokenTags.
func TestTokenOperationSet(t *testing.T) {
	ops, err := tokenOperations()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range wantTokenOperations {
		if !ops[name] {
			t.Errorf("%s is not open to API tokens", name)
		}
	}
	for name := range ops {
		if !slices.Contains(wantTokenOperations, name) {
			t.Errorf("%s is open to API tokens but not expected", name)
		}
	}
	for _, reserved := range []string{
		"createApiToken", "listApiTokens", "revokeApiToken", "listAdminAudit",
		"listUsers", "createUser", "getMe", "createJoinToken", "listClusterNodes",
		"createPrivateProbeJoinToken", "listPrivateProbes", "removePrivateProbe",
		"createTenant", "listTenants", "login",
	} {
		if ops[reserved] {
			t.Errorf("%s must not be open to API tokens", reserved)
		}
	}
}
