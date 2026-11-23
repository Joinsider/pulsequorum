// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/pulsequorum/internal/api/gen"
)

const (
	maintenancePath = "/api/v1/maintenance"
	// unknownMonitorID is a well-formed UUID that no tenant owns.
	unknownMonitorID = "00000000-0000-7000-8000-000000000001"
)

// createMaintenance creates a window as principal and fails the test on any other status.
func createMaintenance(t *testing.T, e *env, principal string, body map[string]any) gen.MaintenanceWindow {
	t.Helper()
	res := e.do("POST", maintenancePath, principal, body)
	if res.code != http.StatusCreated {
		t.Fatalf("create maintenance = %d %s", res.code, res.body)
	}
	return decode[gen.MaintenanceWindow](t, res)
}

// createMonitorAs creates an HTTP monitor as principal and returns its ID.
func createMonitorAs(t *testing.T, e *env, principal, name string) string {
	t.Helper()
	res := e.do("POST", "/api/v1/monitors", principal, httpMonitor(name, "https://example.com"))
	if res.code != http.StatusCreated {
		t.Fatalf("create monitor = %d %s", res.code, res.body)
	}
	return decode[gen.Monitor](t, res).Id.String()
}

func windowBody(name string, monitorIDs []string, schedule map[string]any) map[string]any {
	if monitorIDs == nil {
		monitorIDs = []string{}
	}
	return map[string]any{"name": name, "monitorIds": monitorIDs, "schedule": schedule}
}

func onceSchedule(start, end time.Time) map[string]any {
	return map[string]any{"kind": "once", "startsAt": start, "endsAt": end}
}

func nightlySchedule() map[string]any {
	return map[string]any{"kind": "recurring", "cron": "0 3 * * *", "durationMinutes": 60, "timezone": "Europe/Berlin"}
}

// problemFields returns the field errors of a problem response, keyed by field.
func problemFields(t *testing.T, res response) map[string]string {
	t.Helper()
	p := decode[gen.Problem](t, res)
	out := map[string]string{}
	if p.Errors != nil {
		for _, f := range *p.Errors {
			out[f.Field] = f.Message
		}
	}
	return out
}

func boolValue(p *bool) bool { return p != nil && *p }

func TestMaintenanceOnceWindowRoundTrip(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	mon := createMonitorAs(t, e, editor, "web")
	start := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)

	e.now = start.Add(-time.Hour)
	created := createMaintenance(t, e, editor, map[string]any{
		"name": "  Deploy  ", "description": "rollout", "monitorIds": []string{mon},
		"schedule": onceSchedule(start, end),
	})
	if created.Name != "Deploy" || created.Description == nil || *created.Description != "rollout" {
		t.Errorf("name/description = %q %v", created.Name, created.Description)
	}
	if !boolValue(created.Active) {
		t.Errorf("active defaults to true, got %v", created.Active)
	}
	if len(created.MonitorIds) != 1 || created.MonitorIds[0].String() != mon {
		t.Errorf("monitorIds = %v, want [%s]", created.MonitorIds, mon)
	}
	if created.Schedule.Kind != gen.Once || !created.Schedule.StartsAt.Equal(start) || !created.Schedule.EndsAt.Equal(end) {
		t.Errorf("schedule = %+v", created.Schedule)
	}
	if boolValue(created.InProgress) {
		t.Errorf("inProgress before the window = true")
	}
	if created.NextStart == nil || !created.NextStart.Equal(start) {
		t.Errorf("nextStart before the window = %v, want %v", created.NextStart, start)
	}

	id := created.Id.String()
	get := func(at time.Time) gen.MaintenanceWindow {
		t.Helper()
		e.now = at
		return decode[gen.MaintenanceWindow](t, e.do("GET", maintenancePath+"/"+id, viewer, nil))
	}
	if inside := get(start.Add(30 * time.Minute)); !boolValue(inside.InProgress) || inside.NextStart != nil {
		t.Errorf("inside: inProgress=%v nextStart=%v, want true and nil", inside.InProgress, inside.NextStart)
	}
	if after := get(end.Add(time.Minute)); boolValue(after.InProgress) || after.NextStart != nil {
		t.Errorf("after: inProgress=%v nextStart=%v, want false and nil", after.InProgress, after.NextStart)
	}
}

func TestMaintenanceRecurringNextStart(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.now = time.Date(2026, 10, 21, 0, 30, 0, 0, time.UTC)
	w := createMaintenance(t, e, editor, windowBody("nightly", nil, nightlySchedule()))

	// 21 October is still CEST, so 03:00 Berlin is 01:00 UTC.
	if w.NextStart == nil || !w.NextStart.Equal(time.Date(2026, 10, 21, 1, 0, 0, 0, time.UTC)) {
		t.Errorf("nextStart = %v, want 2026-10-21T01:00Z", w.NextStart)
	}
	if boolValue(w.InProgress) {
		t.Errorf("inProgress before the first start = true")
	}
	if w.Schedule.Kind != gen.Recurring || w.Schedule.Cron == nil || *w.Schedule.Cron != "0 3 * * *" ||
		w.Schedule.DurationMinutes == nil || *w.Schedule.DurationMinutes != 60 ||
		w.Schedule.Timezone == nil || *w.Schedule.Timezone != "Europe/Berlin" {
		t.Errorf("schedule = %+v", w.Schedule)
	}

	id := w.Id.String()
	get := func(at time.Time) gen.MaintenanceWindow {
		t.Helper()
		e.now = at
		return decode[gen.MaintenanceWindow](t, e.do("GET", maintenancePath+"/"+id, viewer, nil))
	}
	if in := get(time.Date(2026, 10, 21, 1, 30, 0, 0, time.UTC)); !boolValue(in.InProgress) ||
		in.NextStart == nil || !in.NextStart.Equal(time.Date(2026, 10, 22, 1, 0, 0, 0, time.UTC)) {
		t.Errorf("inside 01:30Z: inProgress=%v nextStart=%v", in.InProgress, in.NextStart)
	}
	if out := get(time.Date(2026, 10, 21, 2, 0, 0, 0, time.UTC)); boolValue(out.InProgress) {
		t.Errorf("at 02:00Z the window must have ended, inProgress = true")
	}
	// After the switch to CET (25 October) 03:00 Berlin is 02:00 UTC.
	if after := get(time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)); after.NextStart == nil ||
		!after.NextStart.Equal(time.Date(2026, 10, 26, 2, 0, 0, 0, time.UTC)) {
		t.Errorf("nextStart after DST switch = %v, want 2026-10-26T02:00Z", after.NextStart)
	}
}

func TestMaintenanceEmptyMonitorListMeansAll(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	res := e.do("POST", maintenancePath, editor, windowBody("all", []string{}, nightlySchedule()))
	if res.code != http.StatusCreated {
		t.Fatalf("create = %d %s", res.code, res.body)
	}
	if !strings.Contains(string(res.body), `"monitorIds":[]`) {
		t.Errorf("monitorIds must be an empty array, body: %s", res.body)
	}
}

func TestMaintenanceListGetUpdateDelete(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.now = time.Date(2026, 10, 21, 1, 30, 0, 0, time.UTC)
	mon := createMonitorAs(t, e, editor, "web")
	first := createMaintenance(t, e, editor, windowBody("nightly", nil, nightlySchedule()))
	createMaintenance(t, e, editor, windowBody("second", nil, nightlySchedule()))
	id := first.Id.String()
	path := maintenancePath + "/" + id

	if n := len(decode[[]gen.MaintenanceWindow](t, e.do("GET", maintenancePath, viewer, nil))); n != 2 {
		t.Errorf("list has %d windows, want 2", n)
	}
	if got := decode[gen.MaintenanceWindow](t, e.do("GET", path, viewer, nil)); got.Name != "nightly" {
		t.Errorf("get name = %q", got.Name)
	}

	// Update: rename, add a monitor and deactivate. The window is in progress
	// at 01:30 UTC, but inactive windows are neither in progress nor scheduled.
	renamed := windowBody("renamed", []string{mon}, nightlySchedule())
	renamed["active"] = false
	res := e.do("PUT", path, editor, renamed)
	if res.code != http.StatusOK {
		t.Fatalf("update = %d %s", res.code, res.body)
	}
	upd := decode[gen.MaintenanceWindow](t, res)
	if upd.Name != "renamed" || boolValue(upd.Active) || boolValue(upd.InProgress) || upd.NextStart != nil || len(upd.MonitorIds) != 1 {
		t.Errorf("deactivated window = %+v", upd)
	}

	// A second update reads the new revision and succeeds again.
	again := windowBody("renamed again", []string{mon}, nightlySchedule())
	if res := e.do("PUT", path, editor, again); res.code != http.StatusOK {
		t.Fatalf("second update = %d %s", res.code, res.body)
	}
	if got := decode[gen.MaintenanceWindow](t, e.do("GET", path, viewer, nil)); got.Name != "renamed again" || !boolValue(got.Active) {
		t.Errorf("after second update = %+v", got)
	}

	// An invalid update is rejected and leaves the window unchanged.
	if res := e.do("PUT", path, editor, windowBody("", nil, nightlySchedule())); res.code != http.StatusUnprocessableEntity {
		t.Errorf("invalid update = %d, want 422", res.code)
	}
	if got := decode[gen.MaintenanceWindow](t, e.do("GET", path, viewer, nil)); got.Name != "renamed again" {
		t.Errorf("window changed by invalid update: %q", got.Name)
	}

	if res := e.do("DELETE", path, editor, nil); res.code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", res.code, res.body)
	}
	if res := e.do("GET", path, viewer, nil); res.code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", res.code)
	}
	if res := e.do("DELETE", path, editor, nil); res.code != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", res.code)
	}
	if res := e.do("GET", maintenancePath+"/"+uuid.NewString(), viewer, nil); res.code != http.StatusNotFound {
		t.Errorf("get unknown = %d, want 404", res.code)
	}
	if n := len(decode[[]gen.MaintenanceWindow](t, e.do("GET", maintenancePath, viewer, nil))); n != 1 {
		t.Errorf("list after delete has %d windows, want 1", n)
	}
}

func TestMaintenanceCreateIsIdempotent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	body := windowBody("once", nil, onceSchedule(time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC), time.Date(2026, 10, 21, 11, 0, 0, 0, time.UTC)))
	key := uuid.NewString()
	first := createMaintenance(t, e, editor, body)
	res := e.do("POST", maintenancePath, editor, body, "Idempotency-Key", key)
	second := decode[gen.MaintenanceWindow](t, res)
	res = e.do("POST", maintenancePath, editor, body, "Idempotency-Key", key)
	third := decode[gen.MaintenanceWindow](t, res)
	if res.code != http.StatusCreated || second.Id != third.Id {
		t.Fatalf("retry with same key = %d %s", res.code, res.body)
	}
	if second.Id == first.Id {
		t.Errorf("keyed create reused the ID of an unkeyed create")
	}
	if n := len(decode[[]gen.MaintenanceWindow](t, e.do("GET", maintenancePath, viewer, nil))); n != 2 {
		t.Errorf("list has %d windows, want 2", n)
	}
}

func TestMaintenanceValidation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	mon := createMonitorAs(t, e, editor, "web")
	start := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	withSchedule := func(s map[string]any) map[string]any { return windowBody("ok", []string{mon}, s) }
	recurring := func(overrides map[string]any) map[string]any {
		s := nightlySchedule()
		for k, v := range overrides {
			if v == nil {
				delete(s, k)
			} else {
				s[k] = v
			}
		}
		return s
	}
	description := strings.Repeat("d", maxDescription+1)

	cases := []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"empty name", windowBody("   ", []string{mon}, onceSchedule(start, start.Add(time.Hour))), "/name"},
		{"name too long", windowBody(strings.Repeat("n", maxNameLength+1), []string{mon}, onceSchedule(start, start.Add(time.Hour))), "/name"},
		{"description too long", map[string]any{"name": "x", "description": description, "monitorIds": []string{mon}, "schedule": onceSchedule(start, start.Add(time.Hour))}, "/description"},
		{"unknown monitor", windowBody("x", []string{unknownMonitorID}, onceSchedule(start, start.Add(time.Hour))), "/monitorIds"},
		{"bad cron", withSchedule(recurring(map[string]any{"cron": "0 3 * *"})), "/schedule"},
		{"cron not a cron", withSchedule(recurring(map[string]any{"cron": "every night"})), "/schedule"},
		{"bad timezone", withSchedule(recurring(map[string]any{"timezone": "Mars/Olympus"})), "/schedule/timezone"},
		{"duration zero", withSchedule(recurring(map[string]any{"durationMinutes": 0})), "/schedule/durationMinutes"},
		{"duration too long", withSchedule(recurring(map[string]any{"durationMinutes": 10081})), "/schedule/durationMinutes"},
		{"recurring without cron", withSchedule(recurring(map[string]any{"cron": nil})), "/schedule/cron"},
		{"recurring without duration", withSchedule(recurring(map[string]any{"durationMinutes": nil})), "/schedule/durationMinutes"},
		{"recurring bounds reversed", withSchedule(recurring(map[string]any{
			"startsAt": start.Add(time.Hour), "endsAt": start})), "/schedule/endsAt"},
		{"once without endsAt", withSchedule(map[string]any{"kind": "once", "startsAt": start}), "/schedule/endsAt"},
		{"once without startsAt", withSchedule(map[string]any{"kind": "once", "endsAt": start.Add(time.Hour)}), "/schedule/startsAt"},
		{"once endsAt before startsAt", withSchedule(onceSchedule(start, start.Add(-time.Hour))), "/schedule/endsAt"},
		{"once with cron", withSchedule(map[string]any{"kind": "once", "startsAt": start, "endsAt": start.Add(time.Hour), "cron": "0 3 * * *"}), "/schedule/cron"},
		{"once with duration", withSchedule(map[string]any{"kind": "once", "startsAt": start, "endsAt": start.Add(time.Hour), "durationMinutes": 5}), "/schedule/durationMinutes"},
		{"unknown kind", withSchedule(map[string]any{"kind": "weekly"}), "/schedule/kind"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			res := e.do("POST", maintenancePath, editor, c.body)
			if res.code != http.StatusUnprocessableEntity {
				t.Fatalf("create = %d %s, want 422", res.code, res.body)
			}
			if _, ok := problemFields(t, res)[c.field]; !ok {
				t.Errorf("no field error at %s: %s", c.field, res.body)
			}
		})
	}
	if n := len(decode[[]gen.MaintenanceWindow](t, e.do("GET", maintenancePath, viewer, nil))); n != 0 {
		t.Errorf("rejected windows were stored: %d", n)
	}
}

// TestMaintenanceForeignMonitorReportedAsUnknown checks that a monitor of
// another tenant gets the same field error as a monitor that does not exist.
func TestMaintenanceForeignMonitorReportedAsUnknown(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	foreign := createMonitorAs(t, e, other, "foreign")
	start := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	for _, id := range []string{foreign, unknownMonitorID} {
		res := e.do("POST", maintenancePath, editor, windowBody("x", []string{id}, onceSchedule(start, start.Add(time.Hour))))
		if res.code != http.StatusUnprocessableEntity {
			t.Fatalf("monitor %s = %d %s, want 422", id, res.code, res.body)
		}
		if msg := problemFields(t, res)["/monitorIds"]; msg != "unknown monitor "+id {
			t.Errorf("monitor %s: message %q", id, msg)
		}
	}
	if n := len(decode[[]gen.MaintenanceWindow](t, e.do("GET", maintenancePath, viewer, nil))); n != 0 {
		t.Errorf("window with foreign monitor was stored")
	}
}

func TestMaintenanceRoles(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	mon := createMonitorAs(t, e, editor, "web")
	body := windowBody("x", []string{mon}, onceSchedule(time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC), time.Date(2026, 10, 21, 11, 0, 0, 0, time.UTC)))
	w := createMaintenance(t, e, editor, body)
	path := maintenancePath + "/" + w.Id.String()

	if res := e.do("POST", maintenancePath, viewer, body); res.code != http.StatusForbidden {
		t.Errorf("viewer create = %d, want 403", res.code)
	}
	if res := e.do("PUT", path, viewer, body); res.code != http.StatusForbidden {
		t.Errorf("viewer update = %d, want 403", res.code)
	}
	if res := e.do("DELETE", path, viewer, nil); res.code != http.StatusForbidden {
		t.Errorf("viewer delete = %d, want 403", res.code)
	}
	if res := e.do("GET", maintenancePath, viewer, nil); res.code != http.StatusOK {
		t.Errorf("viewer list = %d, want 200", res.code)
	}
	if res := e.do("GET", path, viewer, nil); res.code != http.StatusOK {
		t.Errorf("viewer get = %d, want 200", res.code)
	}
}

func TestMaintenanceTenantIsolation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	start := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	w := createMaintenance(t, e, editor, windowBody("own", nil, onceSchedule(start, start.Add(time.Hour))))
	path := maintenancePath + "/" + w.Id.String()
	body := windowBody("hijacked", nil, onceSchedule(start, start.Add(time.Hour)))

	if res := e.do("GET", path, other, nil); res.code != http.StatusNotFound {
		t.Errorf("other tenant get = %d, want 404", res.code)
	}
	if res := e.do("PUT", path, other, body); res.code != http.StatusNotFound {
		t.Errorf("other tenant update = %d, want 404", res.code)
	}
	if res := e.do("DELETE", path, other, nil); res.code != http.StatusNotFound {
		t.Errorf("other tenant delete = %d, want 404", res.code)
	}
	if n := len(decode[[]gen.MaintenanceWindow](t, e.do("GET", maintenancePath, other, nil))); n != 0 {
		t.Errorf("other tenant sees %d windows, want 0", n)
	}
	if got := decode[gen.MaintenanceWindow](t, e.do("GET", path, editor, nil)); got.Name != "own" {
		t.Errorf("window changed by other tenant: %q", got.Name)
	}
}

func TestMaintenanceRequiresAuthentication(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if res := e.do("GET", maintenancePath, "", nil); res.code != http.StatusUnauthorized {
		t.Errorf("anonymous list = %d, want 401", res.code)
	}
	body := windowBody("x", nil, onceSchedule(time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC), time.Date(2026, 10, 21, 11, 0, 0, 0, time.UTC)))
	if res := e.do("POST", maintenancePath, "", body); res.code != http.StatusUnauthorized {
		t.Errorf("anonymous create = %d, want 401", res.code)
	}
}
