// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package degraded

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/joinsider/uptimecluster/internal/alert"
	"github.com/joinsider/uptimecluster/internal/cluster"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
)

// Outcomes of the node-local log besides the delivery outcomes of
// alert.LocalOutcomeSent and alert.LocalOutcomeFailed (ADR 0024, section 4).
const OutcomeSuppressed = "suppressed"

// Flood guard (ADR 0024, PF-28): a flood needs at least floodMinMonitors DOWN
// changes of one tenant within one bundling window, and more than half of the
// tenant's monitors.
const floodMinMonitors = 5

// rowTimeout bounds the write of one log row.
const rowTimeout = 5 * time.Second

// Overlay is the monitor view of a degraded voter (ADR 0024, section 3): the
// replicated state with the statuses decided locally on top. It implements
// the read side of store.State that the aggregator uses, GetMonitor. The
// other methods are those of the embedded State.
type Overlay struct {
	store.State

	mu    sync.Mutex
	local map[monitorKey]localStatus
}

type monitorKey struct{ tenant, monitor string }

// localStatus is a status decided by this node. It is never confirmed.
type localStatus struct {
	status  store.MonitorStatus
	sinceMs int64
}

// NewOverlay returns an overlay on the replicated state base without local
// statuses.
func NewOverlay(base store.State) *Overlay {
	return &Overlay{State: base, local: make(map[monitorKey]localStatus)}
}

// GetMonitor returns the monitor with the local status on top, if there is one.
// Flapping is taken from the replicated state.
func (o *Overlay) GetMonitor(ctx context.Context, tenantID, id string) (store.Monitor, error) {
	m, err := o.State.GetMonitor(ctx, tenantID, id)
	if err != nil {
		return store.Monitor{}, err
	}
	o.mu.Lock()
	ls, ok := o.local[monitorKey{tenantID, id}]
	o.mu.Unlock()
	if ok {
		m.State = store.MonitorState{
			Status: ls.status, SinceMs: ls.sinceMs, Confirmed: false,
			Flapping: m.State.Flapping, FlappingSinceMs: m.State.FlappingSinceMs,
		}
	}
	return m, nil
}

// set records a status decided locally.
func (o *Overlay) set(tenantID, monitorID string, status store.MonitorStatus, sinceMs int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.local[monitorKey{tenantID, monitorID}] = localStatus{status: status, sinceMs: sinceMs}
}

// Reset forgets all local statuses. It is called when the degraded mode ends
// or this node stops being the sender.
func (o *Overlay) Reset() {
	o.mu.Lock()
	defer o.mu.Unlock()
	clear(o.local)
}

// Queue accepts alerts for delivery. *alert.LocalDispatcher satisfies it.
type Queue interface {
	Enqueue(alert.LocalDelivery)
}

// ApplierOptions configures an Applier. NodeID, State, Metrics, Keyring and
// Dispatcher are required.
type ApplierOptions struct {
	NodeID string
	// State is the read-only replicated state.
	State store.State
	// Overlay holds the local statuses; nil creates one on State.
	Overlay *Overlay
	Metrics store.Metrics
	// Detector returns the degraded state of this node. Nil means an empty
	// state (no reachable voters known).
	Detector func() State
	// SelfCheckFailed reports whether the own self-check failed (ADR 0022).
	// Nil means passed. A failed self-check sends nothing (PF-28).
	SelfCheckFailed func() bool
	Keyring         alert.Keyring
	Dispatcher      Queue
	// BundleWindow is the window of the flood guard. Zero makes every DOWN
	// change its own window, so the guard never triggers.
	BundleWindow time.Duration
	// Now is the clock (default time.Now).
	Now    func() time.Time
	Logger *slog.Logger
}

// Applier implements the aggregator's Applier for the degraded mode. It
// decides status changes like the leader does, but records them in the
// Overlay and delivers the alerts itself instead of proposing them to the
// cluster (ADR 0024, section 3). Only SetMonitorState is supported.
type Applier struct {
	opts ApplierOptions

	// mu guards floods, the DOWN changes per tenant of the current window.
	mu     sync.Mutex
	floods map[string]*floodWindow
}

// floodWindow counts the DOWN changes of one tenant within one window.
type floodWindow struct {
	start   time.Time
	down    map[string]bool
	flooded bool
	// summarized holds the channels that got the summary of this window.
	summarized map[string]bool
}

// NewApplier creates an Applier with defaults applied to opts.
func NewApplier(opts ApplierOptions) *Applier {
	if opts.Overlay == nil {
		opts.Overlay = NewOverlay(opts.State)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Applier{opts: opts, floods: make(map[string]*floodWindow)}
}

// Apply decides a status change of the degraded node. A decided change is not
// an error, so the aggregator does not reload it. A change based on another
// status than the local view returns store.ErrConflict, as the FSM does.
func (a *Applier) Apply(ctx context.Context, cmd *pb.Command) (cluster.ApplyResult, error) {
	set := cmd.GetSetMonitorState()
	if set == nil {
		return cluster.ApplyResult{}, fmt.Errorf("degraded mode supports only SetMonitorState, got %T", cmd.GetPayload())
	}
	tenant, monitorID := cmd.GetTenantId(), set.GetMonitorId()
	cur, err := a.opts.Overlay.GetMonitor(ctx, tenant, monitorID)
	if err != nil {
		return cluster.ApplyResult{}, fmt.Errorf("load monitor %s: %w", monitorID, err)
	}
	if string(cur.State.Status) != set.GetExpectedStatus() {
		return cluster.ApplyResult{}, fmt.Errorf("monitor %s is %s, not %s: %w",
			monitorID, cur.State.Status, set.GetExpectedStatus(), store.ErrConflict)
	}
	from, to := cur.State.Status, store.MonitorStatus(set.GetStatus())
	a.opts.Overlay.set(tenant, monitorID, to, set.GetSinceMs())

	event := alertEvent(from, to)
	if event == "" {
		return cluster.ApplyResult{}, nil
	}
	if cur.State.Flapping {
		// Flapping monitors stay silent; new flapping is not detected here (ADR 0024).
		a.opts.Logger.Debug("degraded: flapping monitor, no alert", "tenant", tenant, "monitor", monitorID)
		return cluster.ApplyResult{}, nil
	}
	a.notify(ctx, cur, set, from, to, event)
	return cluster.ApplyResult{}, nil
}

// alertEvent mirrors cluster's rule for the status changes that are notified
// (ADR 0014). The cluster's function is unexported.
func alertEvent(from, to store.MonitorStatus) string {
	switch to {
	case store.StatusDown:
		return store.EventDown
	case store.StatusUp:
		if from == store.StatusDown {
			return store.EventUp
		}
	case store.StatusDegraded:
		if from == store.StatusDown {
			return store.EventUp
		}
		return store.EventDegraded
	case store.StatusPartial:
		return store.EventPartial
	}
	return ""
}

// notify delivers or logs the alert of a status change for every channel of
// the monitor that subscribes to the event.
func (a *Applier) notify(ctx context.Context, m store.Monitor, set *pb.SetMonitorState, from, to store.MonitorStatus, event string) {
	// Every DOWN change counts for the flood guard, also without channels.
	flooded := to == store.StatusDown && a.flooded(ctx, m.TenantID, m.ID)
	chans := a.channels(ctx, m, event)
	if len(chans) == 0 {
		return
	}
	// The FSM accepts only DOWN and UP entries in a degraded report (ADR 0024,
	// section 4). Alerts for other statuses are sent but not logged.
	logged := to == store.StatusDown || to == store.StatusUp
	st := a.detector()
	now := a.opts.Now().UnixMilli()
	row := store.DegradedAlert{
		TenantID: m.TenantID, MonitorID: m.ID, Status: string(to), SinceMs: set.GetSinceMs(),
		Reachable: st.Reachable, Total: st.Total, Reporters: slices.Clone(set.GetReporters()),
	}

	if a.opts.SelfCheckFailed != nil && a.opts.SelfCheckFailed() {
		for _, ch := range chans {
			if !logged {
				break
			}
			r := row
			r.ChannelID, r.SentMs, r.Outcome = ch.ID, now, OutcomeSuppressed
			a.record(r)
		}
		return
	}

	p := cluster.AlertPayload{
		AlertID:  fmt.Sprintf("degraded:%s:%s:%d:%s", a.opts.NodeID, m.ID, set.GetSinceMs(), to),
		TenantID: m.TenantID, MonitorID: m.ID, MonitorName: m.Name, MonitorType: m.Type,
		Event: event, Status: string(to), PreviousStatus: string(from), SinceMs: set.GetSinceMs(),
		Confirmed: false, Sender: a.sender(st, set.GetReporters()),
	}
	if inc := set.GetIncident(); to == store.StatusDown && inc != nil {
		p.Cause, p.ErrorClass, p.IncidentID = inc.GetCause(), cluster.ErrorClassFromProto(inc.GetErrorClass()), inc.GetIncidentId()
	}

	for _, ch := range chans {
		r := row
		r.ChannelID = ch.ID
		if !flooded {
			if logged {
				a.deliver(ctx, ch, p, &r)
			} else {
				a.deliver(ctx, ch, p, nil)
			}
			continue
		}
		if logged {
			r.SentMs, r.Outcome = now, OutcomeSuppressed
			a.record(r)
		}
		if n, ok := a.claimSummary(m.TenantID, ch.ID); ok {
			a.sendSummary(ctx, ch, n, st)
		}
	}
}

// channels returns the channels of the monitor that are enabled and subscribe
// to the event (the same rule as the cluster's enqueueAlerts).
func (a *Applier) channels(ctx context.Context, m store.Monitor, event string) []store.NotificationChannel {
	var out []store.NotificationChannel
	for _, id := range m.ChannelIDs {
		ch, err := a.opts.State.GetChannel(ctx, m.TenantID, id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			continue
		case err != nil:
			a.opts.Logger.Warn("degraded: load notification channel", "tenant", m.TenantID, "channel", id, "error", err)
			continue
		}
		if ch.State == store.ChannelDisabled || !ch.Subscribes(event) {
			continue
		}
		out = append(out, ch)
	}
	return out
}

// deliver hands one alert to the local dispatcher. row, if set, is written
// with the final outcome; a summary has no row.
func (a *Applier) deliver(ctx context.Context, ch store.NotificationChannel, p cluster.AlertPayload, row *store.DegradedAlert) {
	target, err := alert.DecryptTarget(ctx, a.opts.Keyring, ch)
	if err != nil {
		a.opts.Logger.Warn("degraded: channel secrets unreadable", "tenant", ch.TenantID, "channel", ch.ID, "error", err)
		if row != nil {
			row.SentMs, row.Outcome = a.opts.Now().UnixMilli(), alert.LocalOutcomeFailed
			a.record(*row)
		}
		return
	}
	a.opts.Dispatcher.Enqueue(alert.LocalDelivery{
		TenantID: ch.TenantID, ChannelID: ch.ID, Target: target, Payload: p,
		Done: func(outcome string, err error) {
			if err != nil {
				a.opts.Logger.Warn("degraded: alert not delivered", "tenant", ch.TenantID, "channel", ch.ID,
					"monitor", p.MonitorID, "error", err)
			}
			if row != nil {
				r := *row
				r.SentMs, r.Outcome = a.opts.Now().UnixMilli(), outcome
				a.record(r)
			}
		},
	})
}

// sendSummary sends the flood summary of a channel: one message for n DOWN
// monitors instead of the single alerts (PF-28).
func (a *Applier) sendSummary(ctx context.Context, ch store.NotificationChannel, n int, st State) {
	now := a.opts.Now().UnixMilli()
	p := cluster.AlertPayload{
		AlertID:  fmt.Sprintf("degraded:%s:%s:flood:%d:%s", a.opts.NodeID, ch.TenantID, now, ch.ID),
		TenantID: ch.TenantID, MonitorName: fmt.Sprintf("%d monitors", n),
		Event: store.EventDown, Status: string(store.StatusDown), SinceMs: now,
		Cause:  fmt.Sprintf("%s sees %d monitors DOWN, probably its own network problem", a.opts.NodeID, n),
		Sender: a.sender(st, nil),
	}
	a.deliver(ctx, ch, p, nil)
}

// sender is the view of this node for an alert (FA-ALR-11).
func (a *Applier) sender(st State, reporters []string) *cluster.AlertSender {
	return &cluster.AlertSender{
		NodeID: a.opts.NodeID, ReachableVoters: st.Reachable, TotalVoters: st.Total,
		Reporters: slices.Clone(reporters), Confirmed: false,
	}
}

func (a *Applier) detector() State {
	if a.opts.Detector == nil {
		return State{}
	}
	return a.opts.Detector()
}

// record writes one row of the local log. A failed write is logged only: the
// alert itself has been handled already.
func (a *Applier) record(r store.DegradedAlert) {
	ctx, cancel := context.WithTimeout(context.Background(), rowTimeout)
	defer cancel()
	if _, err := a.opts.Metrics.InsertDegradedAlert(ctx, r); err != nil {
		a.opts.Logger.Warn("degraded: record alert", "tenant", r.TenantID, "monitor", r.MonitorID, "error", err)
	}
}

// flooded counts a DOWN change of a monitor and reports whether the tenant is
// in a flood: at least floodMinMonitors monitors changed to DOWN within the
// window and more than half of the tenant's monitors are among them (PF-28).
func (a *Applier) flooded(ctx context.Context, tenant, monitorID string) bool {
	now := a.opts.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	w := a.floods[tenant]
	if w == nil || now.Sub(w.start) >= a.opts.BundleWindow {
		w = &floodWindow{start: now, down: make(map[string]bool), summarized: make(map[string]bool)}
		a.floods[tenant] = w
	}
	w.down[monitorID] = true
	if len(w.down) < floodMinMonitors || w.flooded {
		return w.flooded
	}
	total, err := a.countMonitors(ctx, tenant)
	if err != nil {
		a.opts.Logger.Warn("degraded: count monitors for flood guard", "tenant", tenant, "error", err)
		return false
	}
	w.flooded = 2*len(w.down) > total
	return w.flooded
}

// claimSummary reports whether the channel still needs the summary of the
// current flood, and the number of DOWN monitors to put in it. It claims the
// summary, so each channel gets one per window.
func (a *Applier) claimSummary(tenant, channelID string) (int, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	w := a.floods[tenant]
	if w == nil || !w.flooded || w.summarized[channelID] {
		return 0, false
	}
	w.summarized[channelID] = true
	return len(w.down), true
}

// countMonitors returns the number of monitors of the tenant.
func (a *Applier) countMonitors(ctx context.Context, tenant string) (int, error) {
	n := 0
	q := store.MonitorQuery{Limit: 500}
	for {
		page, err := a.opts.State.ListMonitors(ctx, tenant, q)
		if err != nil {
			return 0, err
		}
		n += len(page.Items)
		if page.NextAfterID == "" {
			return n, nil
		}
		q.AfterID = page.NextAfterID
	}
}
