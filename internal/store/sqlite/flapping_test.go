// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/joinsider/pulsequorum/internal/store"
)

// TestFlappingRoundTrip covers the flapping columns of monitor_states (ADR 0023):
// SetMonitorFlapping writes them, every read returns them, and PutMonitorState
// keeps them.
func TestFlappingRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openState(t, t.TempDir())
	if err := s.Update(ctx, func(tx store.StateTx) error {
		return tx.PutTenant(ctx, store.Tenant{ID: "t1", Name: "t1", Plan: "default", Limits: json.RawMessage("{}"), CreatedMs: 1000, UpdatedMs: 1000})
	}); err != nil {
		t.Fatal(err)
	}
	m := store.Monitor{TenantID: "t1", ID: "m1", Name: "m1", Type: "http", Config: json.RawMessage(`{"url":"https://example.com"}`),
		IntervalSeconds: 60, TimeoutSeconds: 48, Retries: 1, RecoveryThreshold: 1, Assignment: store.Assignment{Mode: "all"},
		ConsensusPolicy: "majority", CreatedMs: 1000, UpdatedMs: 1000}
	if err := s.Update(ctx, func(tx store.StateTx) error {
		if err := tx.PutMonitor(ctx, m, 0); err != nil {
			return err
		}
		if err := tx.PutMonitorState(ctx, "t1", "m1", store.MonitorState{Status: store.StatusUp, Confirmed: true, SinceMs: 2000}); err != nil {
			return err
		}
		return tx.SetMonitorFlapping(ctx, "t1", "m1", true, 3000)
	}); err != nil {
		t.Fatal(err)
	}
	want := store.MonitorState{Status: store.StatusUp, Confirmed: true, SinceMs: 2000, Flapping: true, FlappingSinceMs: 3000}

	got, err := s.GetMonitor(ctx, "t1", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != want {
		t.Errorf("GetMonitor state = %+v, want %+v", got.State, want)
	}
	page, err := s.ListMonitors(ctx, "t1", store.MonitorQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].State != want {
		t.Errorf("ListMonitors state = %+v, want %+v", page.Items, want)
	}

	// A status change keeps the flapping fields; the history gets the change.
	if err := s.Update(ctx, func(tx store.StateTx) error {
		return tx.PutMonitorState(ctx, "t1", "m1", store.MonitorState{Status: store.StatusDown, Confirmed: true, SinceMs: 4000})
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, func(tx store.StateTx) error {
		st, err := tx.GetMonitorState(ctx, "t1", "m1")
		if err != nil {
			return err
		}
		if !st.Flapping || st.FlappingSinceMs != 3000 || st.Status != store.StatusDown {
			t.Errorf("tx state = %+v, want DOWN flapping since 3000", st)
		}
		changes, err := tx.StatusChangesSince(ctx, "t1", "m1", 2500)
		if err != nil {
			return err
		}
		if len(changes) != 1 || changes[0].SinceMs != 4000 || changes[0].Status != store.StatusDown {
			t.Errorf("StatusChangesSince(2500) = %+v, want the DOWN change at 4000", changes)
		}
		all, err := tx.StatusChangesSince(ctx, "t1", "m1", 0)
		if err != nil {
			return err
		}
		if len(all) != 2 {
			t.Errorf("StatusChangesSince(0) = %+v, want 2 changes", all)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.Update(ctx, func(tx store.StateTx) error {
		return tx.SetMonitorFlapping(ctx, "t1", "m1", false, 0)
	}); err != nil {
		t.Fatal(err)
	}
	st, err := s.GetMonitor(ctx, "t1", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if st.State.Flapping || st.State.FlappingSinceMs != 0 {
		t.Errorf("state after clear = %+v", st.State)
	}

	// Without a state row the flapping flag cannot be set.
	err = s.Update(ctx, func(tx store.StateTx) error {
		return tx.SetMonitorFlapping(ctx, "t1", "missing", true, 1)
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SetMonitorFlapping of unknown monitor: %v, want ErrNotFound", err)
	}
}

// TestFlappingMigrationExtendsChannels covers migration 00014: existing
// channels receive the two new events once, and the rows of the monitors
// start unflapped.
func TestFlappingMigrationExtendsChannels(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), StateFile)
	migrations, err := fs.Sub(stateMigrationFiles, "migrations/state")
	if err != nil {
		t.Fatal(err)
	}
	db, err := Open(ctx, Options{Path: path, Synchronous: SyncFull, ReadConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	provider, err := goose.NewProvider(goose.DialectSQLite3, db.writer, migrations)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 13); err != nil {
		t.Fatalf("migrate to version 13: %v", err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.writer.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO tenants (id, name, created_ms, updated_ms) VALUES ('t1', 't1', 1, 1)`)
	exec(`INSERT INTO notification_channels (tenant_id, id, name, kind, url_enc, events_json, revision, created_ms, updated_ms)
		VALUES ('t1', 'c1', 'c1', 'webhook', 'enc:v1:key:AAAA', '["down","up"]', 1, 1, 1)`)
	exec(`INSERT INTO notification_channels (tenant_id, id, name, kind, url_enc, events_json, revision, created_ms, updated_ms)
		VALUES ('t1', 'c2', 'c2', 'webhook', 'enc:v1:key:AAAA', '["flapping","channel_failed","down"]', 1, 1, 1)`)

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate to version 14: %v", err)
	}
	events := func(id string) string {
		t.Helper()
		var e string
		if err := db.writer.QueryRowContext(ctx, `SELECT events_json FROM notification_channels WHERE id = ?`, id).Scan(&e); err != nil {
			t.Fatal(err)
		}
		return e
	}
	if got := events("c1"); got != `["down","up","flapping","channel_failed"]` {
		t.Errorf("events of c1 = %s", got)
	}
	if got := events("c2"); got != `["flapping","channel_failed","down"]` {
		t.Errorf("events of c2 = %s, want unchanged", got)
	}
}
