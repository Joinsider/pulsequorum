// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"testing"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
)

func onceWindow(name string) MaintenanceDocument {
	return MaintenanceDocument{Name: name, Active: true,
		Schedule: store.MaintenanceSchedule{Kind: store.ScheduleOnce, StartsMs: 1000, EndsMs: 2000}}
}

func recurringWindow(name string) MaintenanceDocument {
	return MaintenanceDocument{Name: name, Active: true, MonitorIDs: []string{"m2", "m1", "m1", ""},
		Schedule: store.MaintenanceSchedule{Kind: store.ScheduleRecurring, Cron: "0 2 * * 1", DurationMinutes: 60, Timezone: "Europe/Berlin"}}
}

type maintenanceStep struct {
	cmd     *pb.Command
	issued  int64
	wantErr error
}

// maintenanceSteps builds the command log. Every request ID is unique, so no
// step is answered from the dedup table.
func maintenanceSteps(t *testing.T) []maintenanceStep {
	t.Helper()
	must := func(c *pb.Command, err error) *pb.Command {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	put := func(req, tenant, id string, doc MaintenanceDocument, rev uint64) *pb.Command {
		return must(NewPutMaintenanceWindow(req, tenant, id, doc, rev))
	}
	invalidDocs := []MaintenanceDocument{
		{Name: "", Schedule: store.MaintenanceSchedule{Kind: store.ScheduleOnce, StartsMs: 1, EndsMs: 2}},
		{Name: "bad kind", Schedule: store.MaintenanceSchedule{Kind: "daily"}},
		{Name: "once with cron", Schedule: store.MaintenanceSchedule{Kind: store.ScheduleOnce, StartsMs: 1, EndsMs: 2, Cron: "* * * * *"}},
		{Name: "once reversed", Schedule: store.MaintenanceSchedule{Kind: store.ScheduleOnce, StartsMs: 2000, EndsMs: 1000}},
		{Name: "once empty", Schedule: store.MaintenanceSchedule{Kind: store.ScheduleOnce, StartsMs: 1000, EndsMs: 1000}},
		{Name: "recurring without cron", Schedule: store.MaintenanceSchedule{Kind: store.ScheduleRecurring, DurationMinutes: 60}},
		{Name: "recurring zero duration", Schedule: store.MaintenanceSchedule{Kind: store.ScheduleRecurring, Cron: "0 2 * * 1"}},
		{Name: "recurring too long", Schedule: store.MaintenanceSchedule{Kind: store.ScheduleRecurring, Cron: "0 2 * * 1", DurationMinutes: maxMaintenanceMinutes + 1}},
		{Name: "recurring reversed bounds", Schedule: store.MaintenanceSchedule{Kind: store.ScheduleRecurring, Cron: "0 2 * * 1", DurationMinutes: 60, StartsMs: 5000, EndsMs: 4000}},
	}
	steps := []maintenanceStep{
		{must(NewPutTenant("t", "default", TenantDocument{Name: "Default", Plan: "default"})), 1000, nil},
		{put("w-create", "default", "w1", recurringWindow("Weekly"), 0), 1001, nil},
		{put("w-dup", "default", "w1", recurringWindow("Weekly"), 0), 1002, store.ErrConflict},
		{put("w-update", "default", "w1", recurringWindow("Weekly patch"), 1), 1003, nil},
		{put("w-stale", "default", "w1", recurringWindow("Stale"), 1), 1004, store.ErrConflict},
		{put("w-missing", "default", "w9", recurringWindow("Missing"), 1), 1005, store.ErrNotFound},
		{put("w-once", "default", "w2", onceWindow("Once"), 0), 1006, nil},
		{NewDeleteMaintenanceWindow("w-del", "default", "w2"), 1007, nil},
		{NewDeleteMaintenanceWindow("w-del-again", "default", "w2"), 1008, store.ErrNotFound},
		{put("w-no-tenant", "", "w3", onceWindow("No tenant"), 0), 1009, ErrInvalidCommand},
		{put("w-ghost", "ghost", "w4", onceWindow("Ghost"), 0), 1010, store.ErrNotFound},
	}
	for i, doc := range invalidDocs {
		steps = append(steps, maintenanceStep{put(fmt.Sprintf("w-invalid-%d", i), "default", fmt.Sprintf("bad%d", i), doc, 0), 1011, ErrInvalidCommand})
	}
	steps = append(steps, maintenanceStep{put("w-empty-id", "default", "", onceWindow("No ID"), 0), 1012, ErrInvalidCommand})
	return steps
}

func applyMaintenance(t *testing.T, fsm *FSM, index uint64, s maintenanceStep) ApplyResult {
	t.Helper()
	cmd := proto.Clone(s.cmd).(*pb.Command)
	cmd.IssuedAtMs = s.issued
	data, err := proto.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}
	return fsm.Apply(&raft.Log{Index: index, Type: raft.LogCommand, Data: data}).(ApplyResult)
}

// TestMaintenanceWindowCommands covers FA-MNT-01 at the FSM: create, replace
// with revision check, delete, and structural validation.
func TestMaintenanceWindowCommands(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, t.TempDir())
	fsm, err := NewFSM(ctx, st, slog.New(slog.DiscardHandler), func(err error) { t.Fatalf("unexpected halt: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range maintenanceSteps(t) {
		res := applyMaintenance(t, fsm, uint64(i+1), s)
		if (s.wantErr == nil) != (res.Err == nil) || (s.wantErr != nil && !errors.Is(res.Err, s.wantErr)) {
			t.Fatalf("step %d: err = %v, want %v", i, res.Err, s.wantErr)
		}
	}

	w, err := st.GetMaintenanceWindow(ctx, "default", "w1")
	if err != nil {
		t.Fatal(err)
	}
	if w.Name != "Weekly patch" || w.Revision != 2 || w.CreatedMs != 1001 || w.UpdatedMs != 1003 || !w.Active {
		t.Errorf("window after updates = %+v", w)
	}
	if !slices.Equal(w.MonitorIDs, []string{"m1", "m2"}) {
		t.Errorf("monitor IDs = %v, want normalized [m1 m2]", w.MonitorIDs)
	}
	if w.Schedule.Cron != "0 2 * * 1" || w.Schedule.DurationMinutes != 60 || w.Schedule.Timezone != "Europe/Berlin" {
		t.Errorf("schedule = %+v", w.Schedule)
	}

	list, err := st.ListMaintenanceWindows(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "w1" {
		t.Errorf("windows after rejected and deleted commands = %+v, want only w1", list)
	}
}

// TestMaintenanceWindowDeterminism checks that two replicas applying the same
// log store identical rows (ADR 0005).
func TestMaintenanceWindowDeterminism(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var prints []string
	for range 2 {
		st := openStore(t, t.TempDir())
		fsm, err := NewFSM(ctx, st, slog.New(slog.DiscardHandler), func(err error) { t.Fatalf("unexpected halt: %v", err) })
		if err != nil {
			t.Fatal(err)
		}
		for i, s := range maintenanceSteps(t) {
			applyMaintenance(t, fsm, uint64(i+1), s)
		}
		fp, err := st.Fingerprint(ctx)
		if err != nil {
			t.Fatal(err)
		}
		prints = append(prints, fp)
	}
	if prints[0] != prints[1] {
		t.Error("replicas diverged after maintenance commands")
	}
}
