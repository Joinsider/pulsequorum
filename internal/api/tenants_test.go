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

	"github.com/google/uuid"

	"github.com/joinsider/pulsequorum/internal/api/gen"
	"github.com/joinsider/pulsequorum/internal/store"
)

const (
	tenantsPath   = "/api/v1/admin/tenants"
	plansPath     = "/api/v1/admin/plans"
	ownTenantPath = "/api/v1/tenant"
	// tenantAdminLevel is the command level of the tenant and plan commands (ADR 0029).
	tenantAdminLevel = 8
)

// enableTenantAdmin raises the command level so that the tenant and plan
// commands and the limits are active.
func enableTenantAdmin(t *testing.T, e *env) {
	t.Helper()
	ctx := context.Background()
	if err := e.state.Update(ctx, func(tx store.StateTx) error {
		return tx.SetCommandLevel(ctx, tenantAdminLevel)
	}); err != nil {
		t.Fatal(err)
	}
}

// putPlan creates or replaces a plan as platform admin and fails on any other status.
func putPlan(t *testing.T, e *env, id string, body map[string]any) gen.Plan {
	t.Helper()
	res := e.do("PUT", plansPath+"/"+id, platformAdmin, body)
	if res.code != http.StatusOK {
		t.Fatalf("put plan %s = %d %s", id, res.code, res.body)
	}
	return decode[gen.Plan](t, res)
}

// createTenant creates a tenant as platform admin and fails on any other status.
func createTenant(t *testing.T, e *env, body map[string]any) gen.Tenant {
	t.Helper()
	res := e.do("POST", tenantsPath, platformAdmin, body)
	if res.code != http.StatusCreated {
		t.Fatalf("create tenant = %d %s", res.code, res.body)
	}
	return decode[gen.Tenant](t, res)
}

// principalFor builds the header principal of a role in a tenant.
func principalFor(tenantID, role string) string { return tenantID + ":" + role }

// TestTenantAdminRequiresPlatformAdmin: every admin operation answers 401 to
// anonymous callers and 403 to everyone below platform admin, tenant admins
// included (ADR 0029). /tenant is open to every role.
func TestTenantAdminRequiresPlatformAdmin(t *testing.T) {
	e := newEnv(t)
	enableTenantAdmin(t, e)
	missing := uuid.NewString()
	// Invalid bodies keep the platform-admin calls free of side effects.
	ops := []struct {
		name, method, path string
		body               any
	}{
		{"list plans", "GET", plansPath, nil},
		{"get plan", "GET", plansPath + "/" + missing, nil},
		{"put plan", "PUT", plansPath + "/" + missing, map[string]any{"name": "", "limits": map[string]any{}}},
		{"delete plan", "DELETE", plansPath + "/" + missing, nil},
		{"list tenants", "GET", tenantsPath, nil},
		{"create tenant", "POST", tenantsPath, map[string]any{"name": ""}},
		{"get tenant", "GET", tenantsPath + "/" + missing, nil},
		{"update tenant", "PATCH", tenantsPath + "/" + missing, map[string]any{"name": ""}},
		{"delete tenant", "DELETE", tenantsPath + "/" + missing, map[string]any{"confirmName": ""}},
		{"suspend tenant", "POST", tenantsPath + "/" + missing + "/suspend", nil},
		{"resume tenant", "POST", tenantsPath + "/" + missing + "/resume", nil},
	}
	for _, o := range ops {
		if res := e.do(o.method, o.path, "", o.body); res.code != http.StatusUnauthorized {
			t.Errorf("%s: anonymous = %d, want 401", o.name, res.code)
		}
		for _, role := range []string{"tenant-admin", "tenant-editor", "tenant-viewer"} {
			if res := e.do(o.method, o.path, principalFor("default", role), o.body); res.code != http.StatusForbidden {
				t.Errorf("%s as %s = %d, want 403: %s", o.name, role, res.code, res.body)
			}
		}
		if res := e.do(o.method, o.path, platformAdmin, o.body); res.code == http.StatusForbidden || res.code == http.StatusUnauthorized {
			t.Errorf("%s as platform-admin = %d: %s", o.name, res.code, res.body)
		}
	}

	for _, role := range []string{"tenant-admin", "tenant-editor", "tenant-viewer", "platform-admin"} {
		if res := e.do("GET", ownTenantPath, principalFor("default", role), nil); res.code != http.StatusOK {
			t.Errorf("GET /tenant as %s = %d, want 200: %s", role, res.code, res.body)
		}
	}
}

// TestTenantLifecycle: create, patch, suspend, delete with the name
// confirmation, and the 404 afterwards. The ID of a deleted tenant is never reused.
func TestTenantLifecycle(t *testing.T) {
	e := newEnv(t)
	enableTenantAdmin(t, e)

	created := createTenant(t, e, map[string]any{"name": "Acme", "id": "acme"})
	if created.Id != "acme" || created.Plan != "default" || created.Source != gen.Admin || created.Suspended {
		t.Errorf("created = %+v", created)
	}
	if created.Limits == nil || len(created.Limits) != 0 {
		t.Errorf("limits = %v, want empty object", created.Limits)
	}
	if created.Usage.Monitors != 0 || created.OrgId != nil {
		t.Errorf("usage = %+v, orgId = %v", created.Usage, created.OrgId)
	}

	if res := e.do("POST", tenantsPath, platformAdmin, map[string]any{"name": "Again", "id": "acme"}); res.code != http.StatusConflict {
		t.Errorf("duplicate ID = %d, want 409: %s", res.code, res.body)
	}

	patched := decode[gen.Tenant](t, e.do("PATCH", tenantsPath+"/acme", platformAdmin,
		map[string]any{"name": "Acme GmbH", "limits": map[string]any{"maxMonitors": 5, "maxChannels": nil}}))
	if patched.Name != "Acme GmbH" || patched.EffectiveLimits.MaxMonitors == nil || *patched.EffectiveLimits.MaxMonitors != 5 {
		t.Errorf("patched = %+v", patched)
	}
	if v, ok := patched.Limits["maxChannels"]; !ok || v != nil {
		t.Errorf("maxChannels override = %v, %v; want kept null", v, ok)
	}

	if res := e.do("DELETE", tenantsPath+"/acme", platformAdmin, map[string]any{"confirmName": "Acme GmbH"}); res.code != http.StatusConflict {
		t.Errorf("delete before suspend = %d, want 409: %s", res.code, res.body)
	}
	if res := e.do("POST", tenantsPath+"/acme/suspend", platformAdmin, nil); res.code != http.StatusOK {
		t.Fatalf("suspend = %d %s", res.code, res.body)
	}
	if got := decode[gen.Tenant](t, e.do("GET", tenantsPath+"/acme", platformAdmin, nil)); !got.Suspended {
		t.Error("tenant not suspended after suspend")
	}
	if res := e.do("DELETE", tenantsPath+"/acme", platformAdmin, map[string]any{"confirmName": "acme"}); res.code != http.StatusUnprocessableEntity {
		t.Errorf("wrong confirmName = %d, want 422: %s", res.code, res.body)
	}
	if res := e.do("DELETE", tenantsPath+"/acme", platformAdmin, map[string]any{"confirmName": "Acme GmbH"}); res.code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", res.code, res.body)
	}
	if res := e.do("GET", tenantsPath+"/acme", platformAdmin, nil); res.code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", res.code)
	}
	if res := e.do("DELETE", tenantsPath+"/acme", platformAdmin, map[string]any{"confirmName": "Acme GmbH"}); res.code != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", res.code)
	}
	if res := e.do("POST", tenantsPath, platformAdmin, map[string]any{"name": "Reborn", "id": "acme"}); res.code != http.StatusConflict {
		t.Errorf("reuse of deleted ID = %d, want 409: %s", res.code, res.body)
	}
}

// TestTenantCreateIsIdempotent: a retry with the same Idempotency-Key returns
// the tenant of the first attempt.
func TestTenantCreateIsIdempotent(t *testing.T) {
	e := newEnv(t)
	enableTenantAdmin(t, e)
	key := uuid.NewString()
	first := e.do("POST", tenantsPath, platformAdmin, map[string]any{"name": "Retry"}, "Idempotency-Key", key)
	second := e.do("POST", tenantsPath, platformAdmin, map[string]any{"name": "Retry"}, "Idempotency-Key", key)
	if first.code != http.StatusCreated || second.code != http.StatusCreated {
		t.Fatalf("create = %d and %d, want 201: %s / %s", first.code, second.code, first.body, second.body)
	}
	if a, b := decode[gen.Tenant](t, first).Id, decode[gen.Tenant](t, second).Id; a != b {
		t.Errorf("retry created %s, first attempt %s", b, a)
	}
}

// TestTenantValidation: field errors come back as 422 before anything is proposed.
func TestTenantValidation(t *testing.T) {
	e := newEnv(t)
	enableTenantAdmin(t, e)
	tests := []struct {
		name   string
		method string
		path   string
		body   map[string]any
		field  string
	}{
		{"empty name", "POST", tenantsPath, map[string]any{"name": ""}, "/name"},
		{"bad id", "POST", tenantsPath, map[string]any{"name": "x", "id": "_system"}, "/id"},
		{"unknown plan", "POST", tenantsPath, map[string]any{"name": "x", "plan": "nope"}, "/plan"},
		{"system plan", "POST", tenantsPath, map[string]any{"name": "x", "plan": "system"}, "/plan"},
		{"bad org", "POST", tenantsPath, map[string]any{"name": "x", "orgId": "bad org"}, "/orgId"},
		{"unknown limit", "POST", tenantsPath, map[string]any{"name": "x", "limits": map[string]any{"maxFoo": 1}}, "/limits"},
		{"negative limit", "POST", tenantsPath, map[string]any{"name": "x", "limits": map[string]any{"maxMonitors": -1}}, "/limits"},
		{"zero interval", "PATCH", tenantsPath + "/default", map[string]any{"limits": map[string]any{"minIntervalSeconds": 0}}, "/limits"},
		{"org of default", "PATCH", tenantsPath + "/default", map[string]any{"orgId": "acme"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := e.do(tt.method, tt.path, platformAdmin, tt.body)
			if res.code != http.StatusUnprocessableEntity {
				t.Fatalf("= %d, want 422: %s", res.code, res.body)
			}
			if tt.field != "" && !strings.Contains(string(res.body), `"field":"`+tt.field+`"`) {
				t.Errorf("field %s missing: %s", tt.field, res.body)
			}
		})
	}
}

// TestSystemPlanAndTenantAreHidden: the system plan and the system tenant are
// neither listed nor served, and the protected tenants cannot be suspended or deleted.
func TestSystemPlanAndTenantAreHidden(t *testing.T) {
	e := newEnv(t)
	enableSystemChannels(t, e)
	enableTenantAdmin(t, e)

	plans := decode[[]gen.Plan](t, e.do("GET", plansPath, platformAdmin, nil))
	for _, p := range plans {
		if p.Id == "system" {
			t.Error("system plan listed")
		}
	}
	if res := e.do("GET", plansPath+"/system", platformAdmin, nil); res.code != http.StatusNotFound {
		t.Errorf("get system plan = %d, want 404", res.code)
	}
	if res := e.do("PUT", plansPath+"/system", platformAdmin, map[string]any{"name": "x", "limits": map[string]any{}}); res.code != http.StatusUnprocessableEntity {
		t.Errorf("put system plan = %d, want 422: %s", res.code, res.body)
	}

	tenants := decode[[]gen.Tenant](t, e.do("GET", tenantsPath, platformAdmin, nil))
	for _, tn := range tenants {
		if tn.Id == store.SystemTenantID {
			t.Error("system tenant listed")
		}
	}
	if !slices.ContainsFunc(tenants, func(tn gen.Tenant) bool { return tn.Id == "default" }) {
		t.Error("default tenant not listed")
	}
	for _, method := range []string{"GET", "PATCH", "DELETE"} {
		if res := e.do(method, tenantsPath+"/_system", platformAdmin, map[string]any{"name": "System", "confirmName": "System"}); res.code != http.StatusNotFound {
			t.Errorf("%s _system = %d, want 404: %s", method, res.code, res.body)
		}
	}
	if res := e.do("POST", tenantsPath+"/_system/suspend", platformAdmin, nil); res.code != http.StatusUnprocessableEntity {
		t.Errorf("suspend _system = %d, want 422", res.code)
	}
	if res := e.do("POST", tenantsPath+"/default/suspend", platformAdmin, nil); res.code != http.StatusUnprocessableEntity {
		t.Errorf("suspend default = %d, want 422", res.code)
	}
	if res := e.do("DELETE", tenantsPath+"/default", platformAdmin, map[string]any{"confirmName": "Default"}); res.code != http.StatusUnprocessableEntity {
		t.Errorf("delete default = %d, want 422: %s", res.code, res.body)
	}
}

// TestPlanInUseCannotBeDeleted: a plan used by a tenant answers 409; the
// default plan cannot be deleted; a freed plan can.
func TestPlanInUseCannotBeDeleted(t *testing.T) {
	e := newEnv(t)
	enableTenantAdmin(t, e)

	p := putPlan(t, e, "starter", map[string]any{"name": "Starter", "limits": map[string]any{"maxMonitors": 1}})
	if p.TenantCount != 0 || p.Limits.MaxMonitors == nil || *p.Limits.MaxMonitors != 1 {
		t.Errorf("plan = %+v", p)
	}
	tn := createTenant(t, e, map[string]any{"name": "Starter Co", "plan": "starter"})
	if got := decode[gen.Plan](t, e.do("GET", plansPath+"/starter", platformAdmin, nil)); got.TenantCount != 1 {
		t.Errorf("tenantCount = %d, want 1", got.TenantCount)
	}
	if res := e.do("DELETE", plansPath+"/starter", platformAdmin, nil); res.code != http.StatusConflict {
		t.Errorf("delete used plan = %d, want 409: %s", res.code, res.body)
	}
	if res := e.do("DELETE", plansPath+"/default", platformAdmin, nil); res.code != http.StatusUnprocessableEntity {
		t.Errorf("delete default plan = %d, want 422", res.code)
	}
	if res := e.do("PATCH", tenantsPath+"/"+tn.Id, platformAdmin, map[string]any{"plan": "default"}); res.code != http.StatusOK {
		t.Fatalf("move tenant = %d %s", res.code, res.body)
	}
	if res := e.do("DELETE", plansPath+"/starter", platformAdmin, nil); res.code != http.StatusNoContent {
		t.Errorf("delete freed plan = %d, want 204: %s", res.code, res.body)
	}
	if res := e.do("GET", plansPath+"/starter", platformAdmin, nil); res.code != http.StatusNotFound {
		t.Errorf("get deleted plan = %d, want 404", res.code)
	}
}

// TestPlanValidation: name, ID and limit values are checked before proposing.
func TestPlanValidation(t *testing.T) {
	e := newEnv(t)
	enableTenantAdmin(t, e)
	tests := []struct {
		name, id, field string
		body            map[string]any
	}{
		{"empty name", "starter", "/name", map[string]any{"name": "", "limits": map[string]any{}}},
		{"bad id", "Bad_ID", "/planId", map[string]any{"name": "x", "limits": map[string]any{}}},
		{"negative monitors", "starter", "/limits", map[string]any{"name": "x", "limits": map[string]any{"maxMonitors": -1}}},
		{"zero interval", "starter", "/limits", map[string]any{"name": "x", "limits": map[string]any{"minIntervalSeconds": 0}}},
		{"too large", "starter", "/limits", map[string]any{"name": "x", "limits": map[string]any{"maxChannels": 1 << 40}}},
		// A typo must not silently mean "unlimited" (ADR 0029).
		{"unknown limit", "starter", "/limits", map[string]any{"name": "x", "limits": map[string]any{"maxMonitor": 5}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := e.do("PUT", plansPath+"/"+tt.id, platformAdmin, tt.body)
			if res.code != http.StatusUnprocessableEntity || !strings.Contains(string(res.body), `"field":"`+tt.field+`"`) {
				t.Fatalf("= %d, want 422 at %s: %s", res.code, tt.field, res.body)
			}
		})
	}
	if res := e.do("GET", plansPath+"/starter", platformAdmin, nil); res.code != http.StatusNotFound {
		t.Errorf("rejected plan was stored: %d", res.code)
	}
}

// TestMonitorLimitEnforcement: maxMonitors and minIntervalSeconds of the
// tenant's plan are enforced on create, update and clone with 403 and the message.
func TestMonitorLimitEnforcement(t *testing.T) {
	e := newEnv(t)
	enableTenantAdmin(t, e)
	putPlan(t, e, "starter", map[string]any{"name": "Starter",
		"limits": map[string]any{"maxMonitors": 1, "minIntervalSeconds": 120}})
	tn := createTenant(t, e, map[string]any{"name": "Limited", "plan": "starter"})
	editor := principalFor(tn.Id, "tenant-editor")

	body := func(name string, interval int) map[string]any {
		return map[string]any{"name": name, "type": "http", "intervalSeconds": interval,
			"config": map[string]any{"url": "https://example.com/" + name}}
	}
	res := e.do("POST", "/api/v1/monitors", editor, body("short", 60))
	if res.code != http.StatusForbidden || !strings.Contains(string(res.body), "interval too short") {
		t.Fatalf("short interval = %d, want 403: %s", res.code, res.body)
	}
	first := e.do("POST", "/api/v1/monitors", editor, body("first", 120))
	if first.code != http.StatusCreated {
		t.Fatalf("first monitor = %d %s", first.code, first.body)
	}
	second := e.do("POST", "/api/v1/monitors", editor, body("second", 120))
	if second.code != http.StatusForbidden {
		t.Fatalf("second monitor = %d, want 403: %s", second.code, second.body)
	}
	const want = `monitor limit reached: plan \"starter\" allows 1 monitors`
	if !strings.Contains(string(second.body), want) {
		t.Errorf("message = %s, want it to contain %s", second.body, want)
	}
	var problem gen.Problem
	if err := json.Unmarshal(second.body, &problem); err != nil || problem.Title != "Forbidden" {
		t.Errorf("problem = %+v, %v; want title Forbidden", problem, err)
	}

	// Cloning goes through the same check.
	firstID := decode[gen.Monitor](t, first).Id.String()
	clone := e.do("POST", "/api/v1/monitors/"+firstID+"/clone", editor, nil)
	if clone.code != http.StatusForbidden {
		t.Errorf("clone = %d, want 403: %s", clone.code, clone.body)
	}

	// Updating the first monitor below the minimum interval is rejected too,
	// but keeping the interval is allowed.
	fullBody := func(name string, interval int) map[string]any {
		return map[string]any{"name": name, "type": "http", "intervalSeconds": interval,
			"config": map[string]any{"url": "https://example.com/first"}}
	}
	if res := e.do("PUT", "/api/v1/monitors/"+firstID, editor, fullBody("renamed", 60)); res.code != http.StatusForbidden {
		t.Errorf("update to short interval = %d, want 403: %s", res.code, res.body)
	}
	if res := e.do("PUT", "/api/v1/monitors/"+firstID, editor, fullBody("renamed", 120)); res.code != http.StatusOK {
		t.Errorf("update keeping interval = %d, want 200: %s", res.code, res.body)
	}
}

// TestChannelLimitEnforcement: maxChannels is enforced on channel creation.
func TestChannelLimitEnforcement(t *testing.T) {
	e := newEnv(t)
	enableTenantAdmin(t, e)
	putPlan(t, e, "chat", map[string]any{"name": "Chat", "limits": map[string]any{"maxChannels": 1}})
	tn := createTenant(t, e, map[string]any{"name": "Chatty", "plan": "chat"})
	admin := principalFor(tn.Id, "tenant-admin")

	body := func(name string) map[string]any {
		return map[string]any{"name": name, "kind": "shoutrrr", "url": shoutrrrURL}
	}
	if res := e.do("POST", channelsPath, admin, body("one")); res.code != http.StatusCreated {
		t.Fatalf("first channel = %d %s", res.code, res.body)
	}
	res := e.do("POST", channelsPath, admin, body("two"))
	if res.code != http.StatusForbidden || !strings.Contains(string(res.body), "notification channel limit reached") {
		t.Errorf("second channel = %d, want 403: %s", res.code, res.body)
	}
}

// TestOwnTenantShowsUsageAndExceeded: after a plan is lowered below the stock,
// GET /tenant reports the usage and the exceeded limit, and new monitors are rejected.
func TestOwnTenantShowsUsageAndExceeded(t *testing.T) {
	e := newEnv(t)
	enableTenantAdmin(t, e)
	putPlan(t, e, "roomy", map[string]any{"name": "Roomy", "limits": map[string]any{"maxMonitors": 5}})
	tn := createTenant(t, e, map[string]any{"name": "Growing", "plan": "roomy"})
	admin := principalFor(tn.Id, "tenant-admin")
	editor := principalFor(tn.Id, "tenant-editor")

	for _, name := range []string{"a", "b"} {
		res := e.do("POST", "/api/v1/monitors", editor, map[string]any{"name": name, "type": "http",
			"intervalSeconds": 60, "config": map[string]any{"url": "https://example.com/" + name}})
		if res.code != http.StatusCreated {
			t.Fatalf("monitor %s = %d %s", name, res.code, res.body)
		}
	}
	own := decode[gen.OwnTenant](t, e.do("GET", ownTenantPath, admin, nil))
	if own.Usage.Monitors != 2 || own.Usage.MinIntervalSeconds == nil || *own.Usage.MinIntervalSeconds != 60 {
		t.Errorf("usage = %+v", own.Usage)
	}
	if len(own.Exceeded) != 0 {
		t.Errorf("exceeded = %+v before lowering the plan", own.Exceeded)
	}

	putPlan(t, e, "roomy", map[string]any{"name": "Roomy", "limits": map[string]any{"maxMonitors": 1}})
	own = decode[gen.OwnTenant](t, e.do("GET", ownTenantPath, admin, nil))
	if len(own.Exceeded) != 1 || own.Exceeded[0].Limit != gen.MaxMonitors ||
		own.Exceeded[0].Allowed != 1 || own.Exceeded[0].Actual != 2 {
		t.Errorf("exceeded = %+v, want maxMonitors 2 of 1", own.Exceeded)
	}
	if own.EffectiveLimits.MaxMonitors == nil || *own.EffectiveLimits.MaxMonitors != 1 {
		t.Errorf("effectiveLimits = %+v", own.EffectiveLimits)
	}
	// The stock stays; only new monitors are rejected (ADR 0029, 3A).
	if res := e.do("POST", "/api/v1/monitors", editor, map[string]any{"name": "c", "type": "http",
		"intervalSeconds": 60, "config": map[string]any{"url": "https://example.com/c"}}); res.code != http.StatusForbidden {
		t.Errorf("new monitor over the lowered limit = %d, want 403: %s", res.code, res.body)
	}

	// The admin view shows the same overage.
	adminView := decode[gen.Tenant](t, e.do("GET", tenantsPath+"/"+tn.Id, platformAdmin, nil))
	if len(adminView.Exceeded) != 1 || adminView.Usage.Monitors != 2 {
		t.Errorf("admin view = %+v", adminView)
	}
}

// TestOwnTenantIsOnlyOwnTenant: /tenant always answers with the caller's tenant
// and hides the organization, the overrides and the source.
func TestOwnTenantIsOnlyOwnTenant(t *testing.T) {
	e := newEnv(t)
	enableTenantAdmin(t, e)
	tn := createTenant(t, e, map[string]any{"name": "Mine", "limits": map[string]any{"maxMonitors": 3}})
	admin := principalFor(tn.Id, "tenant-admin")

	res := e.do("GET", ownTenantPath+"?tenantId=tenant-b", admin, nil)
	if res.code != http.StatusOK {
		t.Fatalf("GET /tenant = %d %s", res.code, res.body)
	}
	if got := decode[gen.OwnTenant](t, res); got.Id != tn.Id || got.Name != "Mine" {
		t.Errorf("own tenant = %+v, want %s", got, tn.Id)
	}
	for _, hidden := range []string{`"orgId"`, `"limits":`, `"source"`, `"suspended"`} {
		if strings.Contains(string(res.body), hidden) {
			t.Errorf("own tenant exposes %s: %s", hidden, res.body)
		}
	}
	if got := decode[gen.OwnTenant](t, e.do("GET", ownTenantPath, principalFor("tenant-b", "tenant-admin"), nil)); got.Id != "tenant-b" {
		t.Errorf("tenant-b admin sees %s", got.Id)
	}
}
