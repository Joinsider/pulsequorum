// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import (
	"errors"
	"slices"
	"testing"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
)

// TestStateEvents covers the StateEvent observer: committed status changes are
// delivered with their tenant, incident effects and nothing else.
func TestStateEvents(t *testing.T) {
	t.Parallel()
	h := newAlertFSM(t, t.TempDir())
	var got []StateEvent
	h.fsm.OnStateChange(func(ev StateEvent) { got = append(got, ev) })

	pausedDoc := monitorDoc("one")
	pausedDoc.Paused = true
	// Monitor IDs are unique across tenants, so each tenant owns its own monitor.
	monitorOf := func(tenant string) string { return map[string]string{"t1": "m1", "t2": "m2"}[tenant] }
	put := func(req, tenant string, doc MonitorDocument, rev uint64) *pb.Command {
		t.Helper()
		c, err := NewPutMonitor(req, tenant, monitorOf(tenant), doc, rev)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	change := func(tenant string, from, to store.MonitorStatus, since int64, incident string) *pb.Command {
		return NewSetMonitorState(StateChange{TenantID: tenant, MonitorID: monitorOf(tenant), From: from, To: to, SinceMs: since,
			Incident: incidentOf(incident)})
	}
	tenant := func(id string) *pb.Command {
		t.Helper()
		c, err := NewPutTenant("tenant-"+id, id, TenantDocument{Name: id, Plan: "default"})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	steps := []struct {
		name    string
		cmd     *pb.Command
		issued  int64
		wantErr error
		want    []StateEvent
	}{
		{"tenant t1", tenant("t1"), 1000, nil, nil},
		{"tenant t2", tenant("t2"), 1000, nil, nil},
		{"create m1 in t1", put("create-t1", "t1", monitorDoc("one"), 0), 1001, nil,
			[]StateEvent{{TenantID: "t1", MonitorID: "m1", Status: store.StatusPending, Confirmed: true, SinceMs: 1001}}},
		{"create m2 in t2", put("create-t2", "t2", monitorDoc("one"), 0), 1002, nil,
			[]StateEvent{{TenantID: "t2", MonitorID: "m2", Status: store.StatusPending, Confirmed: true, SinceMs: 1002}}},
		{"t1 to DOWN", change("t1", store.StatusPending, store.StatusDown, 2000, "i1"), 2100, nil,
			[]StateEvent{{TenantID: "t1", MonitorID: "m1", Status: store.StatusDown, Confirmed: true, SinceMs: 2000, IncidentOpened: "i1"}}},
		{"stale change is rejected", change("t1", store.StatusPending, store.StatusUp, 2200, ""), 2200, store.ErrConflict, nil},
		{"t1 back to UP", change("t1", store.StatusDown, store.StatusUp, 3000, ""), 3100, nil,
			[]StateEvent{{TenantID: "t1", MonitorID: "m1", Status: store.StatusUp, Confirmed: true, SinceMs: 3000, IncidentResolved: true}}},
		{"t1 DOWN again", change("t1", store.StatusUp, store.StatusDown, 4000, "i2"), 4100, nil,
			[]StateEvent{{TenantID: "t1", MonitorID: "m1", Status: store.StatusDown, Confirmed: true, SinceMs: 4000, IncidentOpened: "i2"}}},
		{"pausing a DOWN monitor resolves its incident", put("pause-t1", "t1", pausedDoc, 1), 5000, nil,
			[]StateEvent{{TenantID: "t1", MonitorID: "m1", Status: store.StatusPaused, Confirmed: true, SinceMs: 5000, IncidentResolved: true}}},
		{"t2 to DOWN is independent of t1", change("t2", store.StatusPending, store.StatusDown, 5500, "i3"), 5600, nil,
			[]StateEvent{{TenantID: "t2", MonitorID: "m2", Status: store.StatusDown, Confirmed: true, SinceMs: 5500, IncidentOpened: "i3"}}},
		{"duplicate create is not emitted", put("create-t1", "t1", monitorDoc("one"), 0), 5700, nil, nil},
		{"duplicate change is not emitted", change("t2", store.StatusPending, store.StatusDown, 5500, "i3"), 5800, nil, nil},
		{"unpausing starts PENDING without incident", put("resume-t1", "t1", monitorDoc("one"), 2), 6000, nil,
			[]StateEvent{{TenantID: "t1", MonitorID: "m1", Status: store.StatusPending, Confirmed: true, SinceMs: 6000}}},
	}

	for _, s := range steps {
		got = nil
		res := h.apply(s.cmd, s.issued)
		if s.wantErr == nil && res.Err != nil || s.wantErr != nil && !errors.Is(res.Err, s.wantErr) {
			t.Fatalf("%s: err = %v, want %v", s.name, res.Err, s.wantErr)
		}
		if !slices.Equal(got, s.want) {
			t.Errorf("%s: events = %+v, want %+v", s.name, got, s.want)
		}
	}
}

// TestStateEventsOnlyCommitted checks that no event is delivered for a replayed
// log entry, and that rejected payloads and duplicates do not leak events of
// their failed transaction.
func TestStateEventsOnlyCommitted(t *testing.T) {
	t.Parallel()
	h := newAlertFSM(t, t.TempDir())
	h.putTenant(1000, "t1")
	h.putMonitor(1001, "t1", "m1")

	var got []StateEvent
	h.fsm.OnStateChange(func(ev StateEvent) { got = append(got, ev) })

	// Replaying an already applied index is skipped without events.
	cmd := NewSetMonitorState(StateChange{TenantID: "t1", MonitorID: "m1", From: store.StatusPending, To: store.StatusDown, SinceMs: 2000,
		Incident: incidentOf("i1")})
	cmd.IssuedAtMs = 2100
	data, err := proto.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if res := h.fsm.Apply(&raft.Log{Index: 1, Type: raft.LogCommand, Data: data}).(ApplyResult); res.Err != nil {
		t.Fatal(res.Err)
	}
	if len(got) != 0 {
		t.Fatalf("replayed entry emitted %+v", got)
	}

	// A committed change emits once; applying the same request again is a duplicate.
	h.do(cmd, 2100, nil)
	if want := []StateEvent{{TenantID: "t1", MonitorID: "m1", Status: store.StatusDown, Confirmed: true, SinceMs: 2000, IncidentOpened: "i1"}}; !slices.Equal(got, want) {
		t.Fatalf("events = %+v, want %+v", got, want)
	}
	got = nil
	h.do(cmd, 2200, nil)
	if len(got) != 0 {
		t.Fatalf("duplicate emitted %+v", got)
	}

	// A command rejected after a state write is rolled back and emits nothing.
	bad := NewSetMonitorState(StateChange{TenantID: "t1", MonitorID: "m1", From: store.StatusPending, To: store.StatusUp, SinceMs: 3000})
	h.do(bad, 3100, store.ErrConflict)
	if len(got) != 0 {
		t.Fatalf("rejected command emitted %+v", got)
	}
}

// TestStateEventsWithoutObserver makes sure that applying without a registered
// observer works and that the observer can be removed again.
func TestStateEventsWithoutObserver(t *testing.T) {
	t.Parallel()
	h := newAlertFSM(t, t.TempDir())
	h.putTenant(1000, "t1")
	h.putMonitor(1001, "t1", "m1")

	calls := 0
	h.fsm.OnStateChange(func(StateEvent) { calls++ })
	h.fsm.OnStateChange(nil)
	h.changeState(2000, "t1", "m1", store.StatusPending, store.StatusUp, 1999, "")
	if calls != 0 {
		t.Fatalf("removed observer was called %d times", calls)
	}
}

// incidentOf returns the incident of a DOWN change, or nil.
func incidentOf(id string) *store.Incident {
	if id == "" {
		return nil
	}
	return &store.Incident{ID: id, Cause: "connection refused", ErrorClass: "connect", AffectedNodes: []string{"n1"}}
}
