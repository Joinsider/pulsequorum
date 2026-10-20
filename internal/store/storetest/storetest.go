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
	"maps"
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
		{"MonitorState", testMonitorState},
		{"Incidents", testIncidents},
		{"IncidentTenantIsolation", testIncidentTenantIsolation},
		{"StatusHistory", testStatusHistory},
		{"Channels", testChannels},
		{"MonitorChannels", testMonitorChannels},
		{"Outbox", testOutbox},
		{"ChannelTenantIsolation", testChannelTenantIsolation},
		{"MaintenanceWindows", testMaintenanceWindows},
		{"MaintenanceTenantIsolation", testMaintenanceTenantIsolation},
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
		{"LatestHeartbeats", testLatestHeartbeats},
		{"Rollups", testRollups},
		{"Compaction", testCompaction},
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

func update(t *testing.T, s store.State, fn func(ctx context.Context, tx store.StateTx) error) error {
	t.Helper()
	return s.Update(context.Background(), func(tx store.StateTx) error { return fn(context.Background(), tx) })
}

func setState(t *testing.T, s store.State, tenantID, monitorID string, status store.MonitorStatus, sinceMs int64) {
	t.Helper()
	err := update(t, s, func(ctx context.Context, tx store.StateTx) error {
		return tx.PutMonitorState(ctx, tenantID, monitorID, store.MonitorState{Status: status, Confirmed: true, SinceMs: sinceMs})
	})
	if err != nil {
		t.Fatalf("set state: %v", err)
	}
}

func testMonitorState(t *testing.T, s store.State) {
	ctx := context.Background()
	seedTenants(t, s, tenantA)
	for _, id := range []string{"m1", "m2", "m3"} {
		if err := put(t, s, newMonitor(tenantA, id), 0); err != nil {
			t.Fatal(err)
		}
	}
	// Without a stored state a monitor reads as PENDING since creation.
	if m, _ := s.GetMonitor(ctx, tenantA, "m1"); m.State.Status != store.StatusPending || m.State.SinceMs != 1000 {
		t.Fatalf("default state = %+v", m.State)
	}
	err := update(t, s, func(ctx context.Context, tx store.StateTx) error {
		_, err := tx.GetMonitorState(ctx, tenantA, "m1")
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing state: err = %v, want ErrNotFound", err)
	}

	setState(t, s, tenantA, "m1", store.StatusDown, 2000)
	setState(t, s, tenantA, "m2", store.StatusUp, 3000)
	setState(t, s, tenantA, "m2", store.StatusDown, 4000)
	m, err := s.GetMonitor(ctx, tenantA, "m2")
	if err != nil || m.State != (store.MonitorState{Status: store.StatusDown, Confirmed: true, SinceMs: 4000}) {
		t.Fatalf("state = %+v, %v", m.State, err)
	}
	var got store.MonitorState
	if err := update(t, s, func(ctx context.Context, tx store.StateTx) (err error) {
		got, err = tx.GetMonitorState(ctx, tenantA, "m2")
		return err
	}); err != nil || got.Status != store.StatusDown {
		t.Fatalf("tx state = %+v, %v", got, err)
	}

	page, err := s.ListMonitors(ctx, tenantA, store.MonitorQuery{Status: store.StatusDown})
	if err != nil || len(page.Items) != 2 || page.Items[0].ID != "m1" || page.Items[1].ID != "m2" {
		t.Fatalf("DOWN monitors = %+v, %v", page.Items, err)
	}
	if page, _ := s.ListMonitors(ctx, tenantA, store.MonitorQuery{Status: store.StatusPending}); len(page.Items) != 1 || page.Items[0].ID != "m3" {
		t.Fatalf("PENDING monitors = %+v", page.Items)
	}

	if err := update(t, s, func(ctx context.Context, tx store.StateTx) error {
		return tx.PutMonitorState(ctx, tenantA, "m1", store.MonitorState{Status: "BOGUS"})
	}); err == nil {
		t.Error("invalid status accepted")
	}
	if err := update(t, s, func(ctx context.Context, tx store.StateTx) error {
		return tx.PutMonitorState(ctx, tenantA, "missing", store.MonitorState{Status: store.StatusUp})
	}); err == nil {
		t.Error("state for a missing monitor accepted")
	}
}

func openIncident(t *testing.T, s store.State, tenantID, monitorID, id string, startedMs int64) error {
	t.Helper()
	return update(t, s, func(ctx context.Context, tx store.StateTx) error {
		return tx.OpenIncident(ctx, store.Incident{TenantID: tenantID, ID: id, MonitorID: monitorID, StartedMs: startedMs,
			Cause: "refused", ErrorClass: "connect", AffectedNodes: []string{"n1", "n2"}, Confirmed: true})
	})
}

func resolveIncident(t *testing.T, s store.State, tenantID, monitorID string, resolvedMs int64) {
	t.Helper()
	if err := update(t, s, func(ctx context.Context, tx store.StateTx) error {
		return tx.ResolveIncident(ctx, tenantID, monitorID, resolvedMs)
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
}

func incidentIDs(p store.IncidentPage) []string {
	ids := make([]string, 0, len(p.Items))
	for _, i := range p.Items {
		ids = append(ids, i.ID)
	}
	return ids
}

func testIncidents(t *testing.T, s store.State) {
	ctx := context.Background()
	seedTenants(t, s, tenantA)
	for _, id := range []string{"m1", "m2"} {
		if err := put(t, s, newMonitor(tenantA, id), 0); err != nil {
			t.Fatal(err)
		}
	}
	// m1: [1000, 2000], [3000, 4000], [5000, open); m2: [2500, open)
	for _, step := range []struct {
		monitor, id string
		started     int64
		resolved    int64
	}{{"m1", "i1", 1000, 2000}, {"m1", "i2", 3000, 4000}, {"m1", "i3", 5000, 0}, {"m2", "i4", 2500, 0}} {
		if err := openIncident(t, s, tenantA, step.monitor, step.id, step.started); err != nil {
			t.Fatal(err)
		}
		if step.resolved != 0 {
			resolveIncident(t, s, tenantA, step.monitor, step.resolved)
		}
	}
	if err := openIncident(t, s, tenantA, "m1", "i5", 6000); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second open incident: err = %v, want ErrConflict", err)
	}
	if err := openIncident(t, s, tenantA, "missing", "i6", 6000); err == nil {
		t.Fatal("incident of a missing monitor accepted")
	}

	inc, err := s.GetIncident(ctx, tenantA, "i1")
	if err != nil || inc.MonitorName != "Monitor m1" || inc.ResolvedMs != 2000 || inc.Cause != "refused" ||
		inc.ErrorClass != "connect" || !slices.Equal(inc.AffectedNodes, []string{"n1", "n2"}) || !inc.Confirmed {
		t.Fatalf("incident = %+v, %v", inc, err)
	}

	cases := []struct {
		name string
		q    store.IncidentQuery
		want []string
	}{
		{"all newest first", store.IncidentQuery{}, []string{"i3", "i2", "i4", "i1"}},
		{"by monitor", store.IncidentQuery{MonitorID: "m2"}, []string{"i4"}},
		{"open only", store.IncidentQuery{OpenOnly: true}, []string{"i3", "i4"}},
		{"overlapping period", store.IncidentQuery{FromMs: 2100, ToMs: 2900}, []string{"i4"}},
		{"period touches ends", store.IncidentQuery{FromMs: 2000, ToMs: 3000}, []string{"i2", "i4", "i1"}},
		{"open end from", store.IncidentQuery{FromMs: 4500}, []string{"i3", "i4"}},
	}
	for _, c := range cases {
		page, err := s.ListIncidents(ctx, tenantA, c.q)
		if err != nil || !slices.Equal(incidentIDs(page), c.want) {
			t.Errorf("%s: %v, %v; want %v", c.name, incidentIDs(page), err, c.want)
		}
	}

	var all []string
	q := store.IncidentQuery{Limit: 3}
	for {
		page, err := s.ListIncidents(ctx, tenantA, q)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, incidentIDs(page)...)
		if page.Next == nil {
			break
		}
		q.After = page.Next
	}
	if !slices.Equal(all, []string{"i3", "i2", "i4", "i1"}) {
		t.Fatalf("paged incidents = %v", all)
	}

	if err := del(t, s, tenantA, "m1"); err != nil {
		t.Fatal(err)
	}
	if page, _ := s.ListIncidents(ctx, tenantA, store.IncidentQuery{}); !slices.Equal(incidentIDs(page), []string{"i4"}) {
		t.Fatalf("incidents after deleting m1 = %v", incidentIDs(page))
	}
}

func testIncidentTenantIsolation(t *testing.T, s store.State) {
	ctx := context.Background()
	seedTenants(t, s, tenantA, tenantB)
	if err := put(t, s, newMonitor(tenantA, "ma"), 0); err != nil {
		t.Fatal(err)
	}
	if err := openIncident(t, s, tenantA, "ma", "ia", 1000); err != nil {
		t.Fatal(err)
	}
	// A monitor of another tenant cannot get an incident in tenant B.
	if err := openIncident(t, s, tenantB, "ma", "ib", 1000); err == nil {
		t.Error("incident for another tenant's monitor accepted")
	}
	if _, err := s.GetIncident(ctx, tenantB, "ia"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("foreign incident: err = %v, want ErrNotFound", err)
	}
	if page, _ := s.ListIncidents(ctx, tenantB, store.IncidentQuery{}); len(page.Items) != 0 {
		t.Errorf("tenant B sees %v", incidentIDs(page))
	}
	// Resolving in tenant B must not touch tenant A's incident.
	resolveIncident(t, s, tenantB, "ma", 5000)
	if inc, _ := s.GetIncident(ctx, tenantA, "ia"); inc.ResolvedMs != 0 {
		t.Errorf("tenant B resolved tenant A's incident: %+v", inc)
	}
	if err := update(t, s, func(ctx context.Context, tx store.StateTx) error {
		return tx.PutMonitorState(ctx, tenantB, "ma", store.MonitorState{Status: store.StatusDown})
	}); err == nil {
		t.Error("state for another tenant's monitor accepted")
	}
}

func history(t *testing.T, s store.State, tenantID string, from, to int64) []int64 {
	t.Helper()
	hs, err := s.StatusHistory(context.Background(), tenantID, "m1", from, to)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]int64, 0, len(hs))
	for _, h := range hs {
		out = append(out, h.SinceMs)
	}
	return out
}

func testStatusHistory(t *testing.T, s store.State) {
	seedTenants(t, s, tenantA, tenantB)
	if err := put(t, s, newMonitor(tenantA, "m1"), 0); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		since  int64
		status store.MonitorStatus
	}{{1000, store.StatusPending}, {2000, store.StatusUp}, {3000, store.StatusPending}, {3000, store.StatusDown}, {5000, store.StatusUp}} {
		setState(t, s, tenantA, "m1", c.status, c.since)
	}
	hs, err := s.StatusHistory(context.Background(), tenantA, "m1", 0, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	// The second change at 3000 replaced the first.
	if len(hs) != 4 || hs[2] != (store.StatusChange{SinceMs: 3000, Status: store.StatusDown}) {
		t.Fatalf("history = %+v", hs)
	}
	if got := history(t, s, tenantA, 2500, 4000); !slices.Equal(got, []int64{2000, 3000}) {
		t.Errorf("window with predecessor = %v", got)
	}
	if got := history(t, s, tenantA, 3000, 4000); !slices.Equal(got, []int64{3000}) {
		t.Errorf("window starting at a change = %v", got)
	}
	if got := history(t, s, tenantA, 500, 900); len(got) != 0 {
		t.Errorf("window before the first change = %v", got)
	}
	if got := history(t, s, tenantB, 0, 10_000); len(got) != 0 {
		t.Errorf("tenant B sees %v", got)
	}

	// Pruning keeps the last change before the cut-off.
	if err := update(t, s, func(ctx context.Context, tx store.StateTx) error {
		return tx.PruneStatusHistory(ctx, tenantA, "m1", 4000)
	}); err != nil {
		t.Fatal(err)
	}
	if got := history(t, s, tenantA, 0, 10_000); !slices.Equal(got, []int64{3000, 5000}) {
		t.Errorf("after prune = %v", got)
	}
	if err := update(t, s, func(ctx context.Context, tx store.StateTx) error {
		return tx.PruneStatusHistory(ctx, tenantB, "m1", 99_999)
	}); err != nil {
		t.Fatal(err)
	}
	if got := history(t, s, tenantA, 0, 10_000); len(got) != 2 {
		t.Errorf("prune in tenant B changed tenant A: %v", got)
	}
}

// --- Notification channels and outbox -----------------------------------------

func newChannel(tenantID, id string) store.NotificationChannel {
	return store.NotificationChannel{
		TenantID:        tenantID,
		ID:              id,
		Name:            "Channel " + id,
		Kind:            store.ChannelWebhook,
		URLEnc:          "enc-url-" + id,
		WebhookMethod:   "POST",
		WebhookHeaders:  map[string]string{"Authorization": "enc-header-" + id},
		WebhookTemplate: `{"text":"{{.Name}}"}`,
		Events:          []string{store.EventUp, store.EventDown},
		CreatedMs:       1000,
		UpdatedMs:       1000,
	}
}

func putChannel(t *testing.T, s store.State, c store.NotificationChannel, expectedRevision uint64) error {
	t.Helper()
	return update(t, s, func(ctx context.Context, tx store.StateTx) error {
		return tx.PutChannel(ctx, c, expectedRevision)
	})
}

func delChannel(t *testing.T, s store.State, tenantID, id string) error {
	t.Helper()
	return update(t, s, func(ctx context.Context, tx store.StateTx) error {
		return tx.DeleteChannel(ctx, tenantID, id)
	})
}

func alertEntry(monitorID, key, channelID string, nextAttemptMs int64) store.AlertDelivery {
	return store.AlertDelivery{
		TenantID: tenantA, AlertKey: key, ChannelID: channelID, MonitorID: monitorID,
		Payload: json.RawMessage(`{"event":"down"}`), NextAttemptMs: nextAttemptMs,
		State: store.AlertPending, CreatedMs: nextAttemptMs, UpdatedMs: nextAttemptMs,
	}
}

func enqueueAlert(t *testing.T, s store.State, a store.AlertDelivery) error {
	t.Helper()
	return update(t, s, func(ctx context.Context, tx store.StateTx) error {
		return tx.EnqueueAlert(ctx, a)
	})
}

// editAlert reads an outbox entry in the transaction, applies edit and stores it.
func editAlert(t *testing.T, s store.State, key, channelID string, edit func(a *store.AlertDelivery)) {
	t.Helper()
	err := update(t, s, func(ctx context.Context, tx store.StateTx) error {
		a, err := tx.GetAlert(ctx, key, channelID)
		if err != nil {
			return err
		}
		edit(&a)
		return tx.UpdateAlert(ctx, a)
	})
	if err != nil {
		t.Fatalf("edit alert %s/%s: %v", key, channelID, err)
	}
}

// dueKeys returns "alertKey/channelID" of the due deliveries in order.
func dueKeys(t *testing.T, s store.State, nowMs int64, limit int) []string {
	t.Helper()
	due, err := s.DueAlerts(context.Background(), nowMs, limit)
	if err != nil {
		t.Fatalf("DueAlerts: %v", err)
	}
	out := make([]string, 0, len(due))
	for _, a := range due {
		out = append(out, a.AlertKey+"/"+a.ChannelID)
	}
	return out
}

func testChannels(t *testing.T, s store.State) {
	ctx := context.Background()
	seedTenants(t, s, tenantA)

	c := newChannel(tenantA, "ch-b")
	c.Events = []string{"up", "down", "up"}
	c.IsDefault = true
	if err := putChannel(t, s, c, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetChannel(ctx, tenantA, "ch-b")
	if err != nil {
		t.Fatal(err)
	}
	if got.TenantID != tenantA || got.ID != "ch-b" || got.Name != c.Name || got.Kind != store.ChannelWebhook ||
		got.URLEnc != c.URLEnc || got.WebhookMethod != "POST" || got.WebhookTemplate != c.WebhookTemplate ||
		!got.IsDefault || got.State != store.ChannelOK || got.LastError != "" || got.Revision != 1 ||
		got.CreatedMs != 1000 || got.UpdatedMs != 1000 {
		t.Errorf("unexpected channel: %+v", got)
	}
	if !maps.Equal(got.WebhookHeaders, c.WebhookHeaders) {
		t.Errorf("headers = %v, want %v", got.WebhookHeaders, c.WebhookHeaders)
	}
	if !slices.Equal(got.Events, []string{"down", "up"}) {
		t.Errorf("events = %v, want sorted unique [down up]", got.Events)
	}

	if err := putChannel(t, s, c, 0); !errors.Is(err, store.ErrConflict) {
		t.Errorf("duplicate create error = %v, want ErrConflict", err)
	}

	// An update repairs a failing channel: the delivery state is reset.
	if err := update(t, s, func(ctx context.Context, tx store.StateTx) error {
		return tx.SetChannelState(ctx, tenantA, "ch-b", store.ChannelFailing, "connection refused")
	}); err != nil {
		t.Fatal(err)
	}
	c.Name = "Beta"
	c.UpdatedMs = 2000
	if err := putChannel(t, s, c, 1); err != nil {
		t.Fatalf("update at revision 1: %v", err)
	}
	got, err = s.GetChannel(ctx, tenantA, "ch-b")
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 2 || got.Name != "Beta" || got.UpdatedMs != 2000 || got.CreatedMs != 1000 ||
		got.State != store.ChannelOK || got.LastError != "" {
		t.Errorf("after update: %+v", got)
	}
	if err := putChannel(t, s, c, 1); !errors.Is(err, store.ErrConflict) {
		t.Errorf("stale update error = %v, want ErrConflict", err)
	}
	if err := putChannel(t, s, newChannel(tenantA, "missing"), 1); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("update of missing channel error = %v, want ErrNotFound", err)
	}

	// The list is ordered by name, not by creation time.
	gamma := newChannel(tenantA, "ch-c")
	gamma.Name = "Gamma"
	alpha := newChannel(tenantA, "ch-a")
	alpha.Name = "Alpha"
	for _, ch := range []store.NotificationChannel{gamma, alpha} {
		if err := putChannel(t, s, ch, 0); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListChannels(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ch := range list {
		names = append(names, ch.Name)
	}
	if !slices.Equal(names, []string{"Alpha", "Beta", "Gamma"}) {
		t.Errorf("channel names = %v, want ordered by name", names)
	}

	if err := delChannel(t, s, tenantA, "ch-b"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetChannel(ctx, tenantA, "ch-b"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("get after delete error = %v, want ErrNotFound", err)
	}
	if err := delChannel(t, s, tenantA, "ch-b"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second delete error = %v, want ErrNotFound", err)
	}
}

func testMonitorChannels(t *testing.T, s store.State) {
	ctx := context.Background()
	seedTenants(t, s, tenantA, tenantB)
	for _, id := range []string{"c1", "c2", "c3"} {
		if err := putChannel(t, s, newChannel(tenantA, id), 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := putChannel(t, s, newChannel(tenantB, "cb1"), 0); err != nil {
		t.Fatal(err)
	}

	m := newMonitor(tenantA, "m1")
	m.ChannelIDs = []string{"c3", "c1", "c3", "c2", "c1"}
	if err := put(t, s, m, 0); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetMonitor(ctx, tenantA, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.ChannelIDs, []string{"c1", "c2", "c3"}) {
		t.Errorf("channel IDs = %v, want sorted unique [c1 c2 c3]", got.ChannelIDs)
	}

	// An update replaces the whole set.
	m.ChannelIDs = []string{"c2"}
	if err := put(t, s, m, 1); err != nil {
		t.Fatal(err)
	}
	if got, err = s.GetMonitor(ctx, tenantA, "m1"); err != nil || !slices.Equal(got.ChannelIDs, []string{"c2"}) {
		t.Errorf("after replacing the set: %v, %v", got.ChannelIDs, err)
	}

	// A channel of another tenant is rejected and the update is rolled back.
	m.ChannelIDs = []string{"c1", "cb1"}
	if err := put(t, s, m, 2); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("foreign channel error = %v, want ErrNotFound", err)
	}
	if got, err = s.GetMonitor(ctx, tenantA, "m1"); err != nil || got.Revision != 2 || !slices.Equal(got.ChannelIDs, []string{"c2"}) {
		t.Errorf("rejected update changed the monitor: %+v, %v", got, err)
	}

	// Deleting a channel removes it from the monitor's set.
	m.ChannelIDs = []string{"c1", "c2"}
	if err := put(t, s, m, 2); err != nil {
		t.Fatal(err)
	}
	if err := delChannel(t, s, tenantA, "c1"); err != nil {
		t.Fatal(err)
	}
	if got, err = s.GetMonitor(ctx, tenantA, "m1"); err != nil || got.Revision != 3 || !slices.Equal(got.ChannelIDs, []string{"c2"}) {
		t.Errorf("after deleting c1: %+v, %v", got, err)
	}
}

func testOutbox(t *testing.T, s store.State) {
	ctx := context.Background()
	seedTenants(t, s, tenantA)
	for _, id := range []string{"m1", "m2"} {
		if err := put(t, s, newMonitor(tenantA, id), 0); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"c1", "c2", "c3"} {
		if err := putChannel(t, s, newChannel(tenantA, id), 0); err != nil {
			t.Fatal(err)
		}
	}

	first := alertEntry("m1", "m1:1000:DOWN", "c1", 1000)
	if err := enqueueAlert(t, s, first); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAlert(ctx, "m1:1000:DOWN", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if got.TenantID != tenantA || got.MonitorID != "m1" || string(got.Payload) != `{"event":"down"}` ||
		got.Attempts != 0 || got.NextAttemptMs != 1000 || got.State != store.AlertPending ||
		got.LastError != "" || got.CreatedMs != 1000 {
		t.Errorf("unexpected entry: %+v", got)
	}

	// The same alert for the same channel is enqueued once: the first payload wins.
	again := alertEntry("m1", "m1:1000:DOWN", "c1", 5000)
	again.Payload = json.RawMessage(`{"event":"other"}`)
	if err := enqueueAlert(t, s, again); err != nil {
		t.Fatalf("repeated enqueue: %v", err)
	}
	if got, err = s.GetAlert(ctx, "m1:1000:DOWN", "c1"); err != nil || string(got.Payload) != `{"event":"down"}` || got.NextAttemptMs != 1000 {
		t.Errorf("repeated enqueue changed the entry: %+v, %v", got, err)
	}

	for _, a := range []store.AlertDelivery{
		alertEntry("m1", "m1:1000:DOWN", "c2", 3000),
		alertEntry("m1", "m1:2000:DOWN", "c1", 1000), // same time as first, sorted after it by key
		alertEntry("m1", "m1:1000:UP", "c1", 500),
		alertEntry("m2", "m2:5:DOWN", "c3", 2000),
	} {
		if err := enqueueAlert(t, s, a); err != nil {
			t.Fatal(err)
		}
	}

	wantDue := []string{"m1:1000:UP/c1", "m1:1000:DOWN/c1", "m1:2000:DOWN/c1"}
	if got := dueKeys(t, s, 1000, 10); !slices.Equal(got, wantDue) {
		t.Errorf("DueAlerts(1000) = %v, want %v", got, wantDue)
	}
	if got := dueKeys(t, s, 1000, 2); !slices.Equal(got, wantDue[:2]) {
		t.Errorf("DueAlerts(1000, limit 2) = %v, want %v", got, wantDue[:2])
	}

	editAlert(t, s, "m1:1000:DOWN", "c1", func(a *store.AlertDelivery) {
		a.Attempts, a.NextAttemptMs, a.LastError, a.UpdatedMs = 1, 9000, "timeout", 1500
	})
	editAlert(t, s, "m1:1000:UP", "c1", func(a *store.AlertDelivery) {
		a.State, a.Attempts, a.UpdatedMs = store.AlertDelivered, 1, 1700
	})
	editAlert(t, s, "m1:2000:DOWN", "c1", func(a *store.AlertDelivery) {
		a.State, a.Attempts, a.LastError, a.UpdatedMs = store.AlertFailed, 3, "gave up", 1600
	})
	got, err = s.GetAlert(ctx, "m1:1000:DOWN", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempts != 1 || got.NextAttemptMs != 9000 || got.LastError != "timeout" || got.UpdatedMs != 1500 || got.State != store.AlertPending {
		t.Errorf("after UpdateAlert: %+v", got)
	}
	// Only pending entries are due.
	if got := dueKeys(t, s, 10_000, 10); !slices.Equal(got, []string{"m2:5:DOWN/c3", "m1:1000:DOWN/c2", "m1:1000:DOWN/c1"}) {
		t.Errorf("DueAlerts(10000) = %v, want pending entries ordered by time", got)
	}

	// Pruning removes finished entries updated before the cut-off, never pending ones.
	if err := update(t, s, func(ctx context.Context, tx store.StateTx) error {
		return tx.PruneAlerts(ctx, 1650)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAlert(ctx, "m1:2000:DOWN", "c1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("failed entry before cut-off survived pruning: %v", err)
	}
	if _, err := s.GetAlert(ctx, "m1:1000:UP", "c1"); err != nil {
		t.Errorf("delivered entry after cut-off pruned: %v", err)
	}
	if _, err := s.GetAlert(ctx, "m1:1000:DOWN", "c1"); err != nil {
		t.Errorf("old pending entry pruned: %v", err)
	}
	if err := update(t, s, func(ctx context.Context, tx store.StateTx) error {
		return tx.PruneAlerts(ctx, 1700) // updated exactly at the cut-off: kept
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAlert(ctx, "m1:1000:UP", "c1"); err != nil {
		t.Errorf("entry updated at the cut-off pruned: %v", err)
	}
	if err := update(t, s, func(ctx context.Context, tx store.StateTx) error {
		return tx.PruneAlerts(ctx, 1701)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAlert(ctx, "m1:1000:UP", "c1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delivered entry before cut-off survived pruning: %v", err)
	}

	// Deleting a channel or a monitor removes its outbox entries.
	if err := delChannel(t, s, tenantA, "c2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAlert(ctx, "m1:1000:DOWN", "c2"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("entry of deleted channel: %v, want ErrNotFound", err)
	}
	if err := delChannel(t, s, tenantA, "c1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAlert(ctx, "m1:1000:DOWN", "c1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("pending entry of deleted channel: %v, want ErrNotFound", err)
	}
	if err := del(t, s, tenantA, "m2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAlert(ctx, "m2:5:DOWN", "c3"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("entry of deleted monitor: %v, want ErrNotFound", err)
	}
	if got := dueKeys(t, s, 10_000, 10); len(got) != 0 {
		t.Errorf("DueAlerts after cascades = %v, want none", got)
	}
}

func testChannelTenantIsolation(t *testing.T, s store.State) {
	ctx := context.Background()
	seedTenants(t, s, tenantA, tenantB)
	if err := putChannel(t, s, newChannel(tenantA, "ch-a"), 0); err != nil {
		t.Fatal(err)
	}

	if _, err := s.GetChannel(ctx, tenantB, "ch-a"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetChannel across tenants error = %v, want ErrNotFound", err)
	}
	if list, err := s.ListChannels(ctx, tenantB); err != nil || len(list) != 0 {
		t.Errorf("ListChannels for other tenant = %d channels, %v", len(list), err)
	}
	if err := delChannel(t, s, tenantB, "ch-a"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete across tenants error = %v, want ErrNotFound", err)
	}
	if _, err := s.GetChannel(ctx, tenantA, "ch-a"); err != nil {
		t.Errorf("owner's channel gone after foreign delete: %v", err)
	}

	if err := putChannel(t, s, newChannel(tenantB, "ch-a"), 0); err == nil {
		t.Error("channel ID of another tenant reused")
	}
	hijack := newChannel(tenantB, "ch-a")
	hijack.Name = "hijacked"
	if err := putChannel(t, s, hijack, 1); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("update across tenants error = %v, want ErrNotFound", err)
	}
	got, err := s.GetChannel(ctx, tenantA, "ch-a")
	if err != nil || got.Name != "Channel ch-a" || got.Revision != 1 {
		t.Errorf("owner's channel changed by other tenant: %+v, %v", got, err)
	}
}

// --- Maintenance windows (FA-MNT-01) ------------------------------------------

func newWindow(tenantID, id string) store.MaintenanceWindow {
	return store.MaintenanceWindow{
		TenantID:    tenantID,
		ID:          id,
		Name:        "Window " + id,
		Description: "weekly patch",
		MonitorIDs:  []string{"m2", "m1"},
		Schedule: store.MaintenanceSchedule{
			Kind:            store.ScheduleRecurring,
			Cron:            "0 2 * * 1",
			DurationMinutes: 60,
			Timezone:        "Europe/Berlin",
		},
		Active:    true,
		CreatedMs: 1000,
		UpdatedMs: 1000,
	}
}

func putWindow(t *testing.T, s store.State, w store.MaintenanceWindow, expectedRevision uint64) error {
	t.Helper()
	return update(t, s, func(ctx context.Context, tx store.StateTx) error {
		return tx.PutMaintenanceWindow(ctx, w, expectedRevision)
	})
}

func delWindow(t *testing.T, s store.State, tenantID, id string) error {
	t.Helper()
	return update(t, s, func(ctx context.Context, tx store.StateTx) error {
		return tx.DeleteMaintenanceWindow(ctx, tenantID, id)
	})
}

func testMaintenanceWindows(t *testing.T, s store.State) {
	ctx := context.Background()
	seedTenants(t, s, tenantA)

	w := newWindow(tenantA, "mw-b")
	if err := putWindow(t, s, w, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetMaintenanceWindow(ctx, tenantA, "mw-b")
	if err != nil {
		t.Fatal(err)
	}
	if got.TenantID != tenantA || got.ID != "mw-b" || got.Name != w.Name || got.Description != "weekly patch" ||
		!got.Active || got.Revision != 1 || got.CreatedMs != 1000 || got.UpdatedMs != 1000 {
		t.Errorf("unexpected window: %+v", got)
	}
	if !slices.Equal(got.MonitorIDs, []string{"m1", "m2"}) {
		t.Errorf("monitor IDs = %v, want normalized [m1 m2]", got.MonitorIDs)
	}
	if got.Schedule != w.Schedule {
		t.Errorf("schedule = %+v, want %+v", got.Schedule, w.Schedule)
	}

	once := newWindow(tenantA, "mw-once")
	once.Schedule = store.MaintenanceSchedule{Kind: store.ScheduleOnce, StartsMs: 5000, EndsMs: 9000}
	once.MonitorIDs = []string{"", "m3", "m3", "m1"}
	once.Active = false
	if err := putWindow(t, s, once, 0); err != nil {
		t.Fatalf("create once: %v", err)
	}
	got, err = s.GetMaintenanceWindow(ctx, tenantA, "mw-once")
	if err != nil {
		t.Fatal(err)
	}
	if got.Schedule != once.Schedule || got.Active || !slices.Equal(got.MonitorIDs, []string{"m1", "m3"}) {
		t.Errorf("once window = %+v", got)
	}

	if err := putWindow(t, s, w, 0); !errors.Is(err, store.ErrConflict) {
		t.Errorf("duplicate create error = %v, want ErrConflict", err)
	}

	w.Name = "Beta"
	w.Active = false
	w.MonitorIDs = nil
	w.UpdatedMs = 2000
	w.CreatedMs = 9999 // must be ignored on update
	if err := putWindow(t, s, w, 1); err != nil {
		t.Fatalf("update at revision 1: %v", err)
	}
	got, err = s.GetMaintenanceWindow(ctx, tenantA, "mw-b")
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 2 || got.Name != "Beta" || got.Active || len(got.MonitorIDs) != 0 ||
		got.UpdatedMs != 2000 || got.CreatedMs != 1000 {
		t.Errorf("after update: %+v", got)
	}
	if err := putWindow(t, s, w, 1); !errors.Is(err, store.ErrConflict) {
		t.Errorf("stale update error = %v, want ErrConflict", err)
	}
	if err := putWindow(t, s, newWindow(tenantA, "missing"), 1); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("update of missing window error = %v, want ErrNotFound", err)
	}

	// The list is ordered by name, not by creation time.
	gamma := newWindow(tenantA, "mw-c")
	gamma.Name = "Gamma"
	alpha := newWindow(tenantA, "mw-a")
	alpha.Name = "Alpha"
	for _, mw := range []store.MaintenanceWindow{gamma, alpha} {
		if err := putWindow(t, s, mw, 0); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListMaintenanceWindows(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, mw := range list {
		names = append(names, mw.Name)
	}
	if !slices.Equal(names, []string{"Alpha", "Beta", "Gamma", "Window mw-once"}) {
		t.Errorf("window names = %v, want ordered by name", names)
	}

	// ListAll returns windows of every tenant ordered by tenant ID and ID.
	seedTenants(t, s, tenantB)
	if err := putWindow(t, s, newWindow(tenantB, "mw-x"), 0); err != nil {
		t.Fatal(err)
	}
	all, err := s.ListAllMaintenanceWindows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, mw := range all {
		keys = append(keys, mw.TenantID+"/"+mw.ID)
	}
	want := []string{tenantA + "/mw-a", tenantA + "/mw-b", tenantA + "/mw-c", tenantA + "/mw-once", tenantB + "/mw-x"}
	if !slices.Equal(keys, want) {
		t.Errorf("ListAll = %v, want %v", keys, want)
	}

	if err := delWindow(t, s, tenantA, "mw-b"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetMaintenanceWindow(ctx, tenantA, "mw-b"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("get after delete error = %v, want ErrNotFound", err)
	}
	if err := delWindow(t, s, tenantA, "mw-b"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second delete error = %v, want ErrNotFound", err)
	}
}

func testMaintenanceTenantIsolation(t *testing.T, s store.State) {
	ctx := context.Background()
	seedTenants(t, s, tenantA, tenantB)
	if err := putWindow(t, s, newWindow(tenantA, "mw-a"), 0); err != nil {
		t.Fatal(err)
	}

	if _, err := s.GetMaintenanceWindow(ctx, tenantB, "mw-a"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetMaintenanceWindow across tenants error = %v, want ErrNotFound", err)
	}
	if list, err := s.ListMaintenanceWindows(ctx, tenantB); err != nil || len(list) != 0 {
		t.Errorf("ListMaintenanceWindows for other tenant = %d windows, %v", len(list), err)
	}
	if err := delWindow(t, s, tenantB, "mw-a"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete across tenants error = %v, want ErrNotFound", err)
	}
	if _, err := s.GetMaintenanceWindow(ctx, tenantA, "mw-a"); err != nil {
		t.Errorf("owner's window gone after foreign delete: %v", err)
	}

	if err := putWindow(t, s, newWindow(tenantB, "mw-a"), 0); err == nil {
		t.Error("window ID of another tenant reused")
	}
	hijack := newWindow(tenantB, "mw-a")
	hijack.Name = "hijacked"
	if err := putWindow(t, s, hijack, 1); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("update across tenants error = %v, want ErrNotFound", err)
	}
	got, err := s.GetMaintenanceWindow(ctx, tenantA, "mw-a")
	if err != nil || got.Name != "Window mw-a" || got.Revision != 1 {
		t.Errorf("owner's window changed by other tenant: %+v, %v", got, err)
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

func testLatestHeartbeats(t *testing.T, m store.Metrics) {
	ctx := context.Background()
	insert(t, m, heartbeats(tenantA, "m1", "n1", 1, 5, 10_000))
	insert(t, m, heartbeats(tenantA, "m1", "n2", 1, 2, 12_500))
	insert(t, m, heartbeats(tenantA, "m2", "n1", 10, 3, 20_000))
	insert(t, m, heartbeats(tenantB, "m1", "n1", 20, 3, 30_000))

	got, err := m.LatestHeartbeats(ctx, tenantA, "m1", "", 3)
	if err != nil {
		t.Fatal(err)
	}
	var ts []int64
	for _, hb := range got {
		ts = append(ts, hb.TsMs)
	}
	if !slices.Equal(ts, []int64{14_000, 13_500, 13_000}) {
		t.Fatalf("latest of all nodes = %v", ts)
	}
	got, err = m.LatestHeartbeats(ctx, tenantA, "m1", "n2", 10)
	if err != nil || len(got) != 2 || got[0].Seq != 2 || got[0].NodeID != "n2" {
		t.Fatalf("latest of n2 = %+v, %v", got, err)
	}
	if got, _ := m.LatestHeartbeats(ctx, tenantB, "m2", "", 10); len(got) != 0 {
		t.Fatalf("tenant B sees tenant A's heartbeats: %+v", got)
	}
}

func testRollups(t *testing.T, m store.Metrics) {
	ctx := context.Background()
	if wm, err := m.Watermark(ctx, "x"); err != nil || wm != 0 {
		t.Fatalf("unset watermark = %d, %v", wm, err)
	}
	r := func(tenant, node string, bucket, start, up int64) store.Rollup {
		return store.Rollup{TenantID: tenant, MonitorID: "m1", NodeID: node, BucketMs: bucket, StartMs: start, UpMs: up,
			UnknownMs: bucket - up, LatencyCount: 2, LatencyAvg: 1.5, LatencyP95: 2}
	}
	err := m.PutRollups(ctx, []store.Rollup{
		r(tenantA, "", store.BucketHour, 0, 10), r(tenantA, "", store.BucketHour, 3_600_000, 20),
		r(tenantA, "n1", store.BucketHour, 0, 30), r(tenantA, "", store.BucketDay, 0, 40),
	}, map[string]int64{"x": 7})
	if err != nil {
		t.Fatal(err)
	}
	if wm, _ := m.Watermark(ctx, "x"); wm != 7 {
		t.Fatalf("watermark = %d", wm)
	}
	// Replacing a rollup updates it.
	if err := m.PutRollups(ctx, []store.Rollup{r(tenantA, "", store.BucketHour, 0, 11)}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := m.ListRollups(ctx, tenantA, store.RollupQuery{MonitorID: "m1", BucketMs: store.BucketHour, ToMs: 1 << 62})
	if err != nil || len(got) != 2 || got[0].UpMs != 11 || got[1].UpMs != 20 || got[0].LatencyAvg != 1.5 {
		t.Fatalf("consensus hours = %+v, %v", got, err)
	}
	if got, _ := m.ListRollups(ctx, tenantA, store.RollupQuery{MonitorID: "m1", AllNodes: true, BucketMs: store.BucketHour, ToMs: 3_600_000}); len(got) != 2 || got[1].NodeID != "n1" {
		t.Fatalf("all nodes = %+v", got)
	}
	if got, _ := m.ListRollups(ctx, tenantB, store.RollupQuery{MonitorID: "m1", AllNodes: true, BucketMs: store.BucketHour, ToMs: 1 << 62}); len(got) != 0 {
		t.Fatalf("tenant B sees %+v", got)
	}
	if err := m.SetWatermark(ctx, "x", 9); err != nil {
		t.Fatal(err)
	}
	if wm, _ := m.Watermark(ctx, "x"); wm != 9 {
		t.Fatalf("watermark after set = %d", wm)
	}

	n, err := m.DeleteRollupsBefore(ctx, store.BucketHour, 3_600_000, 1)
	if err != nil || n != 1 {
		t.Fatalf("first batch = %d, %v", n, err)
	}
	if n, _ := m.DeleteRollupsBefore(ctx, store.BucketHour, 3_600_000, 10); n != 1 {
		t.Fatalf("second batch = %d", n)
	}
	if got, _ := m.ListRollups(ctx, tenantA, store.RollupQuery{MonitorID: "m1", BucketMs: store.BucketDay, ToMs: 1}); len(got) != 1 {
		t.Fatal("deleting hourly rollups touched daily ones")
	}
}

func testCompaction(t *testing.T, m store.Metrics) {
	ctx := context.Background()
	insert(t, m, heartbeats(tenantA, "m1", "n1", 1, 4, 0))
	minute := store.LatencyMinute{TenantID: tenantA, MonitorID: "m1", NodeID: "n1", MinuteMs: 0, Min: 10, Max: 13, Avg: 11.5, P95: 13, OKCount: 4}
	keys := []store.HeartbeatKey{{NodeID: "n1", Incarnation: "inc-n1", Seq: 2}, {NodeID: "n1", Incarnation: "inc-n1", Seq: 3}}
	if err := m.CompactHeartbeats(ctx, []store.LatencyMinute{minute}, keys); err != nil {
		t.Fatal(err)
	}
	page, _ := m.ListHeartbeats(ctx, tenantA, store.HeartbeatQuery{MonitorID: "m1"})
	if len(page.Items) != 2 || page.Items[0].Seq != 1 || page.Items[1].Seq != 4 {
		t.Fatalf("remaining = %+v", page.Items)
	}
	// Repeating an interrupted compaction must not fail on existing minutes.
	minute.OKCount = 1
	if err := m.CompactHeartbeats(ctx, []store.LatencyMinute{minute}, keys); err != nil {
		t.Fatal(err)
	}
	if n, err := m.DeleteLatencyMinutesBefore(ctx, 1, 10); err != nil || n != 1 {
		t.Fatalf("deleted minutes = %d, %v", n, err)
	}
}
