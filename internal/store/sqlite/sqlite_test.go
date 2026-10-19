// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package sqlite

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/store/storetest"
)

func openState(t *testing.T, dir string) *StateStore {
	t.Helper()
	s, err := OpenState(context.Background(), dir)
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func openMetrics(t *testing.T, dir string) *MetricsStore {
	t.Helper()
	m, err := OpenMetrics(context.Background(), dir, MetricsOptions{})
	if err != nil {
		t.Fatalf("OpenMetrics: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestStateSuite(t *testing.T) {
	storetest.RunState(t, func(t *testing.T) store.State { return openState(t, t.TempDir()) })
}

func TestMetricsSuite(t *testing.T) {
	storetest.RunMetrics(t, func(t *testing.T) store.Metrics { return openMetrics(t, t.TempDir()) })
}

func TestOpenAppliesPragmas(t *testing.T) {
	s := openState(t, t.TempDir())
	ctx := context.Background()
	for pragma, want := range map[string]string{"journal_mode": "wal", "auto_vacuum": "2", "synchronous": "2"} {
		var got string
		if err := s.db.reader.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil || got != want {
			t.Errorf("PRAGMA %s = %q, %v; want %q", pragma, got, err, want)
		}
	}
	m := openMetrics(t, t.TempDir())
	var sync, cache string
	_ = m.db.writer.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync)
	_ = m.db.writer.QueryRowContext(ctx, "PRAGMA cache_size").Scan(&cache)
	if sync != "1" || cache != fmt.Sprint(-DefaultMetricsCacheKiB) {
		t.Errorf("metrics writer synchronous=%s cache_size=%s", sync, cache)
	}
}

func TestReadersAreReadOnly(t *testing.T) {
	s := openState(t, t.TempDir())
	_, err := s.db.reader.ExecContext(context.Background(), "INSERT INTO tenants (id, name, created_ms, updated_ms) VALUES ('x', 'x', 1, 1)")
	if err == nil {
		t.Fatal("write through the read pool succeeded")
	}
}

func TestSelfTestDetectsWrongPragma(t *testing.T) {
	s := openState(t, t.TempDir())
	ctx := context.Background()
	// The writer pool has exactly one connection, so this change sticks.
	if _, err := s.db.writer.ExecContext(ctx, "PRAGMA busy_timeout = 1"); err != nil {
		t.Fatal(err)
	}
	err := s.db.SelfTest(ctx)
	if err == nil || !strings.Contains(err.Error(), "busy_timeout") {
		t.Fatalf("SelfTest error = %v, want busy_timeout mismatch", err)
	}
}

func TestOpenRejectsDSNCharacters(t *testing.T) {
	_, err := Open(context.Background(), Options{Path: filepath.Join(t.TempDir(), "a?b.db"), Synchronous: SyncNormal})
	if err == nil {
		t.Fatal("path with '?' accepted")
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	dir := t.TempDir()
	s := openState(t, dir)
	ctx := context.Background()
	err := s.Update(ctx, func(tx store.StateTx) error {
		return tx.PutTenant(ctx, store.Tenant{ID: "t1", Name: "t1", Plan: "default", CreatedMs: 1, UpdatedMs: 1})
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	s2 := openState(t, dir)
	if _, err := s2.GetTenant(ctx, "t1"); err != nil {
		t.Fatalf("data lost after reopen: %v", err)
	}
}

// TestIncarnation covers ADR 0012 B4: stable across restarts, new when
// metrics.db is recreated (PF-12).
func TestIncarnation(t *testing.T) {
	dir := t.TempDir()
	m := openMetrics(t, dir)
	first := m.Incarnation()
	_ = m.Close()

	m2 := openMetrics(t, dir)
	if m2.Incarnation() != first {
		t.Errorf("incarnation changed on reopen: %s -> %s", first, m2.Incarnation())
	}
	_ = m2.Close()

	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(filepath.Join(dir, MetricsFile+suffix)); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	m3 := openMetrics(t, dir)
	if m3.Incarnation() == first || len(m3.Incarnation()) != 32 {
		t.Errorf("incarnation after recreate = %q, want new 32-char value (old %q)", m3.Incarnation(), first)
	}
}

// TestConcurrentWritersNoBusy runs writers and readers in parallel; the single
// writer connection must serialize them without SQLITE_BUSY (PF-22).
func TestConcurrentWritersNoBusy(t *testing.T) {
	dir := t.TempDir()
	s := openState(t, dir)
	m := openMetrics(t, dir)
	ctx := context.Background()
	if err := s.Update(ctx, func(tx store.StateTx) error {
		return tx.PutTenant(ctx, store.Tenant{ID: "t", Name: "t", Plan: "default", CreatedMs: 1, UpdatedMs: 1})
	}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for w := range 8 {
		wg.Go(func() {
			for i := range 25 {
				id := fmt.Sprintf("w%d-%d", w, i)
				err := s.Update(ctx, func(tx store.StateTx) error {
					return tx.PutMonitor(ctx, store.Monitor{
						TenantID: "t", ID: id, Name: id, Type: "tcp", IntervalSeconds: 60, TimeoutSeconds: 10,
						Assignment: store.Assignment{Mode: "all"}, ConsensusPolicy: "majority",
					}, 0)
				})
				if err != nil {
					errs <- err
					return
				}
				hb := store.Heartbeat{NodeID: "n", Incarnation: m.Incarnation(), Seq: uint64(w*1000 + i + 1), //nolint:gosec // small test values
					TenantID: "t", MonitorID: id, TsMs: int64(i), Status: store.CheckUp}
				if _, err := m.InsertHeartbeats(ctx, []store.Heartbeat{hb}); err != nil {
					errs <- err
					return
				}
				if _, err := s.ListMonitors(ctx, "t", store.MonitorQuery{Limit: 10}); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	page, err := s.ListMonitors(ctx, "t", store.MonitorQuery{Limit: 500})
	if err != nil || len(page.Items) != 200 {
		t.Errorf("monitors = %d, %v; want 200", len(page.Items), err)
	}
}

func TestMaintenance(t *testing.T) {
	m := openMetrics(t, t.TempDir())
	ctx := context.Background()
	hbs := make([]store.Heartbeat, 0, 2000)
	for i := range 2000 {
		hbs = append(hbs, store.Heartbeat{NodeID: "n", Incarnation: "i", Seq: uint64(i + 1),
			TenantID: "t", MonitorID: "mon", TsMs: int64(i), Status: store.CheckUp, Message: strings.Repeat("x", 200)})
	}
	if _, err := m.InsertHeartbeats(ctx, hbs); err != nil {
		t.Fatal(err)
	}
	if size, err := m.DB().WALSize(); err != nil || size == 0 {
		t.Errorf("WALSize = %d, %v; want > 0 before checkpoint", size, err)
	}
	for {
		n, err := m.DeleteHeartbeatsBefore(ctx, 2000, 500)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	if err := m.DB().IncrementalVacuum(ctx, 0); err != nil {
		t.Fatal(err)
	}
	res, err := m.DB().Checkpoint(ctx)
	if err != nil || res.Busy {
		t.Fatalf("Checkpoint = %+v, %v", res, err)
	}
	if size, err := m.DB().WALSize(); err != nil || size != 0 {
		t.Errorf("WALSize after TRUNCATE checkpoint = %d, %v; want 0", size, err)
	}
	var freelist int
	if err := m.db.writer.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&freelist); err != nil || freelist != 0 {
		t.Errorf("freelist_count = %d, %v; want 0 after incremental vacuum", freelist, err)
	}
}
