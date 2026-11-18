// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package api

import (
	"encoding/base64"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/joinsider/uptimecluster/internal/api/gen"
)

const (
	auditPath      = "/api/v1/audit"
	adminAuditPath = "/api/v1/admin/audit"
)

// auditItems lists one page of the audit log as principal and fails on any status but 200.
func auditItems(t *testing.T, e *env, principal, query string) gen.AuditPage {
	t.Helper()
	res := e.do("GET", auditPath+query, principal, nil)
	if res.code != http.StatusOK {
		t.Fatalf("audit%s = %d %s", query, res.code, res.body)
	}
	return decode[gen.AuditPage](t, res)
}

// adminAuditItems lists one page of the admin audit log as principal.
func adminAuditItems(t *testing.T, e *env, principal, query string) gen.AuditPage {
	t.Helper()
	res := e.do("GET", adminAuditPath+query, principal, nil)
	if res.code != http.StatusOK {
		t.Fatalf("admin audit%s = %d %s", query, res.code, res.body)
	}
	return decode[gen.AuditPage](t, res)
}

// entriesOf returns the entries about one object, in the order of the log.
func entriesOf(page gen.AuditPage, objectID string) []gen.AuditEntry {
	var out []gen.AuditEntry
	for _, e := range page.Items {
		if e.ObjectId == objectID {
			out = append(out, e)
		}
	}
	return out
}

// actionsOf returns the actions of the entries, in order.
func actionsOf(entries []gen.AuditEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Action)
	}
	return out
}

// TestAuditRecordsWrites checks the entries of the single-object operations,
// with the actor by name and without any secret.
func TestAuditRecordsWrites(t *testing.T) {
	e, admin := newLocalAdminEnv(t)
	raiseCommandLevel(t, e, 9)
	_, editorToken := e.addUser(admin, "audit-editor", "tenant-editor")
	editor := bearer(editorToken)

	res := e.do("POST", "/api/v1/monitors", "", httpMonitor("audited", "https://example.com"), editor...)
	if res.code != http.StatusCreated {
		t.Fatalf("create = %d %s", res.code, res.body)
	}
	id := decode[gen.Monitor](t, res).Id.String()
	steps := []struct {
		method, path string
		body         any
	}{
		{"PUT", "/api/v1/monitors/" + id, httpMonitor("audited renamed", "https://example.com")},
		{"POST", "/api/v1/monitors/" + id + "/pause", nil},
		{"POST", "/api/v1/monitors/" + id + "/resume", nil},
		{"DELETE", "/api/v1/monitors/" + id, nil},
	}
	for _, s := range steps {
		if res := e.do(s.method, s.path, "", s.body, editor...); res.code >= 300 {
			t.Fatalf("%s %s = %d %s", s.method, s.path, res.code, res.body)
		}
	}
	clone := e.do("POST", "/api/v1/monitors", "", httpMonitor("source", "https://example.com"), editor...)
	srcID := decode[gen.Monitor](t, clone).Id.String()
	cloned := e.do("POST", "/api/v1/monitors/"+srcID+"/clone", "", nil, editor...)
	if cloned.code != http.StatusCreated {
		t.Fatalf("clone = %d %s", cloned.code, cloned.body)
	}
	cloneID := decode[gen.Monitor](t, cloned).Id.String()

	res = e.do("GET", auditPath+"?limit=200", "", nil, bearer(admin)...)
	if res.code != http.StatusOK {
		t.Fatalf("audit = %d %s", res.code, res.body)
	}
	page := decode[gen.AuditPage](t, res)
	got := actionsOf(entriesOf(page, id))
	want := []string{"monitor.delete", "monitor.resume", "monitor.pause", "monitor.update", "monitor.create"}
	if !slices.Equal(got, want) {
		t.Fatalf("actions of %s = %v, want %v", id, got, want)
	}
	if c := entriesOf(page, cloneID); len(c) != 1 || c[0].Action != "monitor.clone" {
		t.Fatalf("clone entries = %+v, want one monitor.clone", c)
	}
	for _, e := range entriesOf(page, id) {
		if e.Actor.Kind != gen.AuditUser || e.Actor.Name != "audit-editor" {
			t.Errorf("%s actor = %+v, want user audit-editor", e.Action, e.Actor)
		}
		if e.TenantId != "default" || e.RequestId == "" || e.ObjectKind != "monitor" {
			t.Errorf("%s entry = %+v", e.Action, e)
		}
	}
}

// TestAuditRecordsTokensWithoutSecrets: token operations are audited; the
// secret never reaches the log.
func TestAuditRecordsTokensWithoutSecrets(t *testing.T) {
	e, admin := newLocalAdminEnv(t)
	raiseCommandLevel(t, e, 9)
	created := createTokenRes(t, e, bearer(admin), "audited-token", "tenant-viewer")
	if res := e.do("DELETE", apiTokensPath+"/"+created.Id, "", nil, bearer(admin)...); res.code != http.StatusNoContent {
		t.Fatalf("revoke = %d", res.code)
	}

	res := e.do("GET", auditPath+"?limit=200", "", nil, bearer(admin)...)
	if res.code != http.StatusOK {
		t.Fatalf("audit = %d %s", res.code, res.body)
	}
	if strings.Contains(string(res.body), "uct_") || strings.Contains(string(res.body), created.Token) {
		t.Fatalf("audit log contains a secret: %s", res.body)
	}
	entries := entriesOf(decode[gen.AuditPage](t, res), created.Id)
	if got, want := actionsOf(entries), []string{"api_token.revoke", "api_token.create"}; !slices.Equal(got, want) {
		t.Fatalf("token actions = %v, want %v", got, want)
	}
	if entries[0].Actor.Kind != gen.AuditUser || entries[0].Actor.Name != "root-admin" {
		t.Errorf("revoke actor = %+v", entries[0].Actor)
	}
}

// TestAuditNotWrittenBelowLevel9 checks that the log starts with command level 9.
func TestAuditNotWrittenBelowLevel9(t *testing.T) {
	e := newEnv(t)
	raiseCommandLevel(t, e, 8)
	res := e.do("POST", "/api/v1/monitors", editor, httpMonitor("early", "https://example.com"))
	if res.code != http.StatusCreated {
		t.Fatalf("create = %d %s", res.code, res.body)
	}
	if n := len(auditItems(t, e, tenantAdmin, "").Items); n != 0 {
		t.Fatalf("%d entries below command level 9", n)
	}
}

// TestAuditTenantSeparation: a tenant admin sees only its own tenant; the
// platform admin sees all of them and the platform actions.
func TestAuditTenantSeparation(t *testing.T) {
	e := newEnv(t)
	raiseCommandLevel(t, e, 9)
	mineRes := e.do("POST", "/api/v1/monitors", editor, httpMonitor("mine", "https://example.com"))
	mine := decode[gen.Monitor](t, mineRes).Id.String()
	theirsRes := e.do("POST", "/api/v1/monitors", other, httpMonitor("theirs", "https://example.com"))
	theirs := decode[gen.Monitor](t, theirsRes).Id.String()

	defaultPage := auditItems(t, e, tenantAdmin, "?limit=200")
	if len(entriesOf(defaultPage, mine)) == 0 || len(entriesOf(defaultPage, theirs)) != 0 {
		t.Fatalf("default sees %v", actionsOf(defaultPage.Items))
	}
	for _, it := range defaultPage.Items {
		if it.TenantId != "default" {
			t.Errorf("default sees entry of tenant %s", it.TenantId)
		}
	}
	otherPage := auditItems(t, e, other, "?limit=200")
	if len(entriesOf(otherPage, theirs)) == 0 || len(entriesOf(otherPage, mine)) != 0 {
		t.Fatalf("tenant-b sees %v", actionsOf(otherPage.Items))
	}

	createTenant(t, e, map[string]any{"name": "Audit tenant"})
	all := adminAuditItems(t, e, platformAdmin, "?limit=200")
	if len(entriesOf(all, mine)) == 0 || len(entriesOf(all, theirs)) == 0 {
		t.Fatalf("platform admin misses tenant entries")
	}
	system := adminAuditItems(t, e, platformAdmin, "?tenant=_system&limit=200")
	if len(system.Items) == 0 {
		t.Fatalf("no platform entries for tenant _system")
	}
	for _, it := range system.Items {
		if it.TenantId != "_system" {
			t.Errorf("tenant=_system filter returned %s", it.TenantId)
		}
	}
	onlyB := adminAuditItems(t, e, platformAdmin, "?tenant=tenant-b&limit=200")
	for _, it := range onlyB.Items {
		if it.TenantId != "tenant-b" {
			t.Errorf("tenant=tenant-b filter returned %s", it.TenantId)
		}
	}
	if len(entriesOf(onlyB, theirs)) == 0 {
		t.Errorf("tenant=tenant-b misses its own monitor")
	}
}

// TestAuditAccess covers the roles and the query validation.
func TestAuditAccess(t *testing.T) {
	e := newEnv(t)
	raiseCommandLevel(t, e, 9)

	for _, principal := range []string{viewer, editor} {
		if res := e.do("GET", auditPath, principal, nil); res.code != http.StatusForbidden {
			t.Errorf("audit as %s = %d, want 403", principal, res.code)
		}
	}
	if res := e.do("GET", auditPath, "", nil); res.code != http.StatusUnauthorized {
		t.Errorf("anonymous audit = %d, want 401", res.code)
	}
	for _, principal := range []string{tenantAdmin, tenantEditor, viewer} {
		if res := e.do("GET", adminAuditPath, principal, nil); res.code != http.StatusForbidden {
			t.Errorf("admin audit as %s = %d, want 403", principal, res.code)
		}
	}
	if res := e.do("GET", adminAuditPath, platformAdmin, nil); res.code != http.StatusOK {
		t.Errorf("admin audit as platform admin = %d, want 200", res.code)
	}

	badTenant := e.do("GET", adminAuditPath+"?tenant=Not_Valid", platformAdmin, nil)
	if badTenant.code != http.StatusUnprocessableEntity || !slices.Contains(fieldPointers(t, badTenant), "/tenant") {
		t.Errorf("bad tenant filter = %d %s", badTenant.code, badTenant.body)
	}
	for _, q := range []string{"?limit=0", "?limit=201"} {
		res := e.do("GET", auditPath+q, tenantAdmin, nil)
		if res.code != http.StatusUnprocessableEntity || !slices.Contains(fieldPointers(t, res), "/limit") {
			t.Errorf("audit%s = %d %s, want 422 at /limit", q, res.code, res.body)
		}
	}
	malformed := []string{
		"!!!",
		base64.RawURLEncoding.EncodeToString([]byte("x:1.0")),
		base64.RawURLEncoding.EncodeToString([]byte("a:notanumber")),
		base64.RawURLEncoding.EncodeToString([]byte("a:1")),
		base64.RawURLEncoding.EncodeToString([]byte("a:1.-1")),
	}
	for _, c := range malformed {
		res := e.do("GET", auditPath+"?cursor="+c, tenantAdmin, nil)
		if res.code != http.StatusBadRequest {
			t.Errorf("cursor %q = %d, want 400: %s", c, res.code, res.body)
		}
	}
}

// TestAuditPaging walks the log page by page and filters by action and object.
func TestAuditPaging(t *testing.T) {
	e := newEnv(t)
	raiseCommandLevel(t, e, 9)
	var ids []string
	for i := range 5 {
		res := e.do("POST", "/api/v1/monitors", editor, httpMonitor("page-"+string(rune('a'+i)), "https://example.com"))
		ids = append(ids, decode[gen.Monitor](t, res).Id.String())
	}

	full := auditItems(t, e, tenantAdmin, "?limit=200").Items
	if len(full) < 5 {
		t.Fatalf("only %d entries", len(full))
	}
	var paged []gen.AuditEntry
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > len(full) {
			t.Fatal("paging does not end")
		}
		q := "?limit=2"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		page := auditItems(t, e, tenantAdmin, q)
		if len(page.Items) > 2 {
			t.Fatalf("page has %d items, want at most 2", len(page.Items))
		}
		paged = append(paged, page.Items...)
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	if len(paged) != len(full) {
		t.Fatalf("paged %d entries, full listing has %d", len(paged), len(full))
	}
	for i := range full {
		if paged[i].RequestId != full[i].RequestId || paged[i].ObjectId != full[i].ObjectId || paged[i].Action != full[i].Action {
			t.Fatalf("entry %d differs: %+v vs %+v", i, paged[i], full[i])
		}
	}

	creates := auditItems(t, e, tenantAdmin, "?action=monitor.create&limit=200")
	if len(creates.Items) != len(full) {
		t.Errorf("action filter returned %d entries, want %d", len(creates.Items), len(full))
	}
	one := auditItems(t, e, tenantAdmin, "?objectId="+ids[2]+"&limit=200")
	if len(one.Items) != 1 || one.Items[0].ObjectId != ids[2] {
		t.Errorf("objectId filter = %+v, want the one entry of %s", one.Items, ids[2])
	}
}
