// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package sysalert

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/joinsider/uptimecluster/internal/cluster"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
)

// TestJobsSystemTenantAndCheckpoints: on a leader at command level 6 the
// system tenant is created once per leadership, and a checkpoint is proposed
// at once and then once per interval.
func TestJobsSystemTenantAndCheckpoints(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	st := &fakeState{alerts: map[key]store.SystemAlert{}, incs: map[string]string{}, tenants: map[string]bool{}}
	app := &fakeApplier{state: st}
	leader, level := true, uint32(cluster.SystemAlertCommandLevel)
	j := NewJobs(JobOptions{
		IsLeader:           func() bool { return leader },
		CommandLevel:       func(context.Context) (uint32, error) { return level, nil },
		State:              st,
		Cluster:            app,
		CheckpointInterval: time.Hour,
		Now:                func() time.Time { return now },
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ctx := context.Background()

	j.Tick(ctx)
	j.Tick(ctx)
	tenants, checkpoints := tenantCommands(app), checkpointCommands(app)
	if tenants != 1 {
		t.Fatalf("system tenant commands = %d, want 1", tenants)
	}
	if checkpoints != 1 {
		t.Fatalf("checkpoint commands = %d, want 1", checkpoints)
	}
	if actor := app.cmds[0].GetActor().GetKind(); actor != pb.Actor_KIND_SYSTEM {
		t.Errorf("system tenant actor = %v, want KIND_SYSTEM", actor)
	}

	now = now.Add(30 * time.Minute)
	j.Tick(ctx)
	if checkpointCommands(app) != 1 {
		t.Fatalf("checkpoint before the interval ended")
	}
	now = now.Add(31 * time.Minute)
	j.Tick(ctx)
	if checkpointCommands(app) != 2 {
		t.Fatalf("checkpoint after the interval = %d commands, want 2", checkpointCommands(app))
	}

	// A new leadership finds the tenant and creates nothing again.
	leader = false
	j.Tick(ctx)
	leader = true
	j.Tick(ctx)
	if tenantCommands(app) != 1 {
		t.Fatalf("system tenant created again in a new leadership")
	}
}

// TestJobsBelowLevelDoNothing: below command level 6 the reserved tenant and
// the checkpoints are not proposed.
func TestJobsBelowLevelDoNothing(t *testing.T) {
	st := &fakeState{alerts: map[key]store.SystemAlert{}, incs: map[string]string{}, tenants: map[string]bool{}}
	app := &fakeApplier{state: st}
	j := NewJobs(JobOptions{
		IsLeader:     func() bool { return true },
		CommandLevel: func(context.Context) (uint32, error) { return cluster.SystemAlertCommandLevel - 1, nil },
		State:        st,
		Cluster:      app,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	j.Tick(context.Background())
	if len(app.cmds) != 0 {
		t.Fatalf("commands below level 6: %d", len(app.cmds))
	}
}

func tenantCommands(app *fakeApplier) int {
	n := 0
	for _, cmd := range app.cmds {
		if p := cmd.GetPutEntity(); p != nil && p.GetKind() == pb.EntityKind_ENTITY_KIND_TENANT && p.GetId() == store.SystemTenantID {
			n++
		}
	}
	return n
}

func checkpointCommands(app *fakeApplier) int {
	n := 0
	for _, cmd := range app.cmds {
		if cmd.GetCheckpoint() != nil {
			n++
		}
	}
	return n
}
