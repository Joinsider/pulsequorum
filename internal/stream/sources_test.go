// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package stream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/api/gen"
	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/scheduler"
	"github.com/joinsider/pulsequorum/internal/store"
)

const (
	tenantA   = "tenant-a"
	tenantB   = "tenant-b"
	monitorA  = "0b7b1a52-9d3e-4f3a-9a4e-6f1c2d3e4f50"
	monitorB  = "1c8c2b63-0e4f-4a4b-8b5f-7a2d3e4f5061"
	incidentA = "2d9d3c74-1f50-4b5c-9c6a-8b3e4f506172"
	nodeID    = "node-1"
	waitFor   = 2 * time.Second
)

// fakeIncidents is an in-memory IncidentReader that honours the tenant and
// returns incidents newest first, like the store does.
type fakeIncidents struct {
	mu        sync.Mutex
	items     []store.Incident
	getErr    error
	listErr   error
	getCalls  int
	listCalls int
}

func (f *fakeIncidents) GetIncident(_ context.Context, tenantID, id string) (store.Incident, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	if f.getErr != nil {
		return store.Incident{}, f.getErr
	}
	for _, inc := range f.items {
		if inc.TenantID == tenantID && inc.ID == id {
			return inc, nil
		}
	}
	return store.Incident{}, store.ErrNotFound
}

func (f *fakeIncidents) ListIncidents(_ context.Context, tenantID string, q store.IncidentQuery) (store.IncidentPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listErr != nil {
		return store.IncidentPage{}, f.listErr
	}
	var out []store.Incident
	for i := len(f.items) - 1; i >= 0; i-- {
		inc := f.items[i]
		if inc.TenantID != tenantID || (q.MonitorID != "" && inc.MonitorID != q.MonitorID) {
			continue
		}
		out = append(out, inc)
		if q.Limit > 0 && len(out) == q.Limit {
			break
		}
	}
	return store.IncidentPage{Items: out}, nil
}

// recv returns the next event of sub, decoded, or fails the test on timeout.
func recv(t *testing.T, sub *Subscription) gen.StreamEvent {
	t.Helper()
	select {
	case data := <-sub.Events():
		var ev gen.StreamEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		return ev
	case <-time.After(waitFor):
		t.Fatal("timed out waiting for event")
	}
	return gen.StreamEvent{}
}

// expectNone fails if sub has a pending event.
func expectNone(t *testing.T, sub *Subscription) {
	t.Helper()
	select {
	case data := <-sub.Events():
		t.Fatalf("unexpected event: %s", data)
	default:
	}
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestHeartbeatSinkRecord(t *testing.T) {
	started := time.Date(2026, 10, 21, 8, 30, 0, 123_000_000, time.FixedZone("CEST", 2*3600))
	tests := []struct {
		name       string
		result     checker.Result
		wantStatus gen.CheckStatus
		wantEC     *gen.ErrorClass
		wantMsg    *string
	}{
		{
			name:       "up without error",
			result:     checker.Result{Status: checker.StatusUp, LatencyMs: 12},
			wantStatus: "UP",
		},
		{
			name:       "down with error class and message",
			result:     checker.Result{Status: checker.StatusDown, LatencyMs: 5000, ErrorClass: checker.ClassTimeout, Message: "i/o timeout"},
			wantStatus: "DOWN",
			wantEC:     ptr(gen.ErrorClass("timeout")),
			wantMsg:    ptr("i/o timeout"),
		},
		{
			name:       "degraded",
			result:     checker.Result{Status: checker.StatusDegraded, LatencyMs: 80, Message: "certificate expires soon"},
			wantStatus: "DEGRADED",
			wantMsg:    ptr("certificate expires soon"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := NewHub(0)
			sub := hub.Subscribe(tenantA)
			defer sub.Close()
			sink := &HeartbeatSink{Hub: hub, NodeID: nodeID}

			sink.Record(context.Background(), scheduler.Outcome{
				Monitor: store.Monitor{TenantID: tenantA, ID: monitorA},
				Started: started,
				Result:  tt.result,
			})

			ev := recv(t, sub)
			if ev.Type != gen.StreamEventTypeHeartbeat {
				t.Fatalf("type = %q, want heartbeat", ev.Type)
			}
			if !ev.Ts.Equal(started) || ev.Ts.Location() != time.UTC {
				t.Errorf("ts = %v, want %v in UTC", ev.Ts, started.UTC())
			}
			if ev.MonitorId == nil || ev.MonitorId.String() != monitorA {
				t.Errorf("monitorId = %v, want %s", ev.MonitorId, monitorA)
			}
			hb := ev.Heartbeat
			if hb == nil {
				t.Fatal("heartbeat is nil")
			}
			if hb.NodeId != nodeID {
				t.Errorf("nodeId = %q, want %q", hb.NodeId, nodeID)
			}
			if hb.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q", hb.Status, tt.wantStatus)
			}
			if hb.LatencyMs != tt.result.LatencyMs {
				t.Errorf("latencyMs = %d, want %d", hb.LatencyMs, tt.result.LatencyMs)
			}
			if !equalPtr(hb.ErrorClass, tt.wantEC) {
				t.Errorf("errorClass = %v, want %v", deref(hb.ErrorClass), deref(tt.wantEC))
			}
			if !equalPtr(hb.Message, tt.wantMsg) {
				t.Errorf("message = %v, want %v", deref(hb.Message), deref(tt.wantMsg))
			}
		})
	}
}

func TestHeartbeatSinkIsTenantScoped(t *testing.T) {
	hub := NewHub(0)
	subA := hub.Subscribe(tenantA)
	subB := hub.Subscribe(tenantB)
	defer subA.Close()
	defer subB.Close()
	sink := &HeartbeatSink{Hub: hub, NodeID: nodeID}

	sink.Record(context.Background(), scheduler.Outcome{
		Monitor: store.Monitor{TenantID: tenantA, ID: monitorA}, Started: time.Now(),
		Result: checker.Result{Status: checker.StatusUp},
	})
	sink.Record(context.Background(), scheduler.Outcome{
		Monitor: store.Monitor{TenantID: tenantB, ID: monitorB}, Started: time.Now(),
		Result: checker.Result{Status: checker.StatusDown},
	})

	// Publish is synchronous, so both events are already queued.
	if ev := recv(t, subA); ev.MonitorId == nil || ev.MonitorId.String() != monitorA {
		t.Fatalf("tenant A got monitor %v first, want %s", ev.MonitorId, monitorA)
	}
	expectNone(t, subA)
	if ev := recv(t, subB); ev.MonitorId == nil || ev.MonitorId.String() != monitorB {
		t.Fatalf("tenant B got monitor %v, want %s", ev.MonitorId, monitorB)
	}
	expectNone(t, subB)
}

// startBridge runs b until the test ends.
func startBridge(t *testing.T, b *StateBridge) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func TestStateBridge(t *testing.T) {
	openedAt := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC).UnixMilli()
	resolvedAt := time.Date(2026, 10, 21, 9, 5, 0, 0, time.UTC).UnixMilli()
	sinceMs := time.Date(2026, 10, 21, 8, 59, 0, 0, time.UTC).UnixMilli()

	openIncident := store.Incident{
		TenantID: tenantA, ID: incidentA, MonitorID: monitorA, MonitorName: "api",
		StartedMs: openedAt, Cause: "connection refused", ErrorClass: "connect", Confirmed: true,
	}
	resolvedIncident := openIncident
	resolvedIncident.ResolvedMs = resolvedAt

	tests := []struct {
		name      string
		incidents []store.Incident
		event     cluster.StateEvent
		wantTypes []gen.StreamEventType
		check     func(t *testing.T, evs []gen.StreamEvent)
	}{
		{
			name:      "state only",
			event:     cluster.StateEvent{TenantID: tenantA, MonitorID: monitorA, Status: store.StatusUp, SinceMs: sinceMs},
			wantTypes: []gen.StreamEventType{gen.StreamEventTypeState},
			check: func(t *testing.T, evs []gen.StreamEvent) {
				if evs[0].Status == nil || *evs[0].Status != "UP" {
					t.Errorf("status = %v, want UP", deref(evs[0].Status))
				}
				if !evs[0].Ts.Equal(time.UnixMilli(sinceMs)) || evs[0].Ts.Location() != time.UTC {
					t.Errorf("ts = %v, want %v", evs[0].Ts, time.UnixMilli(sinceMs).UTC())
				}
				if evs[0].MonitorId == nil || evs[0].MonitorId.String() != monitorA {
					t.Errorf("monitorId = %v, want %s", evs[0].MonitorId, monitorA)
				}
			},
		},
		{
			name:      "down with incident opened",
			incidents: []store.Incident{openIncident},
			event:     cluster.StateEvent{TenantID: tenantA, MonitorID: monitorA, Status: store.StatusDown, SinceMs: openedAt, IncidentOpened: incidentA},
			wantTypes: []gen.StreamEventType{gen.StreamEventTypeState, gen.StreamEventTypeIncident},
			check: func(t *testing.T, evs []gen.StreamEvent) {
				inc := evs[1].Incident
				if inc == nil {
					t.Fatal("incident is nil")
				}
				if inc.Id.String() != incidentA || inc.Cause != "connection refused" || inc.MonitorName != "api" {
					t.Errorf("incident = %+v", inc)
				}
				if inc.ResolvedAt != nil {
					t.Errorf("resolvedAt = %v, want nil", inc.ResolvedAt)
				}
				if !evs[1].Ts.Equal(time.UnixMilli(openedAt)) {
					t.Errorf("incident ts = %v, want started time", evs[1].Ts)
				}
			},
		},
		{
			name:      "resolved",
			incidents: []store.Incident{resolvedIncident},
			event:     cluster.StateEvent{TenantID: tenantA, MonitorID: monitorA, Status: store.StatusUp, SinceMs: resolvedAt, IncidentResolved: true},
			wantTypes: []gen.StreamEventType{gen.StreamEventTypeState, gen.StreamEventTypeIncident},
			check: func(t *testing.T, evs []gen.StreamEvent) {
				inc := evs[1].Incident
				if inc == nil || inc.ResolvedAt == nil {
					t.Fatalf("incident or resolvedAt missing: %+v", inc)
				}
				if !inc.ResolvedAt.Equal(time.UnixMilli(resolvedAt)) {
					t.Errorf("resolvedAt = %v, want %v", inc.ResolvedAt, time.UnixMilli(resolvedAt).UTC())
				}
				if !evs[1].Ts.Equal(time.UnixMilli(resolvedAt)) {
					t.Errorf("incident ts = %v, want resolved time", evs[1].Ts)
				}
			},
		},
		{
			name:      "resolved flag but no resolved incident yields only state",
			incidents: []store.Incident{openIncident},
			event:     cluster.StateEvent{TenantID: tenantA, MonitorID: monitorA, Status: store.StatusUp, SinceMs: resolvedAt, IncidentResolved: true},
			wantTypes: []gen.StreamEventType{gen.StreamEventTypeState},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := NewHub(0)
			sub := hub.Subscribe(tenantA)
			defer sub.Close()
			b := NewStateBridge(hub, &fakeIncidents{items: tt.incidents}, discard())
			startBridge(t, b)

			b.Handle(tt.event)

			var evs []gen.StreamEvent
			for range tt.wantTypes {
				evs = append(evs, recv(t, sub))
			}
			expectNone(t, sub)
			for i, want := range tt.wantTypes {
				if evs[i].Type != want {
					t.Fatalf("event %d type = %q, want %q", i, evs[i].Type, want)
				}
			}
			if tt.check != nil {
				tt.check(t, evs)
			}
		})
	}
}

func TestStateBridgeUsesEventTenant(t *testing.T) {
	// The incident exists only under tenant B; a read for tenant A must not find
	// it, and no incident event may reach tenant A's subscribers.
	incB := store.Incident{TenantID: tenantB, ID: incidentA, MonitorID: monitorB, StartedMs: 1, Cause: "x"}
	hub := NewHub(0)
	subA := hub.Subscribe(tenantA)
	defer subA.Close()
	b := NewStateBridge(hub, &fakeIncidents{items: []store.Incident{incB}}, discard())
	startBridge(t, b)

	b.Handle(cluster.StateEvent{TenantID: tenantA, MonitorID: monitorA, Status: store.StatusDown, SinceMs: 1, IncidentOpened: incidentA})

	if ev := recv(t, subA); ev.Type != gen.StreamEventTypeState {
		t.Fatalf("first event type = %q, want state", ev.Type)
	}
	// The state event is queued before the incident lookup runs, so a short
	// wait is enough to see whether an incident event follows.
	select {
	case data := <-subA.Events():
		t.Fatalf("tenant A received cross-tenant incident: %s", data)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestStateBridgeStoreErrorDoesNotStopRun(t *testing.T) {
	hub := NewHub(0)
	sub := hub.Subscribe(tenantA)
	defer sub.Close()
	inc := &fakeIncidents{getErr: errors.New("disk on fire")}
	b := NewStateBridge(hub, inc, discard())
	startBridge(t, b)

	b.Handle(cluster.StateEvent{TenantID: tenantA, MonitorID: monitorA, Status: store.StatusDown, SinceMs: 1, IncidentOpened: incidentA})
	if ev := recv(t, sub); ev.Type != gen.StreamEventTypeState {
		t.Fatalf("type = %q, want state", ev.Type)
	}
	// The state event is published before the incident lookup, so wait until
	// the failing lookup has run before the store is healed.
	deadline := time.Now().Add(2 * time.Second)
	for {
		inc.mu.Lock()
		calls := inc.getCalls
		inc.mu.Unlock()
		if calls > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("incident lookup was not attempted")
		}
		time.Sleep(time.Millisecond)
	}

	inc.mu.Lock()
	inc.getErr = nil
	inc.items = []store.Incident{{TenantID: tenantA, ID: incidentA, MonitorID: monitorA, StartedMs: 1}}
	inc.mu.Unlock()
	b.Handle(cluster.StateEvent{TenantID: tenantA, MonitorID: monitorA, Status: store.StatusUp, SinceMs: 2})
	if ev := recv(t, sub); ev.Type != gen.StreamEventTypeState || ev.Status == nil || *ev.Status != "UP" {
		t.Fatalf("after store error got %+v, want UP state event", ev)
	}
}

func TestStateBridgeHandleDoesNotBlock(t *testing.T) {
	b := NewStateBridge(NewHub(0), &fakeIncidents{}, discard())
	// Run is not started: the queue fills up and Handle must keep returning.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			b.Handle(cluster.StateEvent{TenantID: tenantA, MonitorID: monitorA, Status: store.StatusUp, SinceMs: int64(i)})
		}
	}()
	select {
	case <-done:
	case <-time.After(waitFor):
		t.Fatal("Handle blocked on a full queue")
	}
	if got, want := b.dropped.Load(), uint64(2000-stateQueueSize); got != want {
		t.Errorf("dropped = %d, want %d", got, want)
	}
}

func TestStateBridgeRunStopsOnCancel(t *testing.T) {
	b := NewStateBridge(NewHub(0), &fakeIncidents{}, discard())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Run(ctx)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(waitFor):
		t.Fatal("Run did not return after cancel")
	}
}

func ptr[T any](v T) *T { return &v }

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

func equalPtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
