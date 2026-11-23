// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/joinsider/pulsequorum/internal/store"
)

// tablesWithColumn lists the tables that have the column, sorted by name.
func tablesWithColumn(t *testing.T, db *sql.DB, column string) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		`SELECT m.name FROM sqlite_master AS m
		 WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%'
		   AND EXISTS (SELECT 1 FROM pragma_table_info(m.name) AS p WHERE p.name = ?)
		 ORDER BY m.name`, column)
	if err != nil {
		t.Fatalf("list tables with %s: %v", column, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// countWhere counts the rows of table whose column equals value.
func countWhere(t *testing.T, db *sql.DB, table, column string, value any) int {
	t.Helper()
	var n int
	// Table and column names come from sqlite_master and the fixed lists of the tests.
	q := fmt.Sprintf(`SELECT COUNT(*) FROM "%s" WHERE "%s" = ?`, table, column) //nolint:gosec // G201: trusted identifiers
	if err := db.QueryRowContext(context.Background(), q, value).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// stateTenantTables are the state.db tables with a tenant_id column. DeleteTenant
// must clear every one of them; a new table with tenant_id fails the test until
// it is listed here and cleared by DeleteTenant.
var stateTenantTables = []string{
	"alert_outbox", "api_tokens", "audit_log", "incidents", "join_tokens", "maintenance_windows", "monitor_channels",
	"monitor_states", "monitor_status_history", "monitor_tags", "monitors", "notification_channels",
	"probe_alerts", "refresh_sessions", "unconfirmed_alerts", "users",
}

// populateStateTenant writes one row of every kind of tenant data with the
// store API. IDs carry the tenant suffix, because several IDs are unique across tenants.
// auditIndex is the Raft index of the audit entry, unique per tenant.
func populateStateTenant(t *testing.T, s *StateStore, tid string, auditIndex uint64) {
	t.Helper()
	ctx := context.Background()
	x := "-" + tid
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("populate %s: %v", tid, err)
		}
	}
	must(s.Update(ctx, func(tx store.StateTx) error {
		if err := tx.PutTenant(ctx, store.Tenant{ID: tid, Name: tid, Plan: "default", CreatedMs: 1, UpdatedMs: 1}); err != nil {
			return err
		}
		if err := tx.PutChannel(ctx, store.NotificationChannel{TenantID: tid, ID: "ch" + x, Name: "ch" + x, Kind: store.ChannelWebhook,
			URLEnc: "enc", WebhookMethod: "POST", Events: []string{store.EventDown}, CreatedMs: 1, UpdatedMs: 1}, 0); err != nil {
			return err
		}
		if err := tx.PutMonitor(ctx, store.Monitor{TenantID: tid, ID: "mon" + x, Name: "mon" + x, Type: "http",
			Config: json.RawMessage(`{"url":"https://example.com"}`), IntervalSeconds: 60, TimeoutSeconds: 48, Retries: 1,
			RecoveryThreshold: 1, Assignment: store.Assignment{Mode: "all"}, ConsensusPolicy: "majority",
			Tags: []string{"web"}, ChannelIDs: []string{"ch" + x}, CreatedMs: 1, UpdatedMs: 1}, 0); err != nil {
			return err
		}
		if err := tx.PutMonitorState(ctx, tid, "mon"+x, store.MonitorState{Status: store.StatusDown, Confirmed: true, SinceMs: 2}); err != nil {
			return err
		}
		if err := tx.OpenIncident(ctx, store.Incident{TenantID: tid, ID: "inc" + x, MonitorID: "mon" + x, StartedMs: 2,
			Cause: "refused", AffectedNodes: []string{"n1"}, Confirmed: true}); err != nil {
			return err
		}
		if err := tx.EnqueueAlert(ctx, store.AlertDelivery{TenantID: tid, AlertKey: "key" + x, ChannelID: "ch" + x,
			MonitorID: "mon" + x, Payload: json.RawMessage(`{}`), NextAttemptMs: 3, State: store.AlertPending, CreatedMs: 3, UpdatedMs: 3}); err != nil {
			return err
		}
		if err := tx.PutMaintenanceWindow(ctx, store.MaintenanceWindow{TenantID: tid, ID: "win" + x, Name: "win" + x,
			Schedule: store.MaintenanceSchedule{Kind: store.ScheduleOnce, StartsMs: 1, EndsMs: 2}, Active: true,
			CreatedMs: 1, UpdatedMs: 1}, 0); err != nil {
			return err
		}
		if err := tx.PutUser(ctx, store.User{TenantID: tid, ID: "usr" + x, Username: "user" + x, PasswordHash: "hash",
			Role: "viewer", CreatedMs: 1, UpdatedMs: 1}, 0); err != nil {
			return err
		}
		if err := tx.InsertSession(ctx, store.Session{TokenHash: "h" + x, FamilyID: "f" + x, TenantID: tid,
			UserID: "usr" + x, CreatedMs: 1, ExpiresMs: 9_000_000_000_000}); err != nil {
			return err
		}
		if err := tx.InsertJoinToken(ctx, store.JoinToken{ID: "jt" + x, SecretHash: "hash", Role: "probe", TenantID: tid,
			ExpiresMs: 9_000_000_000_000, CreatedMs: 1}); err != nil {
			return err
		}
		if err := tx.InsertAPIToken(ctx, store.APIToken{ID: "tok" + x, TenantID: tid, Scope: "tenant", Name: "ci" + x,
			Role: "tenant-editor", SecretHash: "hash" + x, CreatedMs: 1, ExpiresMs: 9_000_000_000_000,
			CreatedByKind: "user", CreatedByID: "usr" + x, CreatedByName: "User"}); err != nil {
			return err
		}
		if err := tx.InsertAudit(ctx, []store.AuditEntry{{Index: auditIndex, TenantID: tid, AtMs: 1, ActorKind: "user",
			ActorID: "usr" + x, Action: "monitor.create", ObjectKind: "monitor", ObjectID: "mon" + x}}); err != nil {
			return err
		}
		if err := tx.PutProbeAlert(ctx, store.ProbeAlert{TenantID: tid, ProbeID: "prb" + x, Active: true, SinceMs: 1, UpdatedMs: 2}); err != nil {
			return err
		}
		return tx.InsertUnconfirmedAlerts(ctx, []store.UnconfirmedAlert{{NodeID: "node" + x, LocalID: 1, TenantID: tid,
			MonitorID: "mon" + x, ChannelID: "ch" + x, Status: "DOWN", SinceMs: 2, SentMs: 3, Outcome: "sent",
			Reachable: 1, Total: 3, Reporters: []string{"node" + x}, ReportedMs: 4}})
	}))
}

// TestDeleteTenantLeavesNoRows deletes one of two tenants and checks with
// sqlite_master that no table with a tenant_id column keeps a row of it, while
// the other tenant is untouched.
func TestDeleteTenantLeavesNoRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openState(t, t.TempDir())
	populateStateTenant(t, s, "tenant-a", 1)
	populateStateTenant(t, s, "tenant-b", 2)
	db := s.DB().reader

	tables := tablesWithColumn(t, db, "tenant_id")
	want := slices.Clone(stateTenantTables)
	slices.Sort(want)
	if !slices.Equal(tables, want) {
		t.Fatalf("tables with tenant_id = %v, want %v: update DeleteTenant and stateTenantTables", tables, want)
	}

	before := map[string]int{}
	for _, table := range tables {
		before[table] = countWhere(t, db, table, "tenant_id", "tenant-a")
		if before[table] == 0 {
			t.Errorf("table %s has no row of tenant-a: the setup does not cover it", table)
		}
	}
	bBefore := map[string]int{}
	for _, table := range tables {
		bBefore[table] = countWhere(t, db, table, "tenant_id", "tenant-b")
	}

	if err := s.Update(ctx, func(tx store.StateTx) error { return tx.DeleteTenant(ctx, "tenant-a", 42) }); err != nil {
		t.Fatalf("DeleteTenant: %v", err)
	}

	for _, table := range tables {
		if n := countWhere(t, db, table, "tenant_id", "tenant-a"); n != 0 {
			t.Errorf("table %s keeps %d row(s) of tenant-a", table, n)
		}
		if n := countWhere(t, db, table, "tenant_id", "tenant-b"); n != bBefore[table] {
			t.Errorf("table %s: tenant-b rows %d, want %d", table, n, bBefore[table])
		}
	}
	if n := countWhere(t, db, "tenants", "id", "tenant-a"); n != 0 {
		t.Errorf("tenants keeps tenant-a: %d", n)
	}
	if n := countWhere(t, db, "deleted_tenants", "id", "tenant-a"); n != 1 {
		t.Errorf("deleted_tenants rows of tenant-a = %d, want 1", n)
	}
	if n := countWhere(t, db, "tenants", "id", "tenant-b"); n != 1 {
		t.Errorf("tenant-b row = %d, want 1", n)
	}
}

// metricsTenantTables are the metrics.db tables with a tenant_id column that
// PurgeTenant clears. purged_tenants is the purge bookkeeping and keeps its row.
var metricsTenantTables = []string{"degraded_alerts", "heartbeats", "series"}

// metricsSeriesTables reference series by series_id and are cleared through it.
var metricsSeriesTables = []string{"minutes", "rollups"}

// TestPurgeTenantLeavesNoRows purges one of two tenants in small batches and
// checks with sqlite_master that no metrics table keeps data of it, including
// the tables that reference series by series_id.
func TestPurgeTenantLeavesNoRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := openMetrics(t, t.TempDir())
	for _, tid := range []string{"tenant-a", "tenant-b"} {
		if n, err := m.InsertHeartbeats(ctx, purgeHeartbeats(tid, 12)); err != nil || n != 12 {
			t.Fatalf("insert heartbeats: %d, %v", n, err)
		}
		if err := m.PutRollups(ctx, []store.Rollup{
			purgeRollup(tid, "n1", 0, 10), purgeRollup(tid, "n1", store.BucketHour, 20), purgeRollup(tid, "", 0, 30),
		}, nil); err != nil {
			t.Fatal(err)
		}
		if err := m.CompactHeartbeats(ctx, []store.LatencyMinute{
			{TenantID: tid, MonitorID: "mon", NodeID: "n1", MinuteMs: 0, Min: 1, Max: 2, Avg: 1.5, P95: 2, OKCount: 2},
			{TenantID: tid, MonitorID: "mon", NodeID: "n1", MinuteMs: 60_000, Min: 1, Max: 2, Avg: 1.5, P95: 2, OKCount: 2},
		}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := m.InsertDegradedAlert(ctx, store.DegradedAlert{TenantID: tid, MonitorID: "mon", ChannelID: "c", Status: "DOWN",
			SinceMs: 1, SentMs: 1, Outcome: "sent", Reachable: 1, Total: 3}); err != nil {
			t.Fatal(err)
		}
	}
	db := m.db.reader
	if got := tablesWithColumn(t, db, "tenant_id"); !slices.Equal(got, []string{"degraded_alerts", "heartbeats", "purged_tenants", "series"}) {
		t.Fatalf("metrics tables with tenant_id = %v: update PurgeTenant and the lists here", got)
	}
	if got := tablesWithColumn(t, db, "series_id"); !slices.Equal(got, metricsSeriesTables) {
		t.Fatalf("metrics tables with series_id = %v, want %v", got, metricsSeriesTables)
	}
	bBefore := map[string]int{}
	for _, table := range append(slices.Clone(metricsTenantTables), "purged_tenants") {
		if table != "purged_tenants" && countWhere(t, db, table, "tenant_id", "tenant-a") == 0 {
			t.Errorf("table %s has no row of tenant-a before the purge", table)
		}
		bBefore[table] = countWhere(t, db, table, "tenant_id", "tenant-b")
	}
	// A purge of tenant-a must not touch rows of tenant-b (or the series of b).
	bSeriesBefore := seriesRowsOf(t, db, "tenant-b")

	batches := 0
	for {
		n, err := m.PurgeTenant(ctx, "tenant-a", 5)
		if err != nil {
			t.Fatalf("PurgeTenant: %v", err)
		}
		if n == 0 {
			break
		}
		if batches++; batches > 1000 {
			t.Fatal("purge does not finish")
		}
	}
	if batches < 3 {
		t.Errorf("purge took %d batches, want several for batch size 5", batches)
	}

	for _, table := range metricsTenantTables {
		if n := countWhere(t, db, table, "tenant_id", "tenant-a"); n != 0 {
			t.Errorf("table %s keeps %d row(s) of tenant-a", table, n)
		}
		if n := countWhere(t, db, table, "tenant_id", "tenant-b"); n != bBefore[table] {
			t.Errorf("table %s: tenant-b rows %d, want %d", table, n, bBefore[table])
		}
	}
	if n := seriesRowsOf(t, db, "tenant-a"); n != 0 {
		t.Errorf("series of tenant-a left: %d", n)
	}
	if n := seriesRowsOf(t, db, "tenant-b"); n != bSeriesBefore {
		t.Errorf("series of tenant-b = %d, want %d", n, bSeriesBefore)
	}
	// Rollups and minutes of tenant-a are gone through their series; no orphan
	// rows may remain for the series that were deleted.
	for _, table := range metricsSeriesTables {
		var orphans int
		q := fmt.Sprintf(`SELECT COUNT(*) FROM "%s" WHERE series_id NOT IN (SELECT id FROM series)`, table) //nolint:gosec // G201: fixed table names
		if err := db.QueryRowContext(ctx, q).Scan(&orphans); err != nil {
			t.Fatal(err)
		}
		if orphans != 0 {
			t.Errorf("table %s has %d orphan rows", table, orphans)
		}
		var remaining int
		q = fmt.Sprintf(`SELECT COUNT(*) FROM "%s" AS x JOIN series AS s ON s.id = x.series_id WHERE s.tenant_id = ?`, table)
		if err := db.QueryRowContext(ctx, q, "tenant-a").Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining != 0 {
			t.Errorf("table %s keeps %d row(s) of tenant-a", table, remaining)
		}
	}
	if err := m.MarkTenantPurged(ctx, "tenant-a", 7); err != nil {
		t.Fatal(err)
	}
	if n := countWhere(t, db, "purged_tenants", "tenant_id", "tenant-a"); n != 1 {
		t.Errorf("purged marker rows = %d, want 1", n)
	}
}

// purgeHeartbeats returns count heartbeats of one monitor of the tenant; each
// tenant has its own incarnation, because heartbeats are unique by node, incarnation and seq.
func purgeHeartbeats(tenantID string, count int) []store.Heartbeat {
	out := make([]store.Heartbeat, 0, count)
	for i := range count {
		out = append(out, store.Heartbeat{
			NodeID: "n1", Incarnation: "inc-" + tenantID, Seq: uint64(i + 1),
			TenantID: tenantID, MonitorID: "mon", TsMs: int64(i) * 1000, Status: store.CheckUp, LatencyMs: 10,
		})
	}
	return out
}

// purgeRollup returns an hour rollup of the tenant's monitor "mon".
func purgeRollup(tenantID, nodeID string, start, up int64) store.Rollup {
	return store.Rollup{TenantID: tenantID, MonitorID: "mon", NodeID: nodeID, BucketMs: store.BucketHour, StartMs: start,
		UpMs: up, UnknownMs: store.BucketHour - up}
}

// seriesRowsOf counts the series rows of a tenant.
func seriesRowsOf(t *testing.T, db *sql.DB, tenantID string) int {
	t.Helper()
	return countWhere(t, db, "series", "tenant_id", tenantID)
}
