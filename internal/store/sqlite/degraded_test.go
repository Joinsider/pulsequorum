// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package sqlite

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/joinsider/uptimecluster/internal/store"
)

// migrated is a database without automatic migrations, driven by a goose provider.
type migrated struct {
	db       *DB
	provider *goose.Provider
}

func openMigrated(t *testing.T, files embed.FS, dir, file string) migrated {
	t.Helper()
	ctx := context.Background()
	migrations, err := fs.Sub(files, dir)
	if err != nil {
		t.Fatal(err)
	}
	db, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), file), Synchronous: SyncFull, ReadConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	provider, err := goose.NewProvider(goose.DialectSQLite3, db.writer, migrations)
	if err != nil {
		t.Fatal(err)
	}
	return migrated{db: db, provider: provider}
}

// countRows runs a count query and returns its single value.
func countRows(t *testing.T, db *DB, query string) int {
	t.Helper()
	var n int
	if err := db.writer.QueryRowContext(context.Background(), query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// seedMonitors creates the tenants and monitors with a state, for each
// tenant/monitor pair. Monitor IDs are the same across tenants on purpose.
func seedMonitors(t *testing.T, s *StateStore, pairs ...[2]string) {
	t.Helper()
	ctx := context.Background()
	if err := s.Update(ctx, func(tx store.StateTx) error {
		seen := map[string]bool{}
		for _, p := range pairs {
			if !seen[p[0]] {
				seen[p[0]] = true
				if err := tx.PutTenant(ctx, store.Tenant{ID: p[0], Name: p[0], Plan: "default", Limits: json.RawMessage("{}"), CreatedMs: 1000, UpdatedMs: 1000}); err != nil {
					return err
				}
			}
			m := store.Monitor{TenantID: p[0], ID: p[1], Name: p[1], Type: "http", Config: json.RawMessage(`{"url":"https://example.com"}`),
				IntervalSeconds: 60, TimeoutSeconds: 48, Retries: 1, RecoveryThreshold: 1, Assignment: store.Assignment{Mode: "all"},
				ConsensusPolicy: "majority", CreatedMs: 1000, UpdatedMs: 1000}
			if err := tx.PutMonitor(ctx, m, 0); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUnconfirmedAlertInsertIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openState(t, t.TempDir())
	seedMonitors(t, s, [2]string{"t1", "m1"})

	first := store.UnconfirmedAlert{NodeID: "node-a", LocalID: 7, TenantID: "t1", MonitorID: "m1", ChannelID: "c1",
		Status: "DOWN", SinceMs: 2000, SentMs: 3000, Outcome: "sent", Reachable: 1, Total: 3,
		Reporters: []string{"node-a", "node-b"}, ReportedMs: 9000}
	if err := s.Update(ctx, func(tx store.StateTx) error { return tx.InsertUnconfirmedAlerts(ctx, []store.UnconfirmedAlert{first}) }); err != nil {
		t.Fatal(err)
	}
	// A repeated report with other content changes nothing.
	again := first
	again.Outcome = "failed"
	again.Reporters = []string{"node-c"}
	if err := s.Update(ctx, func(tx store.StateTx) error {
		return tx.InsertUnconfirmedAlerts(ctx, []store.UnconfirmedAlert{again})
	}); err != nil {
		t.Fatal(err)
	}

	want := first
	want.Reporters = []string{"node-a", "node-b"}
	if err := s.Update(ctx, func(tx store.StateTx) error {
		got, err := tx.GetUnconfirmedAlert(ctx, "node-a", 7)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("GetUnconfirmedAlert = %+v, want %+v", got, want)
		}
		_, err = tx.GetUnconfirmedAlert(ctx, "node-a", 8)
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown alert: %v, want ErrNotFound", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUnconfirmedAlertResolve(t *testing.T) {
	ctx := context.Background()
	s := openState(t, t.TempDir())
	seedMonitors(t, s, [2]string{"t1", "m1"})
	alerts := []store.UnconfirmedAlert{
		{NodeID: "node-a", LocalID: 1, TenantID: "t1", MonitorID: "m1", ChannelID: "c1", Status: "DOWN", SinceMs: 3000, SentMs: 3000, Outcome: "sent", Total: 3, Reachable: 1},
		{NodeID: "node-a", LocalID: 2, TenantID: "t1", MonitorID: "m1", ChannelID: "c1", Status: "UP", SinceMs: 2000, SentMs: 2000, Outcome: "sent", Total: 3, Reachable: 1},
		{NodeID: "node-b", LocalID: 1, TenantID: "t1", MonitorID: "m1", ChannelID: "c1", Status: "DOWN", SinceMs: 1000, SentMs: 1000, Outcome: "sent", Total: 3, Reachable: 2},
	}
	if err := s.Update(ctx, func(tx store.StateTx) error { return tx.InsertUnconfirmedAlerts(ctx, alerts) }); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		nodeID  string
		localID int64
		verdict string
		wantErr error
	}{
		{name: "confirmed", nodeID: "node-a", localID: 1, verdict: store.VerdictConfirmed},
		{name: "false alarm", nodeID: "node-b", localID: 1, verdict: store.VerdictFalseAlarm},
		{name: "already judged", nodeID: "node-a", localID: 1, verdict: store.VerdictFalseAlarm, wantErr: store.ErrConflict},
		{name: "unknown alert", nodeID: "node-a", localID: 99, verdict: store.VerdictConfirmed, wantErr: store.ErrNotFound},
		{name: "unknown verdict", nodeID: "node-a", localID: 2, verdict: "maybe", wantErr: errAny},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := s.Update(ctx, func(tx store.StateTx) error {
				return tx.ResolveUnconfirmedAlert(ctx, tc.nodeID, tc.localID, tc.verdict, 5000)
			})
			switch {
			case tc.wantErr == nil && err != nil:
				t.Fatalf("resolve: %v", err)
			case errors.Is(tc.wantErr, errAny) && err == nil:
				t.Fatal("resolve: want error")
			case tc.wantErr != nil && !errors.Is(tc.wantErr, errAny) && !errors.Is(err, tc.wantErr):
				t.Fatalf("resolve: %v, want %v", err, tc.wantErr)
			}
		})
	}

	// The verdict and resolution time are stored; the open list keeps only the open alert.
	var got store.UnconfirmedAlert
	if err := s.Update(ctx, func(tx store.StateTx) error {
		var err error
		got, err = tx.GetUnconfirmedAlert(ctx, "node-b", 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got.Verdict != store.VerdictFalseAlarm || got.ResolvedMs != 5000 {
		t.Errorf("resolved alert = %+v", got)
	}
	open, err := s.ListOpenUnconfirmedAlerts(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].NodeID != "node-a" || open[0].LocalID != 2 {
		t.Errorf("open alerts = %+v, want only node-a/2", open)
	}
	if _, err := s.ListOpenUnconfirmedAlerts(ctx, 0); err == nil {
		t.Error("ListOpenUnconfirmedAlerts(0): want error")
	}
}

// errAny marks a table case that only expects some error.
var errAny = errors.New("any error")

func TestReplaceStatusHistory(t *testing.T) {
	ctx := context.Background()
	s := openState(t, t.TempDir())
	// Monitor IDs are unique across tenants, so the second tenant has its own monitor.
	seedMonitors(t, s, [2]string{"t1", "m1"}, [2]string{"t1", "m2"}, [2]string{"t2", "m3"})
	// Changes at 1000 (UP), 2000 (DOWN), 3000 (UP), 4000 (DOWN) for each monitor.
	if err := s.Update(ctx, func(tx store.StateTx) error {
		for _, p := range [][2]string{{"t1", "m1"}, {"t1", "m2"}, {"t2", "m3"}} {
			for i, st := range []store.MonitorStatus{store.StatusUp, store.StatusDown, store.StatusUp, store.StatusDown} {
				if err := tx.PutMonitorState(ctx, p[0], p[1], store.MonitorState{Status: st, Confirmed: true, SinceMs: int64(1000 * (i + 1))}); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	all := func(tenant, monitor string) []store.StatusChange {
		t.Helper()
		h, err := s.StatusHistory(ctx, tenant, monitor, 0, 100000)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	original := all("t1", "m1")
	if len(original) != 4 {
		t.Fatalf("seeded history = %+v, want 4 changes", original)
	}
	other := all("t1", "m2")
	otherTenant := all("t2", "m3")

	tests := []struct {
		name    string
		entries []store.StatusChange
		want    []store.StatusChange
	}{
		{
			// [2000, 4000): the change at 2000 goes, 4000 stays (to is exclusive).
			name: "empty replacement keeps the change at to",
			want: []store.StatusChange{{SinceMs: 1000, Status: store.StatusUp}, {SinceMs: 4000, Status: store.StatusDown}},
		},
		{
			name:    "replacement inside the interval",
			entries: []store.StatusChange{{SinceMs: 2000, Status: store.StatusDown}, {SinceMs: 3500, Status: store.StatusUp}},
			want: []store.StatusChange{
				{SinceMs: 1000, Status: store.StatusUp}, {SinceMs: 2000, Status: store.StatusDown},
				{SinceMs: 3500, Status: store.StatusUp}, {SinceMs: 4000, Status: store.StatusDown},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.Update(ctx, func(tx store.StateTx) error {
				return tx.ReplaceStatusHistory(ctx, "t1", "m1", 2000, 4000, tc.entries)
			}); err != nil {
				t.Fatal(err)
			}
			if got := all("t1", "m1"); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("history = %+v, want %+v", got, tc.want)
			}
			// Other monitors and other tenants stay untouched.
			if got := all("t1", "m2"); !reflect.DeepEqual(got, other) {
				t.Errorf("other monitor changed: %+v", got)
			}
			if got := all("t2", "m3"); !reflect.DeepEqual(got, otherTenant) {
				t.Errorf("other tenant changed: %+v", got)
			}
		})
	}

	// Entries must lie inside the interval, and the interval must not be empty.
	for _, bad := range []struct {
		name     string
		from, to int64
		entry    int64
	}{
		{"entry at to", 2000, 4000, 4000},
		{"entry before from", 2000, 4000, 1999},
		{"empty interval", 3000, 3000, 3000},
	} {
		err := s.Update(ctx, func(tx store.StateTx) error {
			return tx.ReplaceStatusHistory(ctx, "t1", "m1", bad.from, bad.to, []store.StatusChange{{SinceMs: bad.entry, Status: store.StatusUp}})
		})
		if err == nil {
			t.Errorf("%s: want error", bad.name)
		}
	}
}

func TestReevaluatedIncident(t *testing.T) {
	ctx := context.Background()
	s := openState(t, t.TempDir())
	seedMonitors(t, s, [2]string{"t1", "m1"})
	if err := s.Update(ctx, func(tx store.StateTx) error {
		if err := tx.OpenIncident(ctx, store.Incident{TenantID: "t1", ID: "inc-open", MonitorID: "m1", StartedMs: 5000, Confirmed: true}); err != nil {
			return err
		}
		return tx.OpenIncident(ctx, store.Incident{TenantID: "t1", ID: "inc-gap", MonitorID: "m1", StartedMs: 1000, ResolvedMs: 2000, Confirmed: true, Reevaluated: true, Cause: "timeout", ErrorClass: "timeout"})
	}); err != nil {
		t.Fatal(err)
	}
	gap, err := s.GetIncident(ctx, "t1", "inc-gap")
	if err != nil {
		t.Fatal(err)
	}
	if !gap.Reevaluated || gap.ResolvedMs != 2000 || gap.StartedMs != 1000 {
		t.Errorf("re-evaluated incident = %+v", gap)
	}
	open, err := s.GetIncident(ctx, "t1", "inc-open")
	if err != nil {
		t.Fatal(err)
	}
	if open.Reevaluated || open.ResolvedMs != 0 {
		t.Errorf("normal incident = %+v, want not re-evaluated and open", open)
	}
	// A monitor has at most one open incident; inc-open is still open.
	if err := s.Update(ctx, func(tx store.StateTx) error {
		return tx.OpenIncident(ctx, store.Incident{TenantID: "t1", ID: "inc-second", MonitorID: "m1", StartedMs: 6000})
	}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("second open incident: %v, want ErrConflict", err)
	}
}

func TestLastIssuedAt(t *testing.T) {
	ctx := context.Background()
	s := openState(t, t.TempDir())
	fresh, err := s.Fingerprint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := s.LastIssuedAt(ctx); err != nil || v != 0 {
		t.Fatalf("LastIssuedAt on fresh state = %d, %v; want 0", v, err)
	}
	// Zero is not hashed, so a fingerprint of state without the value stays equal.
	if err := s.Update(ctx, func(tx store.StateTx) error { return tx.SetLastIssuedAt(ctx, 0) }); err != nil {
		t.Fatal(err)
	}
	if fp, _ := s.Fingerprint(ctx); fp != fresh {
		t.Error("fingerprint changed by a zero command time")
	}

	if err := s.Update(ctx, func(tx store.StateTx) error { return tx.SetLastIssuedAt(ctx, 5000) }); err != nil {
		t.Fatal(err)
	}
	if v, err := s.LastIssuedAt(ctx); err != nil || v != 5000 {
		t.Errorf("LastIssuedAt = %d, %v; want 5000", v, err)
	}
	if err := s.Update(ctx, func(tx store.StateTx) error {
		v, err := tx.LastIssuedAt(ctx)
		if err != nil || v != 5000 {
			t.Errorf("tx LastIssuedAt = %d, %v; want 5000", v, err)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	withValue, err := s.Fingerprint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if withValue == fresh {
		t.Error("fingerprint did not change with the command time")
	}
	if err := s.Update(ctx, func(tx store.StateTx) error { return tx.SetLastIssuedAt(ctx, 6000) }); err != nil {
		t.Fatal(err)
	}
	if fp, _ := s.Fingerprint(ctx); fp == withValue {
		t.Error("fingerprint did not change with a new command time")
	}
}

func TestDegradedAlertLog(t *testing.T) {
	ctx := context.Background()
	m := openMetrics(t, t.TempDir())
	base := store.DegradedAlert{TenantID: "t1", MonitorID: "m1", ChannelID: "c1", Status: "DOWN", Outcome: "sent", Total: 3, Reachable: 1}
	var ids []int64
	for _, sent := range []int64{1000, 5000, 9000} {
		a := base
		a.SinceMs, a.SentMs = sent-500, sent
		a.Reporters = []string{"node-a"}
		id, err := m.InsertDegradedAlert(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	first, err := m.ListUnreportedDegradedAlerts(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].ID != ids[0] || first[1].ID != ids[1] {
		t.Fatalf("unreported (limit 2) = %+v, want ids %v", first, ids[:2])
	}
	if first[0].SentMs != 1000 || first[0].Reachable != 1 || first[0].Total != 3 || !reflect.DeepEqual(first[0].Reporters, []string{"node-a"}) {
		t.Errorf("alert roundtrip = %+v", first[0])
	}

	if err := m.MarkDegradedAlertsReported(ctx, ids[:2]); err != nil {
		t.Fatal(err)
	}
	if err := m.MarkDegradedAlertsReported(ctx, nil); err != nil {
		t.Fatal(err)
	}
	rest, err := m.ListUnreportedDegradedAlerts(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 1 || rest[0].ID != ids[2] {
		t.Errorf("unreported after marking = %+v, want only %d", rest, ids[2])
	}
	if _, err := m.ListUnreportedDegradedAlerts(ctx, 0); err == nil {
		t.Error("ListUnreportedDegradedAlerts(0): want error")
	}

	// Prune by sent time: only the alert sent before 6000 goes, reported or not.
	n, err := m.PruneDegradedAlerts(ctx, 6000)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("pruned %d alerts, want 2", n)
	}
	left, err := m.ListUnreportedDegradedAlerts(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].SentMs != 9000 {
		t.Errorf("left after prune = %+v", left)
	}
}

// TestDegradedMigrationsUpDown covers migrations 00015 (state) and 00005 (metrics):
// the new tables and the column appear on Up and disappear on Down.
func TestDegradedMigrationsUpDown(t *testing.T) {
	ctx := context.Background()
	stateMigs := openMigrated(t, stateMigrationFiles, "migrations/state", StateFile)
	if _, err := stateMigs.provider.UpTo(ctx, 14); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, stateMigs.db, `SELECT count(*) FROM sqlite_master WHERE name = 'unconfirmed_alerts'`); n != 0 {
		t.Fatalf("tables before 00015: %d", n)
	}
	if _, err := stateMigs.provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, stateMigs.db, `SELECT count(*) FROM sqlite_master WHERE name = 'unconfirmed_alerts'`); n != 1 {
		t.Fatalf("unconfirmed_alerts after Up: %d tables", n)
	}
	if n := countRows(t, stateMigs.db, `SELECT count(*) FROM pragma_table_info('incidents') WHERE name = 'reevaluated'`); n != 1 {
		t.Fatalf("incidents.reevaluated after Up: %d columns", n)
	}
	if _, err := stateMigs.provider.DownTo(ctx, 14); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, stateMigs.db, `SELECT count(*) FROM sqlite_master WHERE name = 'unconfirmed_alerts'`); n != 0 {
		t.Errorf("unconfirmed_alerts after Down: %d tables", n)
	}
	if n := countRows(t, stateMigs.db, `SELECT count(*) FROM pragma_table_info('incidents') WHERE name = 'reevaluated'`); n != 0 {
		t.Errorf("incidents.reevaluated after Down: %d columns", n)
	}

	metricsMigs := openMigrated(t, metricsMigrationFiles, "migrations/metrics", MetricsFile)
	if _, err := metricsMigs.provider.UpTo(ctx, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := metricsMigs.provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, metricsMigs.db, `SELECT count(*) FROM sqlite_master WHERE name = 'degraded_alerts'`); n != 1 {
		t.Fatalf("degraded_alerts after Up: %d tables", n)
	}
	if _, err := metricsMigs.provider.DownTo(ctx, 4); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, metricsMigs.db, `SELECT count(*) FROM sqlite_master WHERE name = 'degraded_alerts'`); n != 0 {
		t.Errorf("degraded_alerts after Down: %d tables", n)
	}
}
