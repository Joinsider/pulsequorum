// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
)

func stateChange(from, to store.MonitorStatus, since int64, incidentID string) *pb.Command {
	c := StateChange{TenantID: "default", MonitorID: "m1", From: from, To: to, SinceMs: since}
	if incidentID != "" {
		c.Incident = &store.Incident{ID: incidentID, Cause: "connection refused", ErrorClass: "connect", AffectedNodes: []string{"n1"}}
	}
	return NewSetMonitorState(c)
}

// TestMonitorStateAndIncidents covers FA-CON-06 and FA-INC-01: status changes
// open and resolve incidents atomically, pausing ends an incident, and every
// replica ends up with the same rows.
func TestMonitorStateAndIncidents(t *testing.T) {
	ctx := context.Background()
	paused := monitorDoc("one")
	paused.Paused = true
	put := func(id string, doc MonitorDocument, rev uint64) *pb.Command {
		c, err := NewPutMonitor(id, "default", "m1", doc, rev)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	tenant, err := NewPutTenant("t", "default", TenantDocument{Name: "Default", Plan: "default"})
	if err != nil {
		t.Fatal(err)
	}
	downNoIncident := stateChange(store.StatusUp, store.StatusDown, 7000, "")

	steps := []struct {
		cmd     *pb.Command
		issued  int64
		wantErr error
		status  store.MonitorStatus
		since   int64
	}{
		{tenant, 1000, nil, "", 0},
		{put("p1", monitorDoc("one"), 0), 1001, nil, store.StatusPending, 1001},
		{stateChange(store.StatusPending, store.StatusDown, 2000, "i1"), 2100, nil, store.StatusDown, 2000},
		{stateChange(store.StatusUp, store.StatusPending, 2200, ""), 2200, store.ErrConflict, store.StatusDown, 2000},
		{stateChange(store.StatusDown, store.StatusDown, 2300, "i9"), 2300, ErrInvalidCommand, store.StatusDown, 2000},
		{stateChange(store.StatusDown, store.StatusUp, 3000, ""), 3100, nil, store.StatusUp, 3000},
		{stateChange(store.StatusUp, store.StatusDown, 4000, "i2"), 4100, nil, store.StatusDown, 4000},
		{put("p2", paused, 1), 5000, nil, store.StatusPaused, 5000},
		{stateChange(store.StatusPaused, store.StatusUp, 5500, ""), 5500, store.ErrConflict, store.StatusPaused, 5000},
		{put("p3", monitorDoc("one"), 2), 6000, nil, store.StatusPending, 6000},
		{stateChange(store.StatusPending, store.StatusPaused, 6500, ""), 6500, ErrInvalidCommand, store.StatusPending, 6000},
		{downNoIncident, 7000, ErrInvalidCommand, store.StatusPending, 6000},
		// A change never predates the current state; the history stays ordered.
		{stateChange(store.StatusPending, store.StatusUp, 5900, ""), 7100, nil, store.StatusUp, 6000},
	}

	var prints []string
	for range 2 {
		st := openStore(t, t.TempDir())
		fsm, err := NewFSM(ctx, st, slog.New(slog.DiscardHandler), func(err error) { t.Fatalf("unexpected halt: %v", err) })
		if err != nil {
			t.Fatal(err)
		}
		for i, s := range steps {
			cmd := proto.Clone(s.cmd).(*pb.Command)
			cmd.IssuedAtMs = s.issued
			data, err := proto.Marshal(cmd)
			if err != nil {
				t.Fatal(err)
			}
			res := fsm.Apply(&raft.Log{Index: uint64(i + 1), Type: raft.LogCommand, Data: data}).(ApplyResult)
			if (s.wantErr == nil) != (res.Err == nil) || (s.wantErr != nil && !errors.Is(res.Err, s.wantErr)) {
				t.Fatalf("step %d: err = %v, want %v", i, res.Err, s.wantErr)
			}
			if s.status == "" {
				continue
			}
			m, err := st.GetMonitor(ctx, "default", "m1")
			if err != nil {
				t.Fatal(err)
			}
			if m.State.Status != s.status || m.State.SinceMs != s.since || !m.State.Confirmed {
				t.Fatalf("step %d: state = %+v, want %s since %d", i, m.State, s.status, s.since)
			}
		}

		page, err := st.ListIncidents(ctx, "default", store.IncidentQuery{})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 2 {
			t.Fatalf("incidents = %+v, want 2", page.Items)
		}
		// Newest first; the second incident was ended by the pause.
		i2, i1 := page.Items[0], page.Items[1]
		if i2.ID != "i2" || i2.StartedMs != 4000 || i2.ResolvedMs != 5000 ||
			i1.ID != "i1" || i1.StartedMs != 2000 || i1.ResolvedMs != 3000 {
			t.Fatalf("incidents = %+v", page.Items)
		}
		if i1.Cause != "connection refused" || i1.ErrorClass != "connect" || i1.MonitorName != "one" ||
			len(i1.AffectedNodes) != 1 || i1.AffectedNodes[0] != "n1" || !i1.Confirmed {
			t.Fatalf("incident details = %+v", i1)
		}

		hist, err := st.StatusHistory(ctx, "default", "m1", 0, 10_000)
		if err != nil {
			t.Fatal(err)
		}
		want := []store.StatusChange{{SinceMs: 1001, Status: store.StatusPending}, {SinceMs: 2000, Status: store.StatusDown},
			{SinceMs: 3000, Status: store.StatusUp}, {SinceMs: 4000, Status: store.StatusDown},
			{SinceMs: 5000, Status: store.StatusPaused}, {SinceMs: 6000, Status: store.StatusUp}}
		if !slices.Equal(hist, want) {
			t.Fatalf("history = %+v", hist)
		}

		fp, err := st.Fingerprint(ctx)
		if err != nil {
			t.Fatal(err)
		}
		prints = append(prints, fp)

		// Deleting the monitor removes its state and incidents.
		data, _ := proto.Marshal(&pb.Command{Version: 1, IssuedAtMs: 8000, TenantId: "default",
			Payload: &pb.Command_DeleteEntity{DeleteEntity: &pb.DeleteEntity{Kind: pb.EntityKind_ENTITY_KIND_MONITOR, Id: "m1"}}})
		if res := fsm.Apply(&raft.Log{Index: uint64(len(steps) + 1), Type: raft.LogCommand, Data: data}).(ApplyResult); res.Err != nil {
			t.Fatal(res.Err)
		}
		if page, _ := st.ListIncidents(ctx, "default", store.IncidentQuery{}); len(page.Items) != 0 {
			t.Fatalf("incidents after delete = %+v", page.Items)
		}
	}
	if prints[0] != prints[1] {
		t.Fatalf("replicas diverged: %s vs %s", prints[0], prints[1])
	}
}

func TestErrorClassMapping(t *testing.T) {
	for _, c := range []string{"dns", "connect", "tls", "timeout", "http_status", "assertion", "ssrf_blocked", "internal"} {
		if got := ErrorClassFromProto(ErrorClassToProto(c)); got != c {
			t.Errorf("round trip of %q = %q", c, got)
		}
	}
	if ErrorClassToProto("bogus") != pb.ErrorClass_ERROR_CLASS_UNSPECIFIED || ErrorClassFromProto(pb.ErrorClass_ERROR_CLASS_UNSPECIFIED) != "" {
		t.Error("unknown classes must map to UNSPECIFIED and back to empty")
	}
}
