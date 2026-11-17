// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package degraded

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joinsider/uptimecluster/internal/alert"
	"github.com/joinsider/uptimecluster/internal/checker"
	"github.com/joinsider/uptimecluster/internal/cluster"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
)

const (
	testTenant = "t1"
	testNode   = "node-a"
)

// fakeState is a replicated state with the reads the applier uses.
type fakeState struct {
	store.State // other methods panic: the applier must not call them
	mu          sync.Mutex
	monitors    map[string]store.Monitor
	channels    map[string]store.NotificationChannel
}

func (s *fakeState) GetMonitor(_ context.Context, tenantID, id string) (store.Monitor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.monitors[tenantID+"/"+id]
	if !ok {
		return store.Monitor{}, store.ErrNotFound
	}
	return m, nil
}

func (s *fakeState) GetChannel(_ context.Context, tenantID, id string) (store.NotificationChannel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.channels[tenantID+"/"+id]
	if !ok {
		return store.NotificationChannel{}, store.ErrNotFound
	}
	return ch, nil
}

func (s *fakeState) ListMonitors(_ context.Context, tenantID string, _ store.MonitorQuery) (store.MonitorPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var items []store.Monitor
	for _, m := range s.monitors {
		if m.TenantID == tenantID {
			items = append(items, m)
		}
	}
	return store.MonitorPage{Items: items}, nil
}

// fakeMetrics records the rows of the local log.
type fakeMetrics struct {
	store.Metrics
	mu   sync.Mutex
	rows []store.DegradedAlert
}

func (m *fakeMetrics) InsertDegradedAlert(_ context.Context, a store.DegradedAlert) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = append(m.rows, a)
	return int64(len(m.rows)), nil
}

func (m *fakeMetrics) all() []store.DegradedAlert {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.rows)
}

// fakeQueue records the deliveries; the test completes them.
type fakeQueue struct {
	mu   sync.Mutex
	dels []alert.LocalDelivery
}

func (q *fakeQueue) Enqueue(d alert.LocalDelivery) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.dels = append(q.dels, d)
}

// take returns the pending deliveries and forgets them.
func (q *fakeQueue) take() []alert.LocalDelivery {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.dels
	q.dels = nil
	return out
}

// plainKeyring treats the stored value as the plaintext. The ciphertext "bad"
// cannot be decrypted.
type plainKeyring struct{}

func (plainKeyring) Encrypt(p []byte, _ string) (string, error) { return string(p), nil }

func (plainKeyring) Decrypt(_ context.Context, c, _ string) ([]byte, error) {
	if c == "bad" {
		return nil, errors.New("decrypt: wrong key")
	}
	return []byte(c), nil
}

// rig wires an applier to the fakes and a controllable clock.
type rig struct {
	t        *testing.T
	state    *fakeState
	metrics  *fakeMetrics
	queue    *fakeQueue
	a        *Applier
	now      time.Time
	selfFail bool
	det      State
}

// newRig creates the channels c-down (down and up), c-up (up only), c-off
// (disabled, down) and c-chat (down), and the given monitors.
func newRig(t *testing.T, window time.Duration, monitors ...store.Monitor) *rig {
	t.Helper()
	r := &rig{
		t:       t,
		state:   &fakeState{monitors: map[string]store.Monitor{}, channels: map[string]store.NotificationChannel{}},
		metrics: &fakeMetrics{},
		queue:   &fakeQueue{},
		now:     time.Unix(1_700_000_000, 0),
		det:     State{Degraded: true, Partition: []string{testNode, "node-b"}, Sender: testNode, Reachable: 2, Total: 3},
	}
	r.channel("c-down", store.ChannelOK, store.EventDown, store.EventUp)
	r.channel("c-up", store.ChannelOK, store.EventUp)
	r.channel("c-off", store.ChannelDisabled, store.EventDown)
	r.channel("c-chat", store.ChannelOK, store.EventDown)
	r.channel("c-all", store.ChannelOK, store.EventDown, store.EventUp, store.EventDegraded, store.EventPartial)
	for _, m := range monitors {
		r.state.monitors[m.TenantID+"/"+m.ID] = m
	}
	r.a = NewApplier(ApplierOptions{
		NodeID: testNode, State: r.state, Metrics: r.metrics, Keyring: plainKeyring{}, Dispatcher: r.queue,
		Detector:        func() State { return r.det },
		SelfCheckFailed: func() bool { return r.selfFail },
		BundleWindow:    window,
		Now:             func() time.Time { return r.now },
		Logger:          slog.New(slog.DiscardHandler),
	})
	return r
}

func (r *rig) channel(id, state string, events ...string) {
	r.state.channels[testTenant+"/"+id] = store.NotificationChannel{
		TenantID: testTenant, ID: id, Name: id, Kind: store.ChannelWebhook, URLEnc: "https://hooks.example.com/" + id,
		Events: events, State: state,
	}
}

// monitor returns an UP monitor with the given channels.
func monitor(id string, channels ...string) store.Monitor {
	return store.Monitor{
		TenantID: testTenant, ID: id, Name: "name-" + id, Type: "http", ChannelIDs: channels,
		State: store.MonitorState{Status: store.StatusUp, Confirmed: true, SinceMs: 1000},
	}
}

// apply proposes a status change the way the aggregator does.
func (r *rig) apply(monitorID string, from, to store.MonitorStatus, sinceMs int64, reporters ...string) error {
	r.t.Helper()
	c := cluster.StateChange{TenantID: testTenant, MonitorID: monitorID, From: from, To: to, SinceMs: sinceMs,
		Reporters: reporters, SenderNodeID: testNode}
	if to == store.StatusDown {
		c.Incident = &store.Incident{ID: "inc-" + monitorID, Cause: "connection refused", ErrorClass: string(checker.ClassConnect)}
	}
	_, err := r.a.Apply(context.Background(), cluster.NewSetMonitorState(c))
	return err
}

// down applies UP to DOWN of monitorID at since, reported by the partition.
func (r *rig) down(monitorID string, sinceMs int64) {
	r.t.Helper()
	if err := r.apply(monitorID, store.StatusUp, store.StatusDown, sinceMs, testNode, "node-b"); err != nil {
		r.t.Fatalf("apply DOWN of %s: %v", monitorID, err)
	}
}

// complete reports every pending delivery as sent and returns them.
func (r *rig) complete() []alert.LocalDelivery {
	r.t.Helper()
	dels := r.queue.take()
	for _, d := range dels {
		d.Done(alert.LocalOutcomeSent, nil)
	}
	return dels
}

// rowsWith returns the rows with the outcome.
func (r *rig) rowsWith(outcome string) []store.DegradedAlert {
	var out []store.DegradedAlert
	for _, row := range r.metrics.all() {
		if row.Outcome == outcome {
			out = append(out, row)
		}
	}
	return out
}

// TestApplierRecordsOverlayWithoutError checks that a decided change is
// recorded locally and reported as success, and that the replicated state is
// left alone.
func TestApplierRecordsOverlayWithoutError(t *testing.T) {
	r := newRig(t, 0, monitor("m1", "c-down"))
	if err := r.apply("m1", store.StatusUp, store.StatusDown, 2000, testNode); err != nil {
		t.Fatalf("Apply = %v, want nil for a decided change", err)
	}
	m, err := r.a.opts.Overlay.GetMonitor(context.Background(), testTenant, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if m.State.Status != store.StatusDown || m.State.SinceMs != 2000 || m.State.Confirmed {
		t.Errorf("overlay state = %+v, want DOWN since 2000 unconfirmed", m.State)
	}
	if rep := r.state.monitors[testTenant+"/m1"].State; rep.Status != store.StatusUp {
		t.Errorf("replicated state changed to %+v", rep)
	}

	r.a.opts.Overlay.Reset()
	m, _ = r.a.opts.Overlay.GetMonitor(context.Background(), testTenant, "m1")
	if m.State.Status != store.StatusUp {
		t.Errorf("after Reset status = %s, want the replicated UP", m.State.Status)
	}
}

// TestApplierDownPayload checks the alert of a DOWN change: its fields, the
// sender view and the single channel that subscribes.
func TestApplierDownPayload(t *testing.T) {
	r := newRig(t, 0, monitor("m1", "c-down", "c-up", "c-off"))
	r.det.Reachable, r.det.Total = 2, 3
	if err := r.apply("m1", store.StatusUp, store.StatusDown, 2000, testNode, "node-b"); err != nil {
		t.Fatal(err)
	}
	dels := r.queue.take()
	if len(dels) != 1 {
		t.Fatalf("deliveries = %d, want 1 (c-up does not subscribe to down, c-off is disabled)", len(dels))
	}
	d := dels[0]
	if d.ChannelID != "c-down" || d.TenantID != testTenant {
		t.Errorf("channel = %s/%s, want %s/c-down", d.TenantID, d.ChannelID, testTenant)
	}
	if d.Target.URL != "https://hooks.example.com/c-down" || d.Target.Kind != store.ChannelWebhook {
		t.Errorf("target = %+v", d.Target)
	}
	p := d.Payload
	want := cluster.AlertPayload{
		AlertID: "degraded:node-a:m1:2000:DOWN", TenantID: testTenant, MonitorID: "m1", MonitorName: "name-m1",
		MonitorType: "http", Event: store.EventDown, Status: "DOWN", PreviousStatus: "UP", SinceMs: 2000,
		Cause: "connection refused", ErrorClass: string(checker.ClassConnect), IncidentID: "inc-m1", Confirmed: false,
		Sender: &cluster.AlertSender{NodeID: testNode, ReachableVoters: 2, TotalVoters: 3,
			Reporters: []string{testNode, "node-b"}},
	}
	if p.AlertID != want.AlertID || p.Event != want.Event || p.Status != want.Status || p.PreviousStatus != want.PreviousStatus ||
		p.SinceMs != want.SinceMs || p.Cause != want.Cause || p.IncidentID != want.IncidentID || p.Confirmed ||
		p.MonitorName != want.MonitorName || p.MonitorType != want.MonitorType || p.ErrorClass != want.ErrorClass {
		t.Errorf("payload = %+v, want %+v", p, want)
	}
	if p.Sender == nil || p.Sender.NodeID != want.Sender.NodeID || p.Sender.ReachableVoters != want.Sender.ReachableVoters ||
		p.Sender.TotalVoters != want.Sender.TotalVoters || p.Sender.Confirmed ||
		!slices.Equal(p.Sender.Reporters, want.Sender.Reporters) {
		t.Errorf("sender = %+v, want %+v", p.Sender, want.Sender)
	}
}

// TestApplierTargets checks which channels get the alert of an event.
func TestApplierTargets(t *testing.T) {
	tests := []struct {
		name     string
		from, to store.MonitorStatus
		channels []string
		want     []string
	}{
		{"down goes to subscribed enabled channels", store.StatusUp, store.StatusDown,
			[]string{"c-down", "c-up", "c-off", "c-chat"}, []string{"c-down", "c-chat"}},
		{"recovery goes to channels that subscribe to up", store.StatusDown, store.StatusUp,
			[]string{"c-down", "c-up", "c-off"}, []string{"c-down", "c-up"}},
		{"event without subscriber sends nothing", store.StatusUp, store.StatusDown,
			[]string{"c-up"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t, 0, monitor("m1", tt.channels...))
			if tt.from == store.StatusDown {
				r.state.monitors[testTenant+"/m1"] = withStatus(r.state.monitors[testTenant+"/m1"], store.StatusDown)
			}
			if err := r.apply("m1", tt.from, tt.to, 2000, testNode); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, d := range r.queue.take() {
				got = append(got, d.ChannelID)
			}
			slices.Sort(got)
			slices.Sort(tt.want)
			if !slices.Equal(got, tt.want) {
				t.Errorf("channels = %v, want %v", got, tt.want)
			}
		})
	}
}

func withStatus(m store.Monitor, s store.MonitorStatus) store.Monitor {
	m.State.Status = s
	return m
}

// TestApplierRecoveryPayload checks the up alert after DOWN.
func TestApplierRecoveryPayload(t *testing.T) {
	m := withStatus(monitor("m1", "c-down"), store.StatusDown)
	r := newRig(t, 0, m)
	if err := r.apply("m1", store.StatusDown, store.StatusUp, 3000, testNode); err != nil {
		t.Fatal(err)
	}
	dels := r.queue.take()
	if len(dels) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(dels))
	}
	p := dels[0].Payload
	if p.Event != store.EventUp || p.Status != "UP" || p.PreviousStatus != "DOWN" || p.AlertID != "degraded:node-a:m1:3000:UP" {
		t.Errorf("payload = %+v", p)
	}
	if p.Cause != "" || p.IncidentID != "" {
		t.Errorf("recovery carries an incident: %+v", p)
	}
}

// TestApplierNoAlertWithoutEvent checks that a change without event is recorded only.
func TestApplierNoAlertWithoutEvent(t *testing.T) {
	r := newRig(t, 0, monitor("m1", "c-down"))
	if err := r.apply("m1", store.StatusUp, store.StatusPending, 2000, testNode); err != nil {
		t.Fatal(err)
	}
	if n := len(r.queue.take()); n != 0 {
		t.Errorf("deliveries = %d, want 0", n)
	}
	if len(r.metrics.all()) != 0 {
		t.Errorf("rows = %+v, want none", r.metrics.all())
	}
}

// TestApplierConflict checks that a change based on another status is refused
// with ErrConflict, as the FSM does, and nothing is recorded.
func TestApplierConflict(t *testing.T) {
	r := newRig(t, 0, monitor("m1", "c-down"))
	err := r.apply("m1", store.StatusDown, store.StatusUp, 2000, testNode)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Apply = %v, want ErrConflict", err)
	}
	m, _ := r.a.opts.Overlay.GetMonitor(context.Background(), testTenant, "m1")
	if m.State.Status != store.StatusUp {
		t.Errorf("overlay changed by a refused change: %+v", m.State)
	}
	if n := len(r.queue.take()); n != 0 {
		t.Errorf("deliveries = %d, want 0", n)
	}
}

// TestApplierUnknownMonitor checks that a change to a missing monitor returns ErrNotFound.
func TestApplierUnknownMonitor(t *testing.T) {
	r := newRig(t, 0)
	if err := r.apply("nope", store.StatusUp, store.StatusDown, 2000, testNode); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Apply = %v, want ErrNotFound", err)
	}
}

// TestApplierOnlySetMonitorState checks that other commands are refused.
func TestApplierOnlySetMonitorState(t *testing.T) {
	r := newRig(t, 0, monitor("m1", "c-down"))
	cmd := &pb.Command{TenantId: testTenant, Payload: &pb.Command_ClearMonitorFlapping{ClearMonitorFlapping: &pb.ClearMonitorFlapping{}}}
	if _, err := r.a.Apply(context.Background(), cmd); err == nil {
		t.Error("Apply of another command returned nil")
	}
}

// TestApplierFlappingSilent checks that a flapping monitor sends nothing, but
// its local status is still decided.
func TestApplierFlappingSilent(t *testing.T) {
	m := monitor("m1", "c-down")
	m.State.Flapping, m.State.FlappingSinceMs = true, 900
	r := newRig(t, 0, m)
	if err := r.apply("m1", store.StatusUp, store.StatusDown, 2000, testNode); err != nil {
		t.Fatal(err)
	}
	if n := len(r.queue.take()); n != 0 {
		t.Errorf("deliveries = %d, want 0 for a flapping monitor", n)
	}
	if len(r.metrics.all()) != 0 {
		t.Errorf("rows = %+v, want none", r.metrics.all())
	}
	got, _ := r.a.opts.Overlay.GetMonitor(context.Background(), testTenant, "m1")
	if got.State.Status != store.StatusDown || !got.State.Flapping {
		t.Errorf("overlay = %+v, want DOWN and the replicated flapping flag", got.State)
	}
}

// TestApplierSelfCheckSuppresses checks that a failed self-check sends nothing
// and logs one suppressed row per target.
func TestApplierSelfCheckSuppresses(t *testing.T) {
	r := newRig(t, 0, monitor("m1", "c-down", "c-chat"))
	r.selfFail = true
	if err := r.apply("m1", store.StatusUp, store.StatusDown, 2000, testNode); err != nil {
		t.Fatal(err)
	}
	if n := len(r.queue.take()); n != 0 {
		t.Errorf("deliveries = %d, want 0", n)
	}
	rows := r.metrics.all()
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	for _, row := range rows {
		if row.Outcome != OutcomeSuppressed || row.MonitorID != "m1" || row.Status != "DOWN" || row.SinceMs != 2000 ||
			row.Reachable != 2 || row.Total != 3 {
			t.Errorf("row = %+v, want suppressed DOWN m1", row)
		}
	}
}

// TestApplierRowsWithOutcomes checks that the log row of a delivery gets the
// outcome reported by the dispatcher.
func TestApplierRowsWithOutcomes(t *testing.T) {
	r := newRig(t, 0, monitor("m1", "c-down", "c-chat"))
	if err := r.apply("m1", store.StatusUp, store.StatusDown, 2000, testNode, "node-b"); err != nil {
		t.Fatal(err)
	}
	dels := r.queue.take()
	if len(dels) != 2 {
		t.Fatalf("deliveries = %d, want 2", len(dels))
	}
	dels[0].Done(alert.LocalOutcomeSent, nil)
	dels[1].Done(alert.LocalOutcomeFailed, errors.New("webhook answered 503"))

	rows := r.metrics.all()
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	outcomes := map[string]string{}
	for _, row := range rows {
		outcomes[row.ChannelID] = row.Outcome
		if row.Reachable != 2 || row.Total != 3 || !slices.Equal(row.Reporters, []string{testNode, "node-b"}) {
			t.Errorf("row = %+v, want the sender view", row)
		}
	}
	if outcomes["c-down"] == "" || outcomes["c-chat"] == "" || outcomes["c-down"] == outcomes["c-chat"] {
		t.Errorf("outcomes = %v, want one sent and one failed", outcomes)
	}
	if len(r.rowsWith(alert.LocalOutcomeSent)) != 1 || len(r.rowsWith(alert.LocalOutcomeFailed)) != 1 {
		t.Errorf("rows = %+v, want one sent and one failed", rows)
	}
}

// TestApplierUnreadableSecretFailsRow checks that a channel whose secrets
// cannot be decrypted gets a failed row and no delivery.
func TestApplierUnreadableSecretFailsRow(t *testing.T) {
	r := newRig(t, 0, monitor("m1", "c-down"))
	ch := r.state.channels[testTenant+"/c-down"]
	ch.URLEnc = "bad"
	r.state.channels[testTenant+"/c-down"] = ch
	if err := r.apply("m1", store.StatusUp, store.StatusDown, 2000, testNode); err != nil {
		t.Fatal(err)
	}
	if n := len(r.queue.take()); n != 0 {
		t.Errorf("deliveries = %d, want 0", n)
	}
	rows := r.metrics.all()
	if len(rows) != 1 || rows[0].Outcome != alert.LocalOutcomeFailed {
		t.Errorf("rows = %+v, want one failed row", rows)
	}
}

// downChanges applies DOWN to the first n of the monitors m1..mTotal at the
// same instant, and completes the deliveries after each change.
func (r *rig) downChanges(_, n int) (singles, summaries int) {
	r.t.Helper()
	for i := 1; i <= n; i++ {
		r.down(fmt.Sprintf("m%d", i), 2000)
		for _, d := range r.complete() {
			if strings.HasSuffix(d.Payload.MonitorName, "monitors") {
				summaries++
			} else {
				singles++
			}
		}
	}
	return singles, summaries
}

// floodMonitors returns total monitors m1..mTotal that subscribe to c-down and c-chat.
func floodMonitors(total int) []store.Monitor {
	var out []store.Monitor
	for i := 1; i <= total; i++ {
		out = append(out, monitor(fmt.Sprintf("m%d", i), "c-down", "c-chat"))
	}
	return out
}

// TestFloodGuard checks the flood guard (PF-28): from the change that makes
// more than half of at least five monitors DOWN within one window, the single
// alerts are suppressed and each channel gets one summary.
func TestFloodGuard(t *testing.T) {
	tests := []struct {
		name          string
		total, n      int
		wantSingles   int // single alerts delivered over all channels
		wantSummaries int // summaries delivered over all channels
	}{
		{"fewer than five monitors never floods", 4, 4, 8, 0},
		{"five of five floods at the fifth", 5, 5, 8, 2},
		{"exactly half does not flood", 10, 5, 10, 0},
		{"more than half floods at the sixth", 10, 6, 10, 2},
		{"all ten: five sent, the rest suppressed", 10, 10, 10, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t, 30*time.Second, floodMonitors(tt.total)...)
			singles, summaries := r.downChanges(tt.total, tt.n)
			if singles != tt.wantSingles || summaries != tt.wantSummaries {
				t.Errorf("singles = %d, summaries = %d; want %d and %d", singles, summaries, tt.wantSingles, tt.wantSummaries)
			}
		})
	}
}

// TestFloodSummaryContent checks the summary and the suppressed rows of a flood.
func TestFloodSummaryContent(t *testing.T) {
	r := newRig(t, 30*time.Second, floodMonitors(10)...)
	r.downChanges(10, 10)

	if n := len(r.rowsWith(alert.LocalOutcomeSent)); n != 10 {
		t.Errorf("sent rows = %d, want 10 (m1..m5 on two channels)", n)
	}
	if n := len(r.rowsWith(OutcomeSuppressed)); n != 10 {
		t.Errorf("suppressed rows = %d, want 10 (m6..m10 on two channels)", n)
	}

	// The summary is sent as its own message and has no log row; only the
	// suppressed alert of m6 is logged, once per channel.
	r2 := newRig(t, 30*time.Second, floodMonitors(10)...)
	for i := 1; i <= 6; i++ {
		r2.down(fmt.Sprintf("m%d", i), 2000)
	}
	if n := len(r2.rowsWith(OutcomeSuppressed)); n != 2 {
		t.Errorf("suppressed rows after m6 = %d, want 2", n)
	}
	var summaries []cluster.AlertPayload
	for _, d := range r2.queue.take() {
		if strings.HasSuffix(d.Payload.MonitorName, "monitors") {
			summaries = append(summaries, d.Payload)
		}
	}
	if len(summaries) != 2 {
		t.Fatalf("summaries = %d, want one per channel", len(summaries))
	}
	for _, p := range summaries {
		want := "6 monitors"
		if p.MonitorName != want || p.Event != store.EventDown || p.Status != "DOWN" || p.Confirmed {
			t.Errorf("summary = %+v, want %s, DOWN, unconfirmed", p, want)
		}
		if p.Cause != "node-a sees 6 monitors DOWN, probably its own network problem" {
			t.Errorf("summary cause = %q", p.Cause)
		}
		if p.Sender == nil || p.Sender.NodeID != testNode || p.Sender.Confirmed {
			t.Errorf("summary sender = %+v", p.Sender)
		}
	}
}

// TestFloodWindowEnds checks that a new bundling window starts a new count.
func TestFloodWindowEnds(t *testing.T) {
	r := newRig(t, 30*time.Second, floodMonitors(10)...)
	r.downChanges(10, 5) // five of ten: no flood
	r.now = r.now.Add(31 * time.Second)

	r.down("m6", 5000)
	dels := r.complete()
	if len(dels) != 2 {
		t.Fatalf("deliveries after the window = %d, want 2 single alerts", len(dels))
	}
	for _, d := range dels {
		if d.Payload.MonitorName != "name-m6" {
			t.Errorf("delivery %s after the window is not the single alert", d.Payload.MonitorName)
		}
	}
}

// TestFloodZeroWindowNeverFloods checks that without a window every change
// counts alone.
func TestFloodZeroWindowNeverFloods(t *testing.T) {
	r := newRig(t, 0, floodMonitors(10)...)
	singles, summaries := r.downChanges(10, 10)
	if singles != 20 || summaries != 0 {
		t.Errorf("singles = %d, summaries = %d; want 20 and 0", singles, summaries)
	}
}

// TestOverlayReplicatedFlappingKept checks the overlay view directly.
func TestOverlayReplicatedFlappingKept(t *testing.T) {
	m := monitor("m1")
	m.State.Flapping, m.State.FlappingSinceMs = true, 77
	st := &fakeState{monitors: map[string]store.Monitor{testTenant + "/m1": m}}
	o := NewOverlay(st)
	o.set(testTenant, "m1", store.StatusDown, 9)
	got, err := o.GetMonitor(context.Background(), testTenant, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State.Status != store.StatusDown || got.State.SinceMs != 9 || got.State.Confirmed ||
		!got.State.Flapping || got.State.FlappingSinceMs != 77 {
		t.Errorf("state = %+v", got.State)
	}
	if _, err := o.GetMonitor(context.Background(), testTenant, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing monitor: err = %v, want ErrNotFound", err)
	}
}

// TestApplierLogsOnlyDownAndUp checks that alerts of other statuses are sent
// but get no row, since the FSM accepts only DOWN and UP entries (ADR 0024, 4).
func TestApplierLogsOnlyDownAndUp(t *testing.T) {
	tests := []struct {
		name     string
		from, to store.MonitorStatus
		wantRows int
	}{
		{"degraded is sent but not logged", store.StatusUp, store.StatusDegraded, 0},
		{"partial is sent but not logged", store.StatusUp, store.StatusPartial, 0},
		{"up is logged", store.StatusDown, store.StatusUp, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := monitor("m1", "c-all")
			if tt.from == store.StatusDown {
				m = withStatus(m, store.StatusDown)
			}
			r := newRig(t, 0, m)
			if err := r.apply("m1", tt.from, tt.to, 2000, testNode); err != nil {
				t.Fatal(err)
			}
			dels := r.complete()
			if len(dels) != 1 {
				t.Fatalf("deliveries = %d, want 1", len(dels))
			}
			if n := len(r.metrics.all()); n != tt.wantRows {
				t.Errorf("rows = %d, want %d", n, tt.wantRows)
			}
		})
	}

	t.Run("self check writes no row for degraded", func(t *testing.T) {
		r := newRig(t, 0, monitor("m1", "c-all"))
		r.selfFail = true
		if err := r.apply("m1", store.StatusUp, store.StatusDegraded, 2000, testNode); err != nil {
			t.Fatal(err)
		}
		if n := len(r.metrics.all()); n != 0 {
			t.Errorf("rows = %d, want 0", n)
		}
	})
}
