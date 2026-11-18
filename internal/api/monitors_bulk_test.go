// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/joinsider/uptimecluster/internal/api/gen"
	"github.com/joinsider/uptimecluster/internal/cluster"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
)

const bulkPath = "/api/v1/monitors/bulk"

// makeMonitor creates an HTTP monitor with tags (nil = none) as principal.
func makeMonitor(t *testing.T, e *env, principal, name string, tags []string) gen.Monitor {
	t.Helper()
	body := httpMonitor(name, "https://example.com")
	if tags != nil {
		body["tags"] = tags
	}
	res := e.do("POST", "/api/v1/monitors", principal, body)
	if res.code != http.StatusCreated {
		t.Fatalf("create %s = %d %s", name, res.code, res.body)
	}
	return decode[gen.Monitor](t, res)
}

// fetchMonitor reads one monitor as the default editor.
func fetchMonitor(t *testing.T, e *env, id string) gen.Monitor {
	t.Helper()
	res := e.do("GET", "/api/v1/monitors/"+id, editor, nil)
	if res.code != http.StatusOK {
		t.Fatalf("get %s = %d %s", id, res.code, res.body)
	}
	return decode[gen.Monitor](t, res)
}

// bulkAs posts a bulk action as principal.
func bulkAs(e *env, principal string, body map[string]any) response {
	return e.do("POST", bulkPath, principal, body)
}

// idsOf returns the IDs of the monitors as strings.
func idsOf(ms ...gen.Monitor) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Id.String())
	}
	return out
}

// revisionOf returns the revision reported for one monitor, or -1 if absent.
func revisionOf(t *testing.T, res response, i int) int {
	t.Helper()
	out := decode[gen.BulkMonitorsResult](t, res)
	if i >= len(out.Results) {
		t.Fatalf("result %d missing in %s", i, res.body)
	}
	if out.Results[i].Revision == nil {
		return -1
	}
	return *out.Results[i].Revision
}

// auditCountOf counts the entries with action that the admin audit log holds for ids.
func auditCountOf(t *testing.T, e *env, action string, ids ...string) int {
	t.Helper()
	page := auditItems(t, e, tenantAdmin, "?action="+action+"&limit=200")
	n := 0
	for _, it := range page.Items {
		if slices.Contains(ids, it.ObjectId) {
			n++
		}
	}
	return n
}

// TestBulkPauseAndResume: one batch pauses or resumes all monitors, one audit
// entry each, all with the same request ID; monitors already in the state are
// unchanged.
func TestBulkPauseAndResume(t *testing.T) {
	e := newEnv(t)
	raiseCommandLevel(t, e, 9)
	m := []gen.Monitor{
		makeMonitor(t, e, editor, "b1", nil),
		makeMonitor(t, e, editor, "b2", nil),
		makeMonitor(t, e, editor, "b3", nil),
	}
	ids := idsOf(m...)

	res := bulkAs(e, editor, map[string]any{"action": "pause", "ids": ids})
	if res.code != http.StatusOK {
		t.Fatalf("pause = %d %s", res.code, res.body)
	}
	out := decode[gen.BulkMonitorsResult](t, res)
	if len(out.Results) != len(ids) {
		t.Fatalf("results = %d, want %d", len(out.Results), len(ids))
	}
	for i, r := range out.Results {
		// A monitor is created at revision 1; the pause makes it 2.
		if r.Id.String() != ids[i] || revisionOf(t, res, i) != 2 {
			t.Errorf("result %d = %+v, want %s at revision 2", i, r, ids[i])
		}
	}
	for _, id := range ids {
		if !*fetchMonitor(t, e, id).Paused {
			t.Errorf("monitor %s not paused", id)
		}
	}
	if n := auditCountOf(t, e, "monitor.pause", ids...); n != len(ids) {
		t.Errorf("monitor.pause entries = %d, want %d", n, len(ids))
	}
	// All entries of the batch share one request ID.
	requestIDs := map[string]bool{}
	for _, it := range auditItems(t, e, tenantAdmin, "?action=monitor.pause&limit=200").Items {
		if slices.Contains(ids, it.ObjectId) {
			requestIDs[it.RequestId] = true
		}
	}
	if len(requestIDs) != 1 {
		t.Errorf("pause entries have %d request IDs, want 1", len(requestIDs))
	}

	again := bulkAs(e, editor, map[string]any{"action": "pause", "ids": ids})
	if again.code != http.StatusOK || revisionOf(t, again, 0) != 2 {
		t.Errorf("pausing paused monitors = %d %s, want unchanged revision 2", again.code, again.body)
	}
	if n := auditCountOf(t, e, "monitor.pause", ids...); n != len(ids) {
		t.Errorf("no-op added audit entries: %d", n)
	}

	res = bulkAs(e, editor, map[string]any{"action": "resume", "ids": ids})
	if res.code != http.StatusOK || revisionOf(t, res, 1) != 3 {
		t.Fatalf("resume = %d %s", res.code, res.body)
	}
	for _, id := range ids {
		if *fetchMonitor(t, e, id).Paused {
			t.Errorf("monitor %s still paused", id)
		}
	}
	if n := auditCountOf(t, e, "monitor.resume", ids...); n != len(ids) {
		t.Errorf("monitor.resume entries = %d, want %d", n, len(ids))
	}
}

// TestBulkTags adds and removes tags with the rules of a single update.
func TestBulkTags(t *testing.T) {
	e := newEnv(t)
	raiseCommandLevel(t, e, 9)
	m1 := makeMonitor(t, e, editor, "t1", []string{"keep"})
	m2 := makeMonitor(t, e, editor, "t2", []string{"b"})
	m3 := makeMonitor(t, e, editor, "t3", nil)
	ids := idsOf(m1, m2, m3)

	res := bulkAs(e, editor, map[string]any{"action": "addTags", "ids": ids, "tags": []string{"a", "b"}})
	if res.code != http.StatusOK {
		t.Fatalf("addTags = %d %s", res.code, res.body)
	}
	// Tags are stored sorted, so the expectations are sorted too.
	want := map[string][]string{
		ids[0]: {"a", "b", "keep"},
		ids[1]: {"a", "b"},
		ids[2]: {"a", "b"},
	}
	for id, tags := range want {
		if got := *fetchMonitor(t, e, id).Tags; !slices.Equal(got, tags) {
			t.Errorf("tags of %s = %v, want %v", id, got, tags)
		}
	}

	res = bulkAs(e, editor, map[string]any{"action": "removeTags", "ids": ids[:2], "tags": []string{"a", "zzz"}})
	if res.code != http.StatusOK {
		t.Fatalf("removeTags = %d %s", res.code, res.body)
	}
	if got := *fetchMonitor(t, e, ids[0]).Tags; !slices.Equal(got, []string{"b", "keep"}) {
		t.Errorf("tags after remove = %v, want [b keep]", got)
	}
	if got := *fetchMonitor(t, e, ids[1]).Tags; !slices.Equal(got, []string{"b"}) {
		t.Errorf("tags after remove = %v, want [b]", got)
	}

	// Removing a tag the monitor does not have changes nothing.
	res = bulkAs(e, editor, map[string]any{"action": "removeTags", "ids": ids[2:], "tags": []string{"zzz"}})
	if res.code != http.StatusOK || revisionOf(t, res, 0) != 2 {
		t.Errorf("no-op removeTags = %d %s, want revision 2", res.code, res.body)
	}

	// Five tag changes in total, each with its own audit entry.
	if n := auditCountOf(t, e, "monitor.tags", ids...); n != 5 {
		t.Errorf("monitor.tags entries = %d, want 5", n)
	}
}

// TestBulkDelete removes monitors; deleted monitors have no revision in the result.
func TestBulkDelete(t *testing.T) {
	e := newEnv(t)
	raiseCommandLevel(t, e, 9)
	m1 := makeMonitor(t, e, editor, "d1", nil)
	m2 := makeMonitor(t, e, editor, "d2", nil)
	ids := idsOf(m1, m2)

	res := bulkAs(e, editor, map[string]any{"action": "delete", "ids": ids})
	if res.code != http.StatusOK {
		t.Fatalf("delete = %d %s", res.code, res.body)
	}
	if strings.Contains(string(res.body), `"revision"`) {
		t.Errorf("deleted monitors report a revision: %s", res.body)
	}
	for _, id := range ids {
		if r := e.do("GET", "/api/v1/monitors/"+id, editor, nil); r.code != http.StatusNotFound {
			t.Errorf("deleted monitor %s = %d, want 404", id, r.code)
		}
	}
	if n := auditCountOf(t, e, "monitor.delete", ids...); n != 2 {
		t.Errorf("monitor.delete entries = %d, want 2", n)
	}
}

// TestBulkValidation: every invalid request is answered with 422 and the
// position of the problem, and changes nothing.
func TestBulkValidation(t *testing.T) {
	e := newEnv(t)
	raiseCommandLevel(t, e, 9)
	m1 := makeMonitor(t, e, editor, "v1", nil)
	twenty := make([]string, maxTags)
	for i := range twenty {
		twenty[i] = "t" + string(rune('a'+i))
	}
	full := makeMonitor(t, e, editor, "v2", twenty)
	unknown := uuid.NewString()
	many := make([]string, 0, bulkMaxIDs+1)
	for range bulkMaxIDs + 1 {
		many = append(many, uuid.NewString())
	}
	long := strings.Repeat("x", maxTagLength+1)
	tooMany := make([]string, 0, maxTags+1)
	for i := range maxTags + 1 {
		tooMany = append(tooMany, "tag"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}

	tests := []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"no ids", map[string]any{"action": "pause", "ids": []string{}}, "/ids"},
		{"too many ids", map[string]any{"action": "pause", "ids": many}, "/ids"},
		{"unknown monitor", map[string]any{"action": "pause", "ids": []string{idsOf(m1)[0], unknown}}, "/ids/1"},
		{"duplicate monitor", map[string]any{"action": "pause", "ids": []string{idsOf(m1)[0], idsOf(m1)[0]}}, "/ids/1"},
		{"unknown action", map[string]any{"action": "archive", "ids": idsOf(m1)}, "/action"},
		{"addTags without tags", map[string]any{"action": "addTags", "ids": idsOf(m1)}, "/tags"},
		{"removeTags without tags", map[string]any{"action": "removeTags", "ids": idsOf(m1)}, "/tags"},
		{"too many tags", map[string]any{"action": "addTags", "ids": idsOf(m1), "tags": tooMany}, "/tags"},
		{"tag too long", map[string]any{"action": "addTags", "ids": idsOf(m1), "tags": []string{long}}, "/tags/0"},
		{"tags would exceed the limit", map[string]any{"action": "addTags", "ids": []string{idsOf(m1)[0], idsOf(full)[0]}, "tags": []string{"extra"}}, "/ids/1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := bulkAs(e, editor, tt.body)
			if res.code != http.StatusUnprocessableEntity {
				t.Fatalf("= %d, want 422: %s", res.code, res.body)
			}
			if fields := fieldPointers(t, res); !slices.Contains(fields, tt.field) {
				t.Fatalf("fields = %v, want %s", fields, tt.field)
			}
		})
	}
	if *fetchMonitor(t, e, idsOf(m1)[0]).Paused {
		t.Error("a rejected bulk action paused a monitor")
	}
	if n := len(*fetchMonitor(t, e, idsOf(full)[0]).Tags); n != maxTags {
		t.Errorf("full monitor has %d tags after rejected addTags", n)
	}
}

// TestBulkIsAtomicOnConflict: a monitor changed between the read and the
// batch makes the whole action fail with 409 and names the monitor.
func TestBulkIsAtomicOnConflict(t *testing.T) {
	var e *env
	var racing *racingApplier
	e = newEnvTweaked(t, false, func(d *Deps) {
		racing = &racingApplier{next: d.Cluster}
		d.Cluster = racing
	})
	raiseCommandLevel(t, e, 9)
	m1 := makeMonitor(t, e, editor, "c1", nil)
	m2 := makeMonitor(t, e, editor, "c2", nil)
	m3 := makeMonitor(t, e, editor, "c3", nil)
	ids := idsOf(m1, m2, m3)

	// Before the batch is proposed, another request renames the second monitor.
	racing.before = func() {
		res := e.do("PUT", "/api/v1/monitors/"+ids[1], editor, httpMonitor("renamed meanwhile", "https://example.com"))
		if res.code != http.StatusOK {
			t.Fatalf("concurrent update = %d %s", res.code, res.body)
		}
	}
	res := bulkAs(e, editor, map[string]any{"action": "pause", "ids": ids})
	if res.code != http.StatusConflict {
		t.Fatalf("= %d, want 409: %s", res.code, res.body)
	}
	detail := problemDetail(t, res)
	if !strings.Contains(detail, "operation 1") || !strings.Contains(detail, ids[1]) {
		t.Errorf("detail = %q, want the operation and monitor %s", detail, ids[1])
	}
	for _, id := range []string{ids[0], ids[2]} {
		if *fetchMonitor(t, e, id).Paused {
			t.Errorf("monitor %s was paused although the batch failed", id)
		}
	}
	if fetchMonitor(t, e, ids[1]).Name != "renamed meanwhile" {
		t.Errorf("concurrent update lost")
	}
}

// racingApplier runs before once, before the first batch command reaches the cluster.
type racingApplier struct {
	next   Applier
	before func()
}

// Apply implements Applier.
func (r *racingApplier) Apply(ctx context.Context, cmd *pb.Command) (cluster.ApplyResult, error) {
	if _, ok := cmd.GetPayload().(*pb.Command_Batch); ok && r.before != nil {
		hook := r.before
		r.before = nil
		hook()
	}
	return r.next.Apply(ctx, cmd)
}

// TestBulkTenantAndRoles: other tenants' monitors are unknown, viewers are refused.
func TestBulkTenantAndRoles(t *testing.T) {
	e := newEnv(t)
	raiseCommandLevel(t, e, 9)
	mine := makeMonitor(t, e, editor, "own", nil)

	res := bulkAs(e, other, map[string]any{"action": "pause", "ids": idsOf(mine)})
	if res.code != http.StatusUnprocessableEntity || !slices.Contains(fieldPointers(t, res), "/ids/0") {
		t.Errorf("other tenant = %d %s, want 422 at /ids/0", res.code, res.body)
	}
	if *fetchMonitor(t, e, idsOf(mine)[0]).Paused {
		t.Error("other tenant paused a monitor")
	}
	body := map[string]any{"action": "pause", "ids": idsOf(mine)}
	if res := bulkAs(e, viewer, body); res.code != http.StatusForbidden {
		t.Errorf("viewer = %d, want 403", res.code)
	}
	if res := e.do("POST", bulkPath, "", body); res.code != http.StatusUnauthorized {
		t.Errorf("anonymous = %d, want 401", res.code)
	}
}
