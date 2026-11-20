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

// TestSystemAlertRoundTrip covers ADR 0025: rows are written with their details,
// read back by key, listed in order, and an active-only listing filters them.
func TestSystemAlertRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openState(t, t.TempDir())
	if err := s.Update(ctx, func(tx store.StateTx) error {
		if err := tx.PutSystemAlert(ctx, store.SystemAlert{Kind: "replication_lag", Subject: "n2", Active: true,
			Level: 1, SinceMs: 100, UpdatedMs: 200, DetailsJSON: json.RawMessage(`{"lagMs":300000}`)}); err != nil {
			return err
		}
		return tx.PutSystemAlert(ctx, store.SystemAlert{Kind: "clock_skew", Subject: "cluster", Active: false, SinceMs: 50, UpdatedMs: 60})
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.Update(ctx, func(tx store.StateTx) error {
		got, err := tx.GetSystemAlert(ctx, "replication_lag", "n2")
		if err != nil {
			return err
		}
		want := store.SystemAlert{Kind: "replication_lag", Subject: "n2", Active: true, Level: 1, SinceMs: 100, UpdatedMs: 200,
			DetailsJSON: json.RawMessage(`{"lagMs":300000}`)}
		if got.Kind != want.Kind || got.Subject != want.Subject || got.Active != want.Active || got.Level != want.Level ||
			got.SinceMs != want.SinceMs || got.UpdatedMs != want.UpdatedMs || string(got.DetailsJSON) != string(want.DetailsJSON) {
			t.Errorf("GetSystemAlert = %+v, want %+v", got, want)
		}
		if _, err := tx.GetSystemAlert(ctx, "replication_lag", "n3"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown subject: err = %v, want ErrNotFound", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	all, err := s.ListSystemAlerts(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Kind != "clock_skew" || all[1].Kind != "replication_lag" {
		t.Errorf("ListSystemAlerts(false) = %+v, want clock_skew then replication_lag", all)
	}
	// Empty details are stored as an empty object.
	if len(all) == 2 && string(all[0].DetailsJSON) != "{}" {
		t.Errorf("details of clock_skew = %s, want {}", all[0].DetailsJSON)
	}
	active, err := s.ListSystemAlerts(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].Kind != "replication_lag" {
		t.Errorf("ListSystemAlerts(true) = %+v, want only replication_lag", active)
	}
}

// TestSystemAlertUpsertReplaces covers the replacement of a row by its key.
func TestSystemAlertUpsertReplaces(t *testing.T) {
	ctx := context.Background()
	s := openState(t, t.TempDir())
	put := func(a store.SystemAlert) {
		t.Helper()
		if err := s.Update(ctx, func(tx store.StateTx) error { return tx.PutSystemAlert(ctx, a) }); err != nil {
			t.Fatal(err)
		}
	}
	put(store.SystemAlert{Kind: "disk_low", Subject: "n1", Active: true, Level: 0, SinceMs: 10, UpdatedMs: 10})
	put(store.SystemAlert{Kind: "disk_low", Subject: "n1", Active: false, Level: 0, SinceMs: 10, UpdatedMs: 20})

	list, err := s.ListSystemAlerts(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Active || list[0].UpdatedMs != 20 {
		t.Errorf("after replace = %+v, want one inactive row updated at 20", list)
	}
}

// TestSystemAlertValidation covers the required fields of a system alert row.
func TestSystemAlertValidation(t *testing.T) {
	ctx := context.Background()
	s := openState(t, t.TempDir())
	for _, a := range []store.SystemAlert{{Subject: "n1"}, {Kind: "disk_low"}} {
		err := s.Update(ctx, func(tx store.StateTx) error { return tx.PutSystemAlert(ctx, a) })
		if err == nil {
			t.Errorf("PutSystemAlert(%+v) succeeded, want error", a)
		}
	}
}

// TestNodeIncarnationRoundTrip covers node_incarnations (FA-REP-06, ADR 0025).
func TestNodeIncarnationRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openState(t, t.TempDir())
	put := func(n store.NodeIncarnation) {
		t.Helper()
		if err := s.Update(ctx, func(tx store.StateTx) error { return tx.PutNodeIncarnation(ctx, n) }); err != nil {
			t.Fatal(err)
		}
	}
	put(store.NodeIncarnation{NodeID: "n2", Incarnation: "b", SeenMs: 1})
	put(store.NodeIncarnation{NodeID: "n1", Incarnation: "a", SeenMs: 2})
	put(store.NodeIncarnation{NodeID: "n1", Incarnation: "c", SeenMs: 3})

	got, err := s.ListNodeIncarnations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []store.NodeIncarnation{{NodeID: "n1", Incarnation: "c", SeenMs: 3}, {NodeID: "n2", Incarnation: "b", SeenMs: 1}}
	if len(got) != len(want) {
		t.Fatalf("ListNodeIncarnations = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ListNodeIncarnations[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if err := s.Update(ctx, func(tx store.StateTx) error {
		return tx.PutNodeIncarnation(ctx, store.NodeIncarnation{NodeID: "n3", SeenMs: 1})
	}); err == nil {
		t.Error("incarnation without value stored, want error")
	}
}

// TestTxFingerprintMatchesCommitted covers StateTx.Fingerprint (ADR 0025): the
// value computed inside the write transaction equals the one of the committed
// state, so a checkpoint and a later comparison agree.
func TestTxFingerprintMatchesCommitted(t *testing.T) {
	ctx := context.Background()
	s := openState(t, t.TempDir())
	var inTx string
	if err := s.Update(ctx, func(tx store.StateTx) error {
		if err := tx.PutSystemAlert(ctx, store.SystemAlert{Kind: "wal_large", Subject: "n1", Active: true, SinceMs: 5, UpdatedMs: 5}); err != nil {
			return err
		}
		if err := tx.PutNodeIncarnation(ctx, store.NodeIncarnation{NodeID: "n1", Incarnation: "x", SeenMs: 5}); err != nil {
			return err
		}
		fp, err := tx.Fingerprint(ctx)
		inTx = fp
		return err
	}); err != nil {
		t.Fatal(err)
	}
	committed, err := s.Fingerprint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if inTx == "" || inTx != committed {
		t.Errorf("fingerprint in transaction = %q, committed = %q", inTx, committed)
	}

	// The new tables are part of the fingerprint.
	if err := s.Update(ctx, func(tx store.StateTx) error {
		return tx.PutNodeIncarnation(ctx, store.NodeIncarnation{NodeID: "n1", Incarnation: "y", SeenMs: 6})
	}); err != nil {
		t.Fatal(err)
	}
	changed, err := s.Fingerprint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if changed == committed {
		t.Error("fingerprint did not change with a node incarnation")
	}
}

// TestEnqueueAlertWithoutMonitor covers the outbox of system alerts: a delivery
// without monitor is stored with NULL and read back with an empty monitor ID,
// while a delivery that names a missing monitor is still rejected.
func TestEnqueueAlertWithoutMonitor(t *testing.T) {
	ctx := context.Background()
	s := openState(t, t.TempDir())
	if err := s.Update(ctx, func(tx store.StateTx) error {
		if err := tx.PutTenant(ctx, store.Tenant{ID: store.SystemTenantID, Name: "System", Plan: "system", CreatedMs: 1, UpdatedMs: 1}); err != nil {
			return err
		}
		return tx.PutChannel(ctx, store.NotificationChannel{
			TenantID: store.SystemTenantID, ID: "sys", Name: "sys", Kind: store.ChannelWebhook, URLEnc: "enc:v1:key:AAAA",
			Events: []string{store.EventSystem}, CreatedMs: 1, UpdatedMs: 1,
		}, 0)
	}); err != nil {
		t.Fatal(err)
	}

	delivery := func(key, monitorID string) store.AlertDelivery {
		return store.AlertDelivery{TenantID: store.SystemTenantID, AlertKey: key, ChannelID: "sys", MonitorID: monitorID,
			Payload: json.RawMessage(`{"event":"system"}`), NextAttemptMs: 1, State: store.AlertPending, CreatedMs: 1, UpdatedMs: 1}
	}
	if err := s.Update(ctx, func(tx store.StateTx) error {
		return tx.EnqueueAlert(ctx, delivery("sys:replication_lag:n1:1:active", ""))
	}); err != nil {
		t.Fatalf("enqueue without monitor: %v", err)
	}
	a, err := s.GetAlert(ctx, "sys:replication_lag:n1:1:active", "sys")
	if err != nil {
		t.Fatal(err)
	}
	if a.MonitorID != "" || a.TenantID != store.SystemTenantID {
		t.Errorf("GetAlert = %+v, want system tenant and no monitor", a)
	}
	due, err := s.DueAlerts(ctx, 2, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].MonitorID != "" {
		t.Errorf("DueAlerts = %+v, want the system delivery without monitor", due)
	}

	err = s.Update(ctx, func(tx store.StateTx) error {
		return tx.EnqueueAlert(ctx, delivery("sys:x:y:1:active", "missing"))
	})
	if err == nil {
		t.Error("delivery of a missing monitor stored, want the foreign key to reject it")
	}
}

// TestSystemAlertMigration covers migration 00016: existing outbox rows keep
// their monitor and its foreign key, the new tables appear, and the down
// migration restores the NOT NULL column.
func TestSystemAlertMigration(t *testing.T) {
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
	if _, err := provider.UpTo(ctx, 15); err != nil {
		t.Fatalf("migrate to version 15: %v", err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.writer.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO tenants (id, name, created_ms, updated_ms) VALUES ('t1', 't1', 1, 1)`)
	exec(`INSERT INTO monitors (tenant_id, id, name, type, config_json, interval_s, timeout_s, retries, recovery_threshold,
		resend_interval_min, assignment_json, consensus_policy, revision, created_ms, updated_ms)
		VALUES ('t1', 'm1', 'm1', 'http', '{}', 60, 5, 0, 1, 5, '{"mode":"all"}', 'majority', 1, 1, 1)`)
	exec(`INSERT INTO notification_channels (tenant_id, id, name, kind, url_enc, events_json, revision, created_ms, updated_ms)
		VALUES ('t1', 'c1', 'c1', 'webhook', 'enc:v1:key:AAAA', '["down"]', 1, 1, 1)`)
	exec(`INSERT INTO alert_outbox (tenant_id, alert_key, channel_id, monitor_id, payload_json, next_attempt_ms, created_ms, updated_ms)
		VALUES ('t1', 'm1:1:DOWN', 'c1', 'm1', '{}', 1, 1, 1)`)

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate to version 16: %v", err)
	}
	var monitorID string
	if err := db.writer.QueryRowContext(ctx, `SELECT monitor_id FROM alert_outbox WHERE alert_key = 'm1:1:DOWN'`).Scan(&monitorID); err != nil || monitorID != "m1" {
		t.Fatalf("monitor of existing row = %q, %v; want m1", monitorID, err)
	}
	if _, err := db.writer.ExecContext(ctx, `INSERT INTO alert_outbox (tenant_id, alert_key, channel_id, monitor_id, payload_json, next_attempt_ms, created_ms, updated_ms)
		VALUES ('t1', 'm1:2:DOWN', 'c1', 'nope', '{}', 1, 1, 1)`); err == nil {
		t.Error("outbox row with a missing monitor stored, want the foreign key to reject it")
	}
	exec(`INSERT INTO alert_outbox (tenant_id, alert_key, channel_id, monitor_id, payload_json, next_attempt_ms, created_ms, updated_ms)
		VALUES ('t1', 'sys:x:y:1:active', 'c1', NULL, '{}', 1, 1, 1)`)
	var tables int
	if err := db.writer.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name IN ('system_alerts', 'node_incarnations')`).Scan(&tables); err != nil || tables != 2 {
		t.Fatalf("new tables = %d, %v; want 2", tables, err)
	}

	if _, err := provider.DownTo(ctx, 15); err != nil {
		t.Fatalf("migrate down to version 15: %v", err)
	}
	var notNull int
	if err := db.writer.QueryRowContext(ctx, `SELECT "notnull" FROM pragma_table_info('alert_outbox') WHERE name = 'monitor_id'`).Scan(&notNull); err != nil || notNull != 1 {
		t.Errorf("monitor_id notnull after down = %d, %v; want 1", notNull, err)
	}
	var n int
	if err := db.writer.QueryRowContext(ctx, `SELECT count(*) FROM alert_outbox`).Scan(&n); err != nil || n != 1 {
		t.Errorf("rows after down = %d, %v; want the monitor row only", n, err)
	}
	if err := db.writer.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name IN ('system_alerts', 'node_incarnations')`).Scan(&tables); err != nil || tables != 0 {
		t.Errorf("tables after down = %d, %v; want 0", tables, err)
	}
}
