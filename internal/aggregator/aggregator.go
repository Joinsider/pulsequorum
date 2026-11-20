// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package aggregator derives the consensus status of the monitors from the
// heartbeats of all responsible nodes and records status changes and incidents
// via Raft (FA-CON-01/02/03/05/06, FA-INC-01, ADR 0021).
//
// Every voter runs an Aggregator, but only the leader evaluates: it evaluates
// each check round (slot) of a monitor exactly once, when all responsible nodes
// reported or at the deadline. A new leader waits for a grace period, then
// rebuilds the streaks from the stored heartbeats.
package aggregator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/pulsequorum/internal/cluster"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/scheduler"
	"github.com/joinsider/pulsequorum/internal/store"
)

const (
	observeQueueSize   = 1024
	applyTimeout       = 5 * time.Second
	tickInterval       = time.Second
	registryTTL        = time.Second
	defaultLeaderGrace = 10 * time.Second // as the health grace (ADR 0017)
	defaultReportDelay = 5 * time.Second  // FA-CON-05 deadline
)

// Applier proposes Raft commands.
type Applier interface {
	Apply(ctx context.Context, cmd *pb.Command) (cluster.ApplyResult, error)
}

// Options configure an Aggregator.
type Options struct {
	State   store.State
	Metrics store.Metrics
	Cluster Applier
	// NodeID is this node. It is the only responsible node while the registry
	// is empty (single voter before registration).
	NodeID string
	// IsLeader reports whether this node leads. Only the leader proposes
	// status changes. Nil means never.
	IsLeader func() bool
	// Registry returns the node registry (ADR 0017). Nil means empty.
	Registry func(ctx context.Context) ([]store.Node, error)
	// Assigner decides the responsible nodes of a monitor (ADR 0017).
	Assigner scheduler.Assigner
	// Exclude removes a node from the responsible nodes, e.g. a node that
	// reports itself unhealthy (FA-CON-04, AP-3.2). Nil means none.
	Exclude func(nodeID string) bool
	// Maintenance reports whether m is in a maintenance window at the start of
	// a round. Such rounds are not evaluated (FA-MNT-02). Nil means none.
	Maintenance func(m store.Monitor, at time.Time) bool
	// Now is the clock for all decisions (ADR 0020). Nil means time.Now.
	Now func() time.Time
	// LeaderGrace is how long a new leader waits before it evaluates, so that
	// anti-entropy can deliver the heartbeats of the previous leader.
	// Zero means 10 s.
	LeaderGrace time.Duration
	// ReportDelay is added to the timeout for the deadline of a round. Zero
	// means 5 s (FA-CON-05).
	ReportDelay time.Duration
	// Unit is the length of one interval second; tests shrink it. Zero means
	// one second, the same as the scheduler.
	Unit   time.Duration
	Logger *slog.Logger
}

// monitorKey identifies a monitor. Monitor IDs are only unique per tenant.
type monitorKey struct {
	tenantID, monitorID string
}

// Aggregator evaluates the rounds of all monitors while it is the leader.
type Aggregator struct {
	opts     Options
	observed chan []store.Heartbeat

	mu          sync.Mutex
	watches     map[monitorKey]*watch
	leaderSince time.Time // zero while not leader
	registry    []store.Node
	registryAt  time.Time
	// deviations and deviationSeen hold the slots counted for Deviation (ADR 0022).
	deviations    []deviationEvent
	deviationSeen map[deviationKey]int64
	// handover holds, per monitor, the end of the window the gap job
	// re-evaluated (ADR 0033). The next rebuild of that monitor replays from
	// there. The gap job clears them when its leadership starts.
	handover map[monitorKey]int64
}

// errSuperseded means the status changed meanwhile (conflict, deletion, or no
// longer leader); the monitor is reloaded before its next round.
var errSuperseded = errors.New("status change superseded")

// New creates an Aggregator; call Run to start it.
func New(opts Options) *Aggregator {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.LeaderGrace <= 0 {
		opts.LeaderGrace = defaultLeaderGrace
	}
	if opts.ReportDelay <= 0 {
		opts.ReportDelay = defaultReportDelay
	}
	if opts.Unit <= 0 {
		opts.Unit = time.Second
	}
	return &Aggregator{opts: opts, observed: make(chan []store.Heartbeat, observeQueueSize),
		watches: map[monitorKey]*watch{}, deviationSeen: map[deviationKey]int64{},
		handover: map[monitorKey]int64{}}
}

// HandOver records toMs as the end of the window the gap job re-evaluated
// for a monitor (ADR 0033). The next rebuild of that monitor replays the
// slots from toMs on, even if they lie before the usual lookback, so that no
// slot between the gap job and the live evaluation stays unevaluated. It is
// used once. A loss of leadership keeps it, because the gap job of the next
// leadership may record it before this aggregator noticed the loss; the gap
// job drops stale points with ClearHandOvers when its leadership starts.
func (a *Aggregator) HandOver(tenantID, monitorID string, toMs int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.handover[monitorKey{tenantID, monitorID}] = toMs
}

// ClearHandOvers drops all recorded handover points (a new gap job run starts).
func (a *Aggregator) ClearHandOvers() {
	a.mu.Lock()
	defer a.mu.Unlock()
	clear(a.handover)
}

// Observe passes heartbeats that were stored (local, replicated or by
// anti-entropy). It never blocks: a batch that does not fit into the queue is
// dropped, and the round deadline covers it.
func (a *Aggregator) Observe(hbs []store.Heartbeat) {
	if len(hbs) == 0 {
		return
	}
	batch := slices.Clone(hbs)
	select {
	case a.observed <- batch:
	default:
		a.opts.Logger.Debug("heartbeats not queued for evaluation, the round deadline covers them", "count", len(batch))
	}
}

// Sync sets the monitors of all tenants. Monitors that no longer exist are
// forgotten; a changed revision or interval restarts the evaluation of the
// monitor from its stored state. The reconciler calls it.
func (a *Aggregator) Sync(monitors []store.Monitor) {
	a.mu.Lock()
	defer a.mu.Unlock()
	keep := make(map[monitorKey]bool, len(monitors))
	for _, m := range monitors {
		key := monitorKey{m.TenantID, m.ID}
		keep[key] = true
		w := a.watches[key]
		if w == nil {
			w = &watch{}
			a.watches[key] = w
		}
		w.configure(m, a.opts.Unit, a.opts.ReportDelay)
	}
	for key := range a.watches {
		if !keep[key] {
			delete(a.watches, key)
		}
	}
}

// Run evaluates until ctx ends. It handles observed heartbeats as they come,
// and checks the deadlines every tick.
func (a *Aggregator) Run(ctx context.Context) {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case batch := <-a.observed:
			a.absorb(batch)
			a.step(ctx)
		case <-ticker.C:
			a.step(ctx)
		}
	}
}

// absorb buffers heartbeats of monitors known to this aggregator. A heartbeat
// of an unknown tenant and monitor pair is ignored (tenant boundary). On a
// node that is not evaluating, nothing is buffered: the rebuild reads the store.
func (a *Aggregator) absorb(hbs []store.Heartbeat) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, hb := range hbs {
		w := a.watches[monitorKey{hb.TenantID, hb.MonitorID}]
		if w == nil || !w.ready {
			continue
		}
		w.add(hb)
	}
}

// step tracks leadership and evaluates all monitors if this node may propose.
func (a *Aggregator) step(ctx context.Context) {
	now := a.opts.Now()
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.opts.IsLeader == nil || !a.opts.IsLeader() {
		if !a.leaderSince.IsZero() {
			a.leaderSince = time.Time{}
			a.dropAll()
			// The handover points stay: the gap job of the next leadership
			// may already have recorded them, and it clears stale ones itself
			// when its leadership starts (ADR 0033).
			a.opts.Logger.Info("no longer leader, monitor evaluation stopped")
		}
		return
	}
	if a.leaderSince.IsZero() {
		a.leaderSince = now
		a.opts.Logger.Info("leader, monitor evaluation starts after the grace period", "grace", a.opts.LeaderGrace)
	}
	if now.Sub(a.leaderSince) < a.opts.LeaderGrace {
		return
	}

	registry, err := a.loadRegistry(ctx, now)
	if err != nil {
		a.opts.Logger.Error("reading the node registry failed, evaluation waits", "err", err)
		return
	}
	nowMs := now.UnixMilli()
	for _, w := range a.watches {
		a.process(ctx, w, nowMs, registry)
	}
}

// dropAll forgets the evaluation state of all monitors (leadership lost).
func (a *Aggregator) dropAll() {
	for _, w := range a.watches {
		w.drop()
	}
}

// loadRegistry returns the node registry, read at most once per registryTTL.
func (a *Aggregator) loadRegistry(ctx context.Context, now time.Time) ([]store.Node, error) {
	if a.opts.Registry == nil {
		return nil, nil
	}
	if !a.registryAt.IsZero() && !now.Before(a.registryAt) && now.Sub(a.registryAt) < registryTTL {
		return a.registry, nil
	}
	nodes, err := a.opts.Registry(ctx)
	if err != nil {
		return nil, err
	}
	a.registry, a.registryAt = nodes, now
	return nodes, nil
}

// responsible returns the nodes whose heartbeats count for m, sorted by ID
// (FA-CON-05, ADR 0021). Without a registry it is this node alone.
func (a *Aggregator) responsible(m store.Monitor, registry []store.Node) []string {
	ids := []string{a.opts.NodeID}
	if len(registry) > 0 {
		ids = a.opts.Assigner.Nodes(m, registry)
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if a.opts.Exclude != nil && a.opts.Exclude(id) {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// process evaluates the due slots of w in order. Slots that are not due yet
// stop the loop. The loop rebuilds the state when it is missing, so the first
// due slot after a leader change starts with the replay of the older slots.
func (a *Aggregator) process(ctx context.Context, w *watch, nowMs int64, registry []store.Node) {
	if !w.enabled() {
		return
	}
	responsible := a.responsible(w.m, registry)
	rebuilt := false
	for {
		// After a long stall, do not walk through every missed round: the
		// rounds before the lookback are no reports or are covered by the rebuild.
		// A rebuild in this call sets w.next itself, possibly before the floor
		// after a handover (ADR 0033), so the clamp skips it then.
		if floor := w.slotOf(nowMs) - w.lookback(); !rebuilt && w.ready && w.next < floor {
			w.next = floor
		}
		k := w.next
		if !w.due(k, nowMs, responsible) {
			return
		}

		// A maintenance round is not evaluated and resets the streaks (FA-MNT-02).
		if a.opts.Maintenance != nil && a.opts.Maintenance(w.m, time.UnixMilli(w.start(k))) {
			if w.ready {
				w.t.resetStreaks()
			}
			w.next = k + 1
			continue
		}

		if !w.ready {
			if err := a.rebuild(ctx, w, nowMs); err != nil {
				if ctx.Err() == nil {
					a.opts.Logger.Warn("rebuilding the monitor state failed, retrying", "tenant", w.m.TenantID, "monitor", w.m.ID, "err", err)
				}
				return
			}
			rebuilt = true
			continue
		}

		// The status is set elsewhere while the monitor is paused or in a
		// maintenance window; reload it before the next round (see rebuild).
		if w.t.status == store.StatusMaintenance || w.t.status == store.StatusPaused {
			w.drop()
			return
		}

		if err := a.evaluate(ctx, w, k, responsible); err != nil {
			w.drop()
			return
		}
		w.next = k + 1
		w.prune()
	}
}

// rebuild loads the status and the heartbeats of the open rounds of w: the
// streaks start at the current status (FA-CON-03, NFA-REL-03). Rounds from
// the status change on are then replayed by process with the same rules, so
// that a new leader reaches the decision of the previous one.
func (a *Aggregator) rebuild(ctx context.Context, w *watch, nowMs int64) error {
	m := w.m
	cur, err := a.opts.State.GetMonitor(ctx, m.TenantID, m.ID)
	if err != nil {
		return fmt.Errorf("load monitor state: %w", err)
	}
	from := max(cur.State.SinceMs, w.start(w.slotOf(nowMs)-w.lookback()))
	// A handover from the gap job reaches back to its window end (ADR 0033).
	// It is used once; rebuild runs under a.mu, which step holds.
	key := monitorKey{m.TenantID, m.ID}
	if h, ok := a.handover[key]; ok {
		from = max(cur.State.SinceMs, min(from, h))
		delete(a.handover, key)
	}
	first := w.slotOf(from)
	if w.start(first) < from {
		first++
	}
	hbs, err := a.loadHeartbeats(ctx, m.TenantID, m.ID, w.start(first)-2*w.intervalMs+1, 0)
	if err != nil {
		return fmt.Errorf("load heartbeats: %w", err)
	}
	w.hbs = make(map[hbKey]store.Heartbeat, len(hbs))
	for _, hb := range hbs {
		w.hbs[hbKey{hb.NodeID, hb.Incarnation, hb.Seq}] = hb
	}
	w.t = &tracker{status: cur.State.Status}
	w.next = first
	w.ready = true
	return nil
}

// loadHeartbeats reads the heartbeats of all nodes of a monitor from fromMs on,
// up to toMs if it is not zero (the store reads without bound for zero).
func (a *Aggregator) loadHeartbeats(ctx context.Context, tenantID, monitorID string, fromMs, toMs int64) ([]store.Heartbeat, error) {
	var out []store.Heartbeat
	q := store.HeartbeatQuery{MonitorID: monitorID, FromMs: fromMs, ToMs: toMs}
	for {
		page, err := a.opts.Metrics.ListHeartbeats(ctx, tenantID, q)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.Next == nil {
			return out, nil
		}
		q.After = page.Next
	}
}

// evaluate decides round k of w and proposes the status change it causes.
// It returns errSuperseded if the change was refused because the state moved
// on; the monitor is then reloaded.
func (a *Aggregator) evaluate(ctx context.Context, w *watch, k int64, responsible []string) error {
	reports := w.reporters(k, responsible)
	startMs := w.start(k)
	a.recordDeviation(w, k, w.allReporters(k), startMs)
	to, since, change := decide(w.t, w.m, startMs, reports)
	if !change {
		return nil
	}

	m := w.m
	c := cluster.StateChange{TenantID: m.TenantID, MonitorID: m.ID, From: w.t.status, To: to, SinceMs: since,
		Reporters: nodeIDs(reports), SenderNodeID: a.opts.NodeID}
	if to == store.StatusDown {
		c.Incident = incidentOf(uuid.NewString(), reports)
	}
	applyCtx, cancel := context.WithTimeout(ctx, applyTimeout)
	_, err := a.opts.Cluster.Apply(applyCtx, cluster.NewSetMonitorState(c))
	cancel()

	log := a.opts.Logger.With("tenant", m.TenantID, "monitor", m.ID, "from", c.From, "to", to, "round", k)
	switch {
	case err == nil:
		w.t.status = to
		log.Info("monitor status changed", "reports", len(reports))
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrNotFound), errors.Is(err, cluster.ErrNotApplied):
		log.Debug("status change superseded", "err", err)
		return errSuperseded
	case ctx.Err() != nil:
		return nil
	default:
		// The streak has counted this round; the next round proposes again.
		log.Warn("recording status change failed; the next round retries", "err", err)
	}
	return nil
}

// incidentOf opens the incident with the given ID for a change to DOWN
// (FA-INC-01). The affected nodes are those that reported DOWN in the
// triggering round; cause and error class come from the first of them by node ID.
func incidentOf(id string, reports []store.Heartbeat) *store.Incident {
	inc := &store.Incident{ID: id}
	for _, r := range reports {
		if r.Status != store.CheckDown {
			continue
		}
		if len(inc.AffectedNodes) == 0 {
			inc.Cause = r.Message
			inc.ErrorClass = r.ErrorClass
		}
		inc.AffectedNodes = append(inc.AffectedNodes, r.NodeID)
	}
	return inc
}
