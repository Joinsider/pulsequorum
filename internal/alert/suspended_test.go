// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package alert

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/joinsider/uptimecluster/internal/checker"
	"github.com/joinsider/uptimecluster/internal/cluster"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
)

const suspendedTenantID = "acme"

// suspendTestTenant creates a second tenant with a channel and a DOWN monitor,
// and returns the clock and the alert key of its DOWN alert.
func suspendTestTenant(t *testing.T, f *fixture, resendMinutes int) (*clock, string) {
	t.Helper()
	if _, err := f.node.Apply(f.ctx, cluster.NewSetFeatureLevel("test-level", cluster.TenantAdminCommandLevel)); err != nil {
		t.Fatal(err)
	}
	create := &pb.Command{
		RequestId: "test-create-acme",
		Actor:     &pb.Actor{Kind: pb.Actor_KIND_USER},
		Payload: &pb.Command_CreateTenant{CreateTenant: &pb.CreateTenant{
			TenantId: suspendedTenantID, Name: "Acme", PlanId: "default", Source: "admin",
		}},
	}
	if _, err := f.node.Apply(f.ctx, create); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	enc, err := EncryptURL(f.kr, testChannelID, "https://hooks.example.com/secret-path")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := cluster.NewPutChannel("", suspendedTenantID, testChannelID, cluster.ChannelDocument{
		Name: "acme", Kind: store.ChannelWebhook, URLEnc: enc, Events: []string{store.EventDown, store.EventUp},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.node.Apply(f.ctx, ch); err != nil {
		t.Fatal(err)
	}
	doc := cluster.MonitorDocument{Name: "web", Type: "http", IntervalSeconds: 60, TimeoutSeconds: 10,
		RecoveryThreshold: 1, ResendIntervalMinutes: resendMinutes, Assignment: store.Assignment{Mode: "all"},
		ConsensusPolicy: "majority", NotificationChannelIDs: []string{testChannelID}}
	mon, err := cluster.NewPutMonitor("", suspendedTenantID, testMonitorID, doc, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.node.Apply(f.ctx, mon); err != nil {
		t.Fatal(err)
	}
	since := start.UnixMilli() + 1000
	incident := "22222222-2222-2222-2222-222222222222"
	if _, err := f.node.Apply(f.ctx, cluster.NewSetMonitorState(cluster.StateChange{
		TenantID: suspendedTenantID, MonitorID: testMonitorID, From: store.StatusPending, To: store.StatusDown,
		SinceMs: since, Incident: &store.Incident{ID: incident, Cause: "connection refused", ErrorClass: string(checker.ClassConnect)},
	})); err != nil {
		t.Fatal(err)
	}
	return &clock{t: start.Add(time.Minute)}, fmt.Sprintf("%s:%d:DOWN", testMonitorID, since)
}

// setSuspendedTenant suspends or resumes the tenant through the FSM.
func setSuspendedTenant(t *testing.T, f *fixture, tenantID string, suspended bool) {
	t.Helper()
	cmd := &pb.Command{
		RequestId: fmt.Sprintf("test-suspend-%s-%t-%d", tenantID, suspended, time.Now().UnixNano()),
		Actor:     &pb.Actor{Kind: pb.Actor_KIND_USER},
		Payload: &pb.Command_SetTenantSuspended{SetTenantSuspended: &pb.SetTenantSuspended{
			TenantId: tenantID, Suspended: suspended,
		}},
	}
	if _, err := f.node.Apply(f.ctx, cmd); err != nil {
		t.Fatal(err)
	}
}

func TestDeliverDueSkipsSuspendedTenantUntilResumed(t *testing.T) {
	f := newFixture(t)
	clk, key := suspendTestTenant(t, f, 0)
	s := &fakeSender{}
	d := f.dispatcher(s, clk, true, 10)

	setSuspendedTenant(t, f, suspendedTenantID, true)
	if err := d.DeliverDue(f.ctx); err != nil {
		t.Fatal(err)
	}
	if s.count() != 0 {
		t.Fatalf("sent %d alerts of a suspended tenant", s.count())
	}
	a, err := f.state.GetAlert(f.ctx, key, testChannelID)
	if err != nil {
		t.Fatal(err)
	}
	if a.State != store.AlertPending || a.Attempts != 0 {
		t.Fatalf("alert = %+v, want pending without attempts", a)
	}

	setSuspendedTenant(t, f, suspendedTenantID, false)
	if err := d.DeliverDue(f.ctx); err != nil {
		t.Fatal(err)
	}
	if s.count() != 1 {
		t.Fatalf("sent %d alerts after resume, want 1", s.count())
	}
}

func TestDeliverDueReadsPastSuspendedEntries(t *testing.T) {
	f := newFixture(t)
	clk, _ := suspendTestTenant(t, f, 0)
	s := &fakeSender{}
	d := NewDispatcher(DispatcherOptions{
		State: f.state, Cluster: f.node, Keyring: f.kr, Sender: s,
		IsLeader: func() bool { return true }, MaxAttempts: 10, BatchSize: 1,
		Logger: slog.New(slog.DiscardHandler), Now: clk.now, Jitter: func() float64 { return 0.5 },
	})
	setSuspendedTenant(t, f, suspendedTenantID, true)
	if err := d.DeliverDue(f.ctx); err != nil {
		t.Fatal(err)
	}
	if d.skipped != 1 || s.count() != 0 {
		t.Fatalf("skipped = %d, sent = %d; want 1 skipped and nothing sent", d.skipped, s.count())
	}
	// The next round asks for one more entry, so the suspended one does not hide others.
	if got := d.opts.BatchSize + d.skipped; got != 2 {
		t.Fatalf("next batch = %d, want 2", got)
	}
	due, err := f.state.DueAlerts(f.ctx, clk.now().UnixMilli(), d.opts.BatchSize+d.skipped)
	if err != nil || len(due) != 1 {
		t.Fatalf("due = %d, %v; want the suspended entry only", len(due), err)
	}
}

func TestEnqueueRemindersSkipsSuspendedTenant(t *testing.T) {
	f := newFixture(t)
	clk, _ := suspendTestTenant(t, f, 1)
	pendingCount := func(at time.Time) int {
		due, err := f.state.DueAlerts(f.ctx, at.UnixMilli(), 1000)
		if err != nil {
			t.Fatal(err)
		}
		return len(due)
	}
	later := clk.t.Add(time.Hour)

	// Control: an active tenant gets its reminders.
	active := f.dispatcher(&fakeSender{}, &clock{t: later}, true, 10)
	if err := active.EnqueueReminders(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := pendingCount(later)
	if before < 2 {
		t.Fatalf("control: pending = %d, want the DOWN alert and a reminder", before)
	}

	setSuspendedTenant(t, f, suspendedTenantID, true)
	fresh := f.dispatcher(&fakeSender{}, &clock{t: later.Add(time.Hour)}, true, 10)
	if err := fresh.EnqueueReminders(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after := pendingCount(later.Add(time.Hour)); after != before {
		t.Fatalf("pending grew from %d to %d while the tenant is suspended", before, after)
	}
}
