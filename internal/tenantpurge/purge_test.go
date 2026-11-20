// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package tenantpurge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/store/sqlite"
)

// fakeState answers only the tombstone list; the other methods panic through
// the nil embedded interface.
type fakeState struct {
	store.State
	deleted []store.DeletedTenant
	err     error
}

func (f *fakeState) ListDeletedTenants(context.Context) ([]store.DeletedTenant, error) {
	return f.deleted, f.err
}

type fixture struct {
	t       *testing.T
	ctx     context.Context
	metrics *sqlite.MetricsStore
	state   *fakeState
	seq     uint64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	m, err := sqlite.OpenMetrics(ctx, t.TempDir(), sqlite.MetricsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return &fixture{t: t, ctx: ctx, metrics: m, state: &fakeState{}}
}

// heartbeats returns n heartbeats of the tenant from this node's stream.
func (f *fixture) heartbeats(tenantID string, n int) []store.Heartbeat {
	out := make([]store.Heartbeat, 0, n)
	for range n {
		f.seq++
		out = append(out, store.Heartbeat{
			NodeID: "node-a", Incarnation: f.metrics.Incarnation(), Seq: f.seq,
			TenantID: tenantID, MonitorID: "m1", TsMs: time.Now().UnixMilli(), Status: 1,
		})
	}
	return out
}

func (f *fixture) count(tenantID string) int {
	f.t.Helper()
	page, err := f.metrics.ListHeartbeats(f.ctx, tenantID, store.HeartbeatQuery{MonitorID: "m1", Limit: 1000})
	if err != nil {
		f.t.Fatal(err)
	}
	return len(page.Items)
}

func (f *fixture) job(snapshot func() error, tombs *Tombstones) *Job {
	return New(Options{
		State: f.state, Metrics: f.metrics, Tombstones: tombs, Snapshot: snapshot,
		BatchSize: 1, Pause: time.Microsecond, Logger: slog.New(slog.DiscardHandler),
	})
}

func TestIngestFilterDropsDeletedTenants(t *testing.T) {
	tests := []struct {
		name       string
		deleted    []string
		wantInsert int
		wantGone   int
		wantLive   int
	}{
		{name: "no tombstones keeps everything", deleted: nil, wantInsert: 5, wantGone: 2, wantLive: 3},
		{name: "tombstone drops only that tenant", deleted: []string{"gone"}, wantInsert: 3, wantGone: 0, wantLive: 3},
		{name: "unrelated tombstone changes nothing", deleted: []string{"other"}, wantInsert: 5, wantGone: 2, wantLive: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			tombs := NewTombstones()
			tombs.Replace(tt.deleted)
			ingest := NewIngestFilter(f.metrics, tombs, nil)

			batch := append(f.heartbeats("gone", 2), f.heartbeats("live", 3)...)
			n, err := ingest.InsertHeartbeats(f.ctx, batch)
			if err != nil {
				t.Fatal(err)
			}
			if n != tt.wantInsert {
				t.Errorf("inserted = %d, want %d", n, tt.wantInsert)
			}
			if got := f.count("gone"); got != tt.wantGone {
				t.Errorf("gone rows = %d, want %d", got, tt.wantGone)
			}
			if got := f.count("live"); got != tt.wantLive {
				t.Errorf("live rows = %d, want %d", got, tt.wantLive)
			}
		})
	}
}

func TestIngestFilterAllDroppedIsNoop(t *testing.T) {
	f := newFixture(t)
	tombs := NewTombstones()
	tombs.Replace([]string{"gone"})
	n, err := NewIngestFilter(f.metrics, tombs, nil).InsertHeartbeats(f.ctx, f.heartbeats("gone", 4))
	if err != nil || n != 0 {
		t.Fatalf("InsertHeartbeats = %d, %v; want 0, nil", n, err)
	}
}

func TestCyclePurgesDeletedTenantOnce(t *testing.T) {
	f := newFixture(t)
	if _, err := f.metrics.InsertHeartbeats(f.ctx, append(f.heartbeats("gone", 3), f.heartbeats("live", 2)...)); err != nil {
		t.Fatal(err)
	}
	f.state.deleted = []store.DeletedTenant{{ID: "gone", DeletedMs: 1}}
	tombs := NewTombstones()
	snapshots := 0
	job := f.job(func() error { snapshots++; return nil }, tombs)

	if err := job.Cycle(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.count("gone"); got != 0 {
		t.Errorf("gone rows after purge = %d, want 0", got)
	}
	if got := f.count("live"); got != 2 {
		t.Errorf("live rows = %d, want 2 untouched", got)
	}
	purged, err := f.metrics.PurgedTenants(f.ctx)
	if err != nil || !slices.Equal(purged, []string{"gone"}) {
		t.Fatalf("purged = %v, %v; want [gone]", purged, err)
	}
	if !tombs.Has("gone") || tombs.Has("live") {
		t.Errorf("tombstones not refreshed: gone=%t live=%t", tombs.Has("gone"), tombs.Has("live"))
	}

	// A second cycle has nothing to do and does not snapshot again.
	if err := job.Cycle(f.ctx); err != nil {
		t.Fatal(err)
	}
	if snapshots != 1 {
		t.Errorf("snapshots = %d, want 1", snapshots)
	}
}

func TestCycleContinuesAfterPartialPurge(t *testing.T) {
	// A restart in the middle of a purge leaves some rows behind; the next
	// cycle finishes the tenant (ADR 0029, restart-safe).
	f := newFixture(t)
	if _, err := f.metrics.InsertHeartbeats(f.ctx, f.heartbeats("gone", 5)); err != nil {
		t.Fatal(err)
	}
	if n, err := f.metrics.PurgeTenant(f.ctx, "gone", 2); err != nil || n != 2 {
		t.Fatalf("partial purge = %d, %v; want 2 rows", n, err)
	}
	f.state.deleted = []store.DeletedTenant{{ID: "gone"}}
	if err := f.job(nil, NewTombstones()).Cycle(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.count("gone"); got != 0 {
		t.Errorf("gone rows = %d, want 0", got)
	}
	if purged, _ := f.metrics.PurgedTenants(f.ctx); !slices.Equal(purged, []string{"gone"}) {
		t.Errorf("purged = %v, want [gone]", purged)
	}
}

func TestCycleSkipsAlreadyPurgedTenant(t *testing.T) {
	f := newFixture(t)
	f.state.deleted = []store.DeletedTenant{{ID: "gone"}}
	if err := f.metrics.MarkTenantPurged(f.ctx, "gone", 1); err != nil {
		t.Fatal(err)
	}
	// Rows that arrive after the mark would be a bug of the filter, not of the job;
	// the job must not touch a tenant it has marked.
	if _, err := f.metrics.InsertHeartbeats(f.ctx, f.heartbeats("gone", 1)); err != nil {
		t.Fatal(err)
	}
	if err := f.job(nil, NewTombstones()).Cycle(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.count("gone"); got != 1 {
		t.Errorf("rows of a marked tenant = %d, want 1 (not revisited)", got)
	}
}

func TestCycleReportsListErrors(t *testing.T) {
	f := newFixture(t)
	f.state.err = errors.New("db down")
	if err := f.job(nil, NewTombstones()).Cycle(f.ctx); err == nil {
		t.Fatal("Cycle returned no error for a failing tombstone list")
	}
}

func TestCycleSnapshotFailureDoesNotStopPurge(t *testing.T) {
	f := newFixture(t)
	if _, err := f.metrics.InsertHeartbeats(f.ctx, f.heartbeats("gone", 2)); err != nil {
		t.Fatal(err)
	}
	f.state.deleted = []store.DeletedTenant{{ID: "gone"}}
	err := f.job(func() error { return fmt.Errorf("raft busy") }, NewTombstones()).Cycle(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.count("gone"); got != 0 {
		t.Errorf("gone rows = %d, want 0", got)
	}
}

func TestTombstonesReplaceAndHas(t *testing.T) {
	tombs := NewTombstones()
	if tombs.Has("a") {
		t.Fatal("new cache has a tombstone")
	}
	tombs.Replace([]string{"a", "b"})
	if !tombs.Has("a") || !tombs.Has("b") || tombs.Has("c") {
		t.Fatal("Replace did not set the IDs")
	}
	tombs.Replace(nil)
	if tombs.Has("a") {
		t.Fatal("Replace with nil kept an ID")
	}
}
