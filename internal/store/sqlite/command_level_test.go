// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/joinsider/uptimecluster/internal/store"
)

// TestCommandLevel covers ADR 0018 section 2: the command level is stored in
// fsm_meta, readable through State and StateTx, and survives a snapshot.
func TestCommandLevel(t *testing.T) {
	ctx := context.Background()
	src := openState(t, t.TempDir())

	if level, err := src.CommandLevel(ctx); err != nil || level != 0 {
		t.Fatalf("fresh CommandLevel = %d, %v; want 0", level, err)
	}

	err := src.Update(ctx, func(tx store.StateTx) error {
		if err := tx.SetCommandLevel(ctx, 3); err != nil {
			return err
		}
		level, err := tx.CommandLevel(ctx)
		if err != nil {
			return err
		}
		if level != 3 {
			t.Errorf("StateTx.CommandLevel inside Update = %d; want 3", level)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if level, err := src.CommandLevel(ctx); err != nil || level != 3 {
		t.Fatalf("CommandLevel = %d, %v; want 3", level, err)
	}

	var buf bytes.Buffer
	if err := src.WriteSnapshot(ctx, &buf); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	dst := openState(t, t.TempDir())
	if err := dst.RestoreSnapshot(ctx, &buf); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	if level, err := dst.CommandLevel(ctx); err != nil || level != 3 {
		t.Errorf("restored CommandLevel = %d, %v; want 3", level, err)
	}
}

// TestFingerprintIncludesCommandLevel checks that replicas with different
// command levels are detected as diverged (ADR 0018 section 2).
func TestFingerprintIncludesCommandLevel(t *testing.T) {
	ctx := context.Background()
	a := openState(t, t.TempDir())
	b := openState(t, t.TempDir())

	fa, err := a.Fingerprint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fb, err := b.Fingerprint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fa != fb {
		t.Fatalf("empty stores have different fingerprints")
	}

	if err := b.Update(ctx, func(tx store.StateTx) error { return tx.SetCommandLevel(ctx, 3) }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	fb, err = b.Fingerprint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fa == fb {
		t.Error("fingerprint did not change when only the command level differs")
	}
}

// TestOpenToleratesNewerMigrations covers ADR 0018 section 5: an older binary
// must open state.db and metrics.db that carry migrations it does not know.
func TestOpenToleratesNewerMigrations(t *testing.T) {
	ctx := context.Background()

	t.Run("state", func(t *testing.T) {
		dir := t.TempDir()
		s, err := OpenState(ctx, dir)
		if err != nil {
			t.Fatalf("OpenState: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		simulateNewerMigration(t, filepath.Join(dir, StateFile))
		s, err = OpenState(ctx, dir)
		if err != nil {
			t.Fatalf("reopen state.db with a newer migration: %v", err)
		}
		_ = s.Close()
	})

	t.Run("metrics", func(t *testing.T) {
		dir := t.TempDir()
		m, err := OpenMetrics(ctx, dir, MetricsOptions{})
		if err != nil {
			t.Fatalf("OpenMetrics: %v", err)
		}
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		simulateNewerMigration(t, filepath.Join(dir, MetricsFile))
		m, err = OpenMetrics(ctx, dir, MetricsOptions{})
		if err != nil {
			t.Fatalf("reopen metrics.db with a newer migration: %v", err)
		}
		_ = m.Close()
	})
}

// simulateNewerMigration marks a migration version as applied that this binary
// does not ship, and adds the table such an expand migration would create.
func simulateNewerMigration(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	stmts := []string{
		"CREATE TABLE IF NOT EXISTS expand_test_future (id INTEGER PRIMARY KEY, note TEXT)",
		"INSERT INTO goose_db_version (version_id, is_applied, tstamp) VALUES (99999, 1, CURRENT_TIMESTAMP)",
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}
