// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package storetest is the shared test suite for every store implementation
// (ADR 0004, NFA-PORT-03). It includes the tenant isolation tests required by
// FA-TEN-01 and NFA-SEC-02.
package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/joinsider/uptimecluster/internal/store"
)

const (
	tenantA = "tenant-a"
	tenantB = "tenant-b"
)

// RunState runs the suite against a fresh, empty store.State per subtest.
func RunState(t *testing.T, newState func(t *testing.T) store.State) {
	t.Helper()
	tests := []struct {
		name string
		fn   func(t *testing.T, s store.State)
	}{
		{"Tenants", testTenants},
		{"MonitorLifecycle", testMonitorLifecycle},
		{"MonitorRevisions", testMonitorRevisions},
		{"MonitorUnknownTenant", testMonitorUnknownTenant},
		{"MonitorListPagingAndFilters", testMonitorList},
		{"UpdateRollsBackOnError", testUpdateRollback},
		{"TenantIsolation", testStateTenantIsolation},
		{"DedupAndAppliedIndex", testDedupAndAppliedIndex},
		{"SecretKeys", testSecretKeys},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.fn(t, newState(t)) })
	}
}

// RunMetrics runs the suite against a fresh, empty store.Metrics per subtest.
func RunMetrics(t *testing.T, newMetrics func(t *testing.T) store.Metrics) {
	t.Helper()
	tests := []struct {
		name string
		fn   func(t *testing.T, m store.Metrics)
	}{
		{"Incarnation", testIncarnation},
		{"InsertIsIdempotent", testInsertIdempotent},
		{"ListPagingAndFilters", testHeartbeatList},
		{"DeleteBefore", testDeleteBefore},
		{"TenantIsolation", testMetricsTenantIsolation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.fn(t, newMetrics(t)) })
	}
}

// --- State ------------------------------------------------------------------

func seedTenants(t *testing.T, s store.State, ids ...string) {
	t.Helper()
	err := s.Update(context.Background(), func(tx store.StateTx) error {
		for _, id := range ids {
			if err := tx.PutTenant(context.Background(), store.Tenant{ID: id, Name: id, Plan: "default", CreatedMs: 1, UpdatedMs: 1}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed tenants: %v", err)
	}
}

func newMonitor(tenantID, id string) store.Monitor {
	return store.Monitor{
		TenantID:          tenantID,
		ID:                id,
		Name:              "Monitor " + id,
		Type:              "http",
		Config:            json.RawMessage(`{"url":"https://example.com"}`),
		IntervalSeconds:   60,
		TimeoutSeconds:    48,
		Retries:           1,
		RecoveryThreshold: 1,
		Assignment:        store.Assignment{Mode: "all"},
		ConsensusPolicy:   "majority",
		CreatedMs:         1000,
		UpdatedMs:         1000,
	}
}

func put(t *testing.T, s store.State, m store.Monitor, expectedRevision uint64) error {
	t.Helper()
	return s.Update(context.Background(), func(tx store.StateTx) error {
		return tx.PutMonitor(context.Background(), m, expectedRevision)
	})
}

func del(t *testing.T, s store.State, tenantID, id string) error {
	t.Helper()
	return s.Update(context.Background(), func(tx store.StateTx) error {
		return tx.DeleteMonitor(context.Background(), tenantID, id)
	})
}

func testTenants(t *testing.T, s store.State) {
	ctx := context.Background()
	seedTenants(t, s, tenantB, tenantA)

	got, err := s.GetTenant(ctx, tenantA)
	if err != nil || got.Name != tenantA || got.Plan != "default" {
		t.Fatalf("GetTenant = %+v, %v", got, err)
	}
	if _, err := s.GetTenant(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetTenant(missing) error = %v, want ErrNotFound", err)
	}

	err = s.Update(ctx, func(tx store.StateTx) error {
		return tx.PutTenant(ctx, store.Tenant{ID: tenantA, Name: "Renamed", Plan: "pro", Suspended: true, CreatedMs: 1, UpdatedMs: 2})
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTenant(ctx, tenantA)
	if got.Name != "Renamed" || got.Plan != "pro" || !got.Suspended || got.UpdatedMs != 2 {
		t.Errorf("tenant not updated: %+v", got)
	}

	list, err := s.ListTenants(ctx)
	if err != nil || len(list) != 2 || list[0].ID != tenantA || list[1].ID != tenantB {
		t.Errorf("ListTenants = %+v, %v; want ordered by ID", list, err)
	}
}

func testMonitorLifecycle(t *testing.T, s store.State) {
	ctx := context.Background()
	seedTenants(t, s, tenantA)

	m := newMonitor(tenantA, "m1")
	m.Tags = []string{"prod", "", "api", "prod"}
	m.Paused = true
	if err := put(t, s, m, 0); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.GetMonitor(ctx, tenantA, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 1 || got.Name != m.Name || got.Type != "http" || !got.Paused ||
		got.Assignment.Mode != "all" || got.IntervalSeconds != 60 || got.TimeoutSeconds != 48 {
		t.Errorf("unexpected monitor: %+v", got)
	}
	if !slices.Equal(got.Tags, []string{"api", "prod"}) {
		t.Errorf("tags = %v, want normalized [api prod]", got.Tags)
	}
	var cfg map[string]string
	if err := json.Unmarshal(got.Config, &cfg); err != nil || cfg["url"] != "https://example.com" {
		t.Errorf("config = %s, %v", got.Config, err)
	}

	if err := put(t, s, m, 0); !errors.Is(err, store.ErrConflict) {
		t.Errorf("duplicate create error = %v, want ErrConflict", err)
	}

	if err := del(t, s, tenantA, "m1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetMonitor(ctx, tenantA, "m1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("get after delete error = %v, want ErrNotFound", err)
	}
	if err := del(t, s, tenantA, "m1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second delete error = %v, want ErrNotFound", err)
	}
}

func testMonitorRevisions(t *testing.T, s store.State) {
	ctx := context.Background()
	seedTenants(t, s, tenantA)
	m := newMonitor(tenantA, "m1")
	m.Tags = []string{"old"}
	if err := put(t, s, m, 0); err != nil {
		t.Fatal(err)
	}

	m.Name = "Updated"
	m.Tags = []string{"new"}
	m.UpdatedMs = 2000
	if err := put(t, s, m, 1); err != nil {
		t.Fatalf("update at revision 1: %v", err)
	}
	got, _ := s.GetMonitor(ctx, tenantA, "m1")
	if got.Revision != 2 || got.Name != "Updated" || got.UpdatedMs != 2000 || got.CreatedMs != 1000 {
		t.Errorf("after update: %+v", got)
	}
	if !slices.Equal(got.Tags, []string{"new"}) {
		t.Errorf("tags = %v, want [new]", got.Tags)
	}

	if err := put(t, s, m, 1); !errors.Is(err, store.ErrConflict) {
		t.Errorf("stale update error = %v, want ErrConflict", err)
	}
	if err := put(t, s, newMonitor(tenantA, "missing"), 1); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("update of missing monitor error = %v, want ErrNotFound", err)
	}
}

func testMonitorUnknownTenant(t *testing.T, s store.State) {
	if err := put(t, s, newMonitor("no-such-tenant", "m1"), 0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("create for unknown tenant error = %v, want ErrNotFound", err)
	}
}

func testMonitorList(t *testing.T, s store.State) {
	ctx := context.Background()
	seedTenants(t, s, tenantA)
	for i := 1; i <= 5; i++ {
		m := newMonitor(tenantA, fmt.Sprintf("m%d", i))
		m.Name = fmt.Sprintf("Service %d", i)
		if i%2 == 0 {
			m.Tags = []string{"even"}
			m.Name = fmt.Sprintf("API Gateway %d", i)
		}
		if err := put(t, s, m, 0); err != nil {
			t.Fatal(err)
		}
	}

	var ids []string
	q := store.MonitorQuery{Limit: 2}
	for pages := 0; ; pages++ {
		page, err := s.ListMonitors(ctx, tenantA, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range page.Items {
			ids = append(ids, m.ID)
		}
		if page.NextAfterID == "" {
			break
		}
		if pages > 5 {
			t.Fatal("paging does not terminate")
		}
		q.AfterID = page.NextAfterID
	}
	if !slices.Equal(ids, []string{"m1", "m2", "m3", "m4", "m5"}) {
		t.Errorf("paged IDs = %v", ids)
	}

	page, err := s.ListMonitors(ctx, tenantA, store.MonitorQuery{Tag: "even"})
	if err != nil || len(page.Items) != 2 || page.Items[0].ID != "m2" || page.Items[1].ID != "m4" {
		t.Errorf("tag filter = %+v, %v", page.Items, err)
	}
	page, err = s.ListMonitors(ctx, tenantA, store.MonitorQuery{Search: "api gate"})
	if err != nil || len(page.Items) != 2 {
		t.Errorf("case-insensitive search returned %d items, %v; want 2", len(page.Items), err)
	}
	page, err = s.ListMonitors(ctx, tenantA, store.MonitorQuery{Search: "nothing matches"})
	if err != nil || len(page.Items) != 0 || page.NextAfterID != "" {
		t.Errorf("empty search = %+v, %v", page, err)
	}
}

func testUpdateRollback(t *testing.T, s store.State) {
	ctx := context.Background()
	seedTenants(t, s, tenantA)
	boom := errors.New("boom")
	err := s.Update(ctx, func(tx store.StateTx) error {
		if err := tx.PutMonitor(ctx, newMonitor(tenantA, "m1"), 0); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Update error = %v, want boom", err)
	}
	if _, err := s.GetMonitor(ctx, tenantA, "m1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("monitor visible after rollback: %v", err)
	}
}

// testStateTenantIsolation checks every tenant-scoped State method with the ID
// of another tenant (FA-TEN-01/02).
func testStateTenantIsolation(t *testing.T, s store.State) {
	ctx := context.Background()
	seedTenants(t, s, tenantA, tenantB)
	m := newMonitor(tenantA, "m-a")
	m.Tags = []string{"shared"}
	if err := put(t, s, m, 0); err != nil {
		t.Fatal(err)
	}

	if _, err := s.GetMonitor(ctx, tenantB, "m-a"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetMonitor across tenants error = %v, want ErrNotFound", err)
	}
	for _, q := range []store.MonitorQuery{{}, {Tag: "shared"}, {Search: "Monitor"}} {
		page, err := s.ListMonitors(ctx, tenantB, q)
		if err != nil || len(page.Items) != 0 {
			t.Errorf("ListMonitors(%+v) for other tenant = %d items, %v", q, len(page.Items), err)
		}
	}

	hijack := newMonitor(tenantB, "m-a")
	hijack.Name = "hijacked"
	if err := put(t, s, hijack, 1); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("update across tenants error = %v, want ErrNotFound", err)
	}
	if err := put(t, s, hijack, 0); err == nil {
		t.Error("creating a monitor with another tenant's monitor ID succeeded")
	}
	if err := del(t, s, tenantB, "m-a"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete across tenants error = %v, want ErrNotFound", err)
	}

	got, err := s.GetMonitor(ctx, tenantA, "m-a")
	if err != nil || got.Name != m.Name || got.Revision != 1 {
		t.Errorf("owner's monitor changed by other tenant: %+v, %v", got, err)
	}
}

func testDedupAndAppliedIndex(t *testing.T, s store.State) {
	ctx := context.Background()
	if idx, err := s.AppliedIndex(ctx); err != nil || idx != 0 {
		t.Fatalf("initial AppliedIndex = %d, %v; want 0", idx, err)
	}

	entry := store.DedupEntry{RequestID: "req-1", AppliedIndex: 5, IssuedAtMs: 1_000, Result: json.RawMessage(`{"id":"m1"}`)}
	failed := store.DedupEntry{RequestID: "req-2", AppliedIndex: 6, IssuedAtMs: 90_000_000, ErrorCode: "conflict", ErrorMessage: "revision mismatch"}
	err := s.Update(ctx, func(tx store.StateTx) error {
		if _, err := tx.GetDedup(ctx, "req-1"); !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("GetDedup before put: %w", err)
		}
		if err := tx.PutDedup(ctx, entry); err != nil {
			return err
		}
		if err := tx.PutDedup(ctx, failed); err != nil {
			return err
		}
		return tx.SetAppliedIndex(ctx, 6)
	})
	if err != nil {
		t.Fatal(err)
	}
	if idx, err := s.AppliedIndex(ctx); err != nil || idx != 6 {
		t.Errorf("AppliedIndex = %d, %v; want 6", idx, err)
	}

	err = s.Update(ctx, func(tx store.StateTx) error {
		got, err := tx.GetDedup(ctx, "req-1")
		if err != nil {
			return err
		}
		if got.AppliedIndex != 5 || string(got.Result) != `{"id":"m1"}` || got.ErrorCode != "" {
			return fmt.Errorf("GetDedup = %+v", got)
		}
		got, err = tx.GetDedup(ctx, "req-2")
		if err != nil || got.ErrorCode != "conflict" || got.ErrorMessage != "revision mismatch" {
			return fmt.Errorf("GetDedup(req-2) = %+v, %w", got, err)
		}
		if err := tx.PutDedup(ctx, entry); !errors.Is(err, store.ErrConflict) {
			return fmt.Errorf("duplicate PutDedup error = %w, want ErrConflict", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	err = s.Update(ctx, func(tx store.StateTx) error {
		if err := tx.PruneDedup(ctx, 50_000_000); err != nil {
			return err
		}
		if _, err := tx.GetDedup(ctx, "req-1"); !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("req-1 survived pruning: %w", err)
		}
		if _, err := tx.GetDedup(ctx, "req-2"); err != nil {
			return fmt.Errorf("req-2 pruned too early: %w", err)
		}
		return tx.SetAppliedIndex(ctx, 9)
	})
	if err != nil {
		t.Fatal(err)
	}
	if idx, _ := s.AppliedIndex(ctx); idx != 9 {
		t.Errorf("AppliedIndex after second update = %d, want 9", idx)
	}
}

func testSecretKeys(t *testing.T, s store.State) {
	ctx := context.Background()
	if keys, err := s.ListSecretKeys(ctx); err != nil || len(keys) != 0 {
		t.Fatalf("initial keys = %v, %v", keys, err)
	}
	put := func(k store.SecretKey) error {
		return s.Update(ctx, func(tx store.StateTx) error { return tx.PutSecretKey(ctx, k) })
	}
	if err := put(store.SecretKey{KeyID: "k2", WrappedDEK: []byte{2}, KEKFingerprint: "fp", CreatedMs: 20}); err != nil {
		t.Fatal(err)
	}
	if err := put(store.SecretKey{KeyID: "k1", WrappedDEK: []byte{1}, KEKFingerprint: "fp", CreatedMs: 10}); err != nil {
		t.Fatal(err)
	}
	// Retire and rewrap k1; its creation time must be kept.
	if err := put(store.SecretKey{KeyID: "k1", WrappedDEK: []byte{9}, KEKFingerprint: "fp2", CreatedMs: 99, RetiredMs: 30}); err != nil {
		t.Fatal(err)
	}
	if err := put(store.SecretKey{KeyID: "k3"}); err == nil {
		t.Error("incomplete key accepted")
	}
	keys, err := s.ListSecretKeys(ctx)
	if err != nil || len(keys) != 2 {
		t.Fatalf("keys = %+v, %v", keys, err)
	}
	if keys[0].KeyID != "k1" || keys[0].CreatedMs != 10 || keys[0].RetiredMs != 30 ||
		keys[0].KEKFingerprint != "fp2" || keys[0].WrappedDEK[0] != 9 {
		t.Errorf("k1 = %+v", keys[0])
	}
	if keys[1].KeyID != "k2" || keys[1].RetiredMs != 0 {
		t.Errorf("k2 = %+v", keys[1])
	}
}

// --- Metrics ----------------------------------------------------------------

func heartbeats(tenantID, monitorID, node string, fromSeq, count int, startMs int64) []store.Heartbeat {
	out := make([]store.Heartbeat, 0, count)
	for i := range count {
		seq := uint64(fromSeq + i) //nolint:gosec // small test values
		out = append(out, store.Heartbeat{
			NodeID: node, Incarnation: "inc-" + node, Seq: seq,
			TenantID: tenantID, MonitorID: monitorID,
			TsMs: startMs + int64(i)*1000, Status: store.CheckUp, LatencyMs: 10 + i,
		})
	}
	return out
}

func insert(t *testing.T, m store.Metrics, hbs []store.Heartbeat) int {
	t.Helper()
	n, err := m.InsertHeartbeats(context.Background(), hbs)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	return n
}

func testIncarnation(t *testing.T, m store.Metrics) {
	if m.Incarnation() == "" {
		t.Error("empty incarnation")
	}
}

func testInsertIdempotent(t *testing.T, m store.Metrics) {
	ctx := context.Background()
	hbs := heartbeats(tenantA, "mon", "n1", 1, 3, 10_000)
	hbs[0].Details = json.RawMessage(`{"statusCode":200}`)
	if n := insert(t, m, hbs); n != 3 {
		t.Errorf("first insert = %d, want 3", n)
	}
	if n := insert(t, m, hbs); n != 0 {
		t.Errorf("repeated insert = %d, want 0 (idempotent)", n)
	}
	if n := insert(t, m, heartbeats(tenantA, "mon", "n1", 3, 2, 13_000)); n != 1 {
		t.Errorf("overlapping insert = %d, want 1", n)
	}
	if n, err := m.InsertHeartbeats(ctx, nil); n != 0 || err != nil {
		t.Errorf("empty insert = %d, %v", n, err)
	}

	if seq, err := m.LastSeq(ctx, "n1", "inc-n1"); err != nil || seq != 4 {
		t.Errorf("LastSeq = %d, %v; want 4", seq, err)
	}
	if seq, err := m.LastSeq(ctx, "n9", "unknown"); err != nil || seq != 0 {
		t.Errorf("LastSeq of unknown stream = %d, %v; want 0", seq, err)
	}

	page, err := m.ListHeartbeats(ctx, tenantA, store.HeartbeatQuery{MonitorID: "mon"})
	if err != nil || len(page.Items) != 4 {
		t.Fatalf("list = %d items, %v", len(page.Items), err)
	}
	if string(page.Items[0].Details) != `{"statusCode":200}` || page.Items[0].Status != store.CheckUp {
		t.Errorf("first heartbeat = %+v", page.Items[0])
	}
}

func testHeartbeatList(t *testing.T, m store.Metrics) {
	ctx := context.Background()
	// Two nodes report at the same timestamps; ordering must still be total.
	insert(t, m, heartbeats(tenantA, "mon", "n1", 1, 10, 0))
	insert(t, m, heartbeats(tenantA, "mon", "n2", 1, 10, 0))
	insert(t, m, heartbeats(tenantA, "other", "n1", 11, 5, 0))

	var got []store.Heartbeat
	q := store.HeartbeatQuery{MonitorID: "mon", FromMs: 2000, ToMs: 7000, Limit: 3}
	for pages := 0; ; pages++ {
		page, err := m.ListHeartbeats(ctx, tenantA, q)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, page.Items...)
		if page.Next == nil {
			break
		}
		if pages > 10 {
			t.Fatal("paging does not terminate")
		}
		q.After = page.Next
	}
	if len(got) != 12 { // ts 2000..7000 inclusive = 6 timestamps × 2 nodes
		t.Fatalf("got %d heartbeats, want 12", len(got))
	}
	for i := 1; i < len(got); i++ {
		a, b := got[i-1], got[i]
		if a.TsMs > b.TsMs || (a.TsMs == b.TsMs && a.NodeID >= b.NodeID) {
			t.Fatalf("not strictly ordered at %d: %+v then %+v", i, a, b)
		}
	}
	if got[0].TsMs != 2000 || got[len(got)-1].TsMs != 7000 {
		t.Errorf("range = %d..%d, want 2000..7000", got[0].TsMs, got[len(got)-1].TsMs)
	}

	page, err := m.ListHeartbeats(ctx, tenantA, store.HeartbeatQuery{MonitorID: "mon", NodeID: "n2"})
	if err != nil || len(page.Items) != 10 {
		t.Fatalf("node filter = %d items, %v", len(page.Items), err)
	}
	for _, hb := range page.Items {
		if hb.NodeID != "n2" || hb.MonitorID != "mon" {
			t.Fatalf("node filter returned %+v", hb)
		}
	}
}

func testDeleteBefore(t *testing.T, m store.Metrics) {
	ctx := context.Background()
	insert(t, m, heartbeats(tenantA, "mon", "n1", 1, 25, 0)) // ts 0..24000

	var total int64
	for range 10 {
		n, err := m.DeleteHeartbeatsBefore(ctx, 20_000, 7)
		if err != nil {
			t.Fatal(err)
		}
		if n > 7 {
			t.Fatalf("batch deleted %d rows, want <= 7", n)
		}
		if n == 0 {
			break
		}
		total += n
	}
	if total != 20 {
		t.Errorf("deleted %d, want 20", total)
	}
	page, err := m.ListHeartbeats(ctx, tenantA, store.HeartbeatQuery{MonitorID: "mon"})
	if err != nil || len(page.Items) != 5 || page.Items[0].TsMs != 20_000 {
		t.Errorf("remaining = %d items (first ts %v), %v", len(page.Items), firstTs(page.Items), err)
	}
	if _, err := m.DeleteHeartbeatsBefore(ctx, 1, 0); err == nil {
		t.Error("batch size 0 accepted")
	}
}

func firstTs(hbs []store.Heartbeat) any {
	if len(hbs) == 0 {
		return nil
	}
	return hbs[0].TsMs
}

func testMetricsTenantIsolation(t *testing.T, m store.Metrics) {
	ctx := context.Background()
	insert(t, m, heartbeats(tenantA, "mon-a", "n1", 1, 5, 0))

	for _, q := range []store.HeartbeatQuery{
		{MonitorID: "mon-a"},
		{MonitorID: "mon-a", NodeID: "n1"},
		{MonitorID: "mon-a", FromMs: 0, ToMs: 10_000},
	} {
		page, err := m.ListHeartbeats(ctx, tenantB, q)
		if err != nil || len(page.Items) != 0 {
			t.Errorf("ListHeartbeats(%+v) for other tenant = %d items, %v", q, len(page.Items), err)
		}
	}
}
