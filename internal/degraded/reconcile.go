// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package degraded

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/joinsider/pulsequorum/internal/aggregator"
	"github.com/joinsider/pulsequorum/internal/alert"
	"github.com/joinsider/pulsequorum/internal/cluster"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/replication"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/sysalert"
)

// Reconciliation after the quorum returned (ADR 0024, sections 4, 6 and 7):
// the Reporter sends the node-local log to the leader, the Judge decides the
// unconfirmed alerts, and the GapJob re-evaluates the history gap that a leader
// without quorum left behind (FA-ALR-10, FA-UPT-07).
const (
	reportInterval = 10 * time.Second
	reportBatch    = 100
	// logRetention is how long the node-local log keeps its rows (ADR 0024, section 4).
	logRetention  = 30 * 24 * time.Hour
	pruneInterval = 24 * time.Hour

	judgeInterval = 30 * time.Second
	judgeLimit    = 200
	// judgeSettle keeps a window open until it is this old, so that the
	// heartbeats of its last slots have been replicated and evaluated.
	judgeSettle = 30 * time.Second

	gapTick        = time.Second
	gapCatchUpWait = 2 * time.Minute
	// gapMinAge is the age of the last applied command from which on a
	// leadership change suspects a gap. Shorter gaps are covered by the live
	// rebuild, which looks back at least three slots after the leader grace.
	gapMinAge = 5 * time.Second
	// gapMaxLength bounds the re-evaluated gap (ADR 0024, section 7).
	gapMaxLength = 24 * time.Hour
	gapPageSize  = 500

	logReadCommandLevel = "degraded: read command level"
)

// Proposer proposes Raft commands. *cluster.Node satisfies it.
type Proposer interface {
	Apply(ctx context.Context, cmd *pb.Command) (cluster.ApplyResult, error)
}

// ReporterOptions configures a Reporter. NodeID, Detector, LeaderID, CaughtUp,
// CommandLevel, Proposer and Metrics are required.
type ReporterOptions struct {
	NodeID   string
	Detector func() State
	LeaderID func() string
	CaughtUp func() bool
	// CommandLevel returns the effective command level (cluster.Node.CommandLevel).
	CommandLevel func(ctx context.Context) (uint32, error)
	Proposer     Proposer
	Metrics      store.Metrics
	// Interval is the period of the report, default 10 s.
	Interval time.Duration
	// Now is the clock (default time.Now).
	Now    func() time.Time
	Logger *slog.Logger
}

// Reporter sends the unreported alerts of the node-local log to the leader
// once this node is no longer degraded (FA-ALR-10, ADR 0024, section 6).
type Reporter struct {
	opts      ReporterOptions
	lastPrune time.Time
}

// NewReporter returns a Reporter with defaults applied to opts.
func NewReporter(opts ReporterOptions) *Reporter {
	if opts.Interval <= 0 {
		opts.Interval = reportInterval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Reporter{opts: opts}
}

// Run reports every Interval until ctx ends.
func (r *Reporter) Run(ctx context.Context) {
	ticker := time.NewTicker(r.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

// tick prunes the log once a day and reports one round of alerts when the
// node is not degraded, a leader is known, the node is caught up and the
// cluster supports the degraded commands.
func (r *Reporter) tick(ctx context.Context) {
	now := r.opts.Now()
	if r.lastPrune.IsZero() || now.Sub(r.lastPrune) >= pruneInterval {
		r.lastPrune = now
		deleted, err := r.opts.Metrics.PruneDegradedAlerts(ctx, now.Add(-logRetention).UnixMilli())
		if err != nil {
			r.opts.Logger.Warn("degraded: prune alert log", "err", err)
		} else if deleted > 0 {
			r.opts.Logger.Info("degraded: pruned alert log", "deleted", deleted)
		}
	}
	if !r.ready(ctx) {
		return
	}
	for {
		batch, err := r.opts.Metrics.ListUnreportedDegradedAlerts(ctx, reportBatch)
		if err != nil {
			r.opts.Logger.Warn("degraded: read alert log", "err", err)
			return
		}
		if len(batch) == 0 {
			return
		}
		if _, err := r.opts.Proposer.Apply(ctx, cluster.NewReportDegradedAlerts(r.opts.NodeID, batch)); err != nil {
			// The block stays unreported; the leader stores it idempotently.
			r.opts.Logger.Warn("degraded: report alerts to the leader", "count", len(batch), "err", err)
			return
		}
		ids := make([]int64, 0, len(batch))
		for _, a := range batch {
			ids = append(ids, a.ID)
		}
		if err := r.opts.Metrics.MarkDegradedAlertsReported(ctx, ids); err != nil {
			r.opts.Logger.Warn("degraded: mark alerts reported", "count", len(ids), "err", err)
			return
		}
		r.opts.Logger.Info("degraded: reported alerts to the leader", "count", len(batch))
		if len(batch) < reportBatch {
			return
		}
	}
}

// ready reports whether the unreported alerts may be sent now.
func (r *Reporter) ready(ctx context.Context) bool {
	if r.opts.Detector().Degraded || r.opts.LeaderID() == "" || !r.opts.CaughtUp() {
		return false
	}
	level, err := r.opts.CommandLevel(ctx)
	if err != nil {
		r.opts.Logger.Warn(logReadCommandLevel, "err", err)
		return false
	}
	return level >= cluster.DegradedCommandLevel
}

// JudgeOptions configures a Judge. State, Proposer, IsLeader and CommandLevel
// are required. GapDone nil means the gap is never pending. RetentionRaw 0
// means the raw data is kept forever.
type JudgeOptions struct {
	State        store.State
	Proposer     Proposer
	IsLeader     func() bool
	CommandLevel func(ctx context.Context) (uint32, error)
	// GapDone reports whether the GapJob has finished for this leadership.
	GapDone      func() bool
	RetentionRaw time.Duration
	// Interval is the period of the judgement, default 30 s.
	Interval time.Duration
	// Unit is the length of one interval second, as aggregator.Options.Unit;
	// tests shrink it. Zero means 1 s.
	Unit time.Duration
	// Now is the clock (default time.Now).
	Now    func() time.Time
	Logger *slog.Logger
}

// episodeKey groups the alerts of one node, channel and monitor (ADR 0024, section 6).
type episodeKey struct {
	nodeID, channelID, tenantID, monitorID string
}

// decision is the judgement of one alert before its notification is set.
type decision struct {
	verdict string
	// matchSinceMs is the start of the matching change of a confirmed alert.
	matchSinceMs int64
}

// Judge decides the unconfirmed alerts once their window has confirmed history
// (FA-ALR-10, ADR 0024, section 6). It runs on the leader only.
type Judge struct {
	opts JudgeOptions
	// falseDown holds, per episode, whether the last DOWN judged by this leader
	// was a false alarm. An UP after it is a false alarm too. It is lost on a
	// leader change, so an UP whose DOWN was judged by an earlier leader is
	// judged by its own history.
	falseDown map[episodeKey]bool
}

// NewJudge returns a Judge with defaults applied to opts.
func NewJudge(opts JudgeOptions) *Judge {
	if opts.Interval <= 0 {
		opts.Interval = judgeInterval
	}
	if opts.Unit <= 0 {
		opts.Unit = time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Judge{opts: opts, falseDown: map[episodeKey]bool{}}
}

// Run judges every Interval until ctx ends.
func (j *Judge) Run(ctx context.Context) {
	ticker := time.NewTicker(j.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			j.tick(ctx)
		}
	}
}

func (j *Judge) tick(ctx context.Context) {
	if !j.opts.IsLeader() {
		clear(j.falseDown)
		return
	}
	if !j.ready(ctx) {
		return
	}
	alerts, err := j.opts.State.ListOpenUnconfirmedAlerts(ctx, judgeLimit)
	if err != nil {
		j.opts.Logger.Warn("degraded: list unconfirmed alerts", "err", err)
		return
	}
	if len(alerts) == 0 {
		return
	}
	verdicts, episodes := j.judge(ctx, j.opts.Now(), alerts)
	if len(verdicts) == 0 {
		return
	}
	if _, err := j.opts.Proposer.Apply(ctx, cluster.NewResolveUnconfirmedAlerts(verdicts)); err != nil {
		j.opts.Logger.Warn("degraded: resolve unconfirmed alerts", "count", len(verdicts), "err", err)
		return
	}
	j.falseDown = episodes
	j.opts.Logger.Info("degraded: judged unconfirmed alerts", "count", len(verdicts))
}

// ready reports whether judging may run: the gap job has finished and the
// cluster supports the degraded commands.
func (j *Judge) ready(ctx context.Context) bool {
	if j.opts.GapDone != nil && !j.opts.GapDone() {
		return false
	}
	level, err := j.opts.CommandLevel(ctx)
	if err != nil {
		j.opts.Logger.Warn(logReadCommandLevel, "err", err)
		return false
	}
	return level >= cluster.DegradedCommandLevel
}

// judge returns the verdicts of the alerts that can be decided now, in order,
// and the episode memory after them. An alert that cannot be decided yet blocks
// the later alerts of its episode, so that the order of an episode holds.
func (j *Judge) judge(ctx context.Context, now time.Time, alerts []store.UnconfirmedAlert) ([]cluster.AlertVerdict, map[episodeKey]bool) {
	alerts = slices.Clone(alerts)
	slices.SortStableFunc(alerts, func(a, b store.UnconfirmedAlert) int {
		switch {
		case a.SinceMs != b.SinceMs:
			return compareInt64(a.SinceMs, b.SinceMs)
		case a.NodeID != b.NodeID:
			return compareString(a.NodeID, b.NodeID)
		default:
			return compareInt64(a.LocalID, b.LocalID)
		}
	})

	episodes := maps.Clone(j.falseDown)
	if episodes == nil {
		episodes = map[episodeKey]bool{}
	}
	blocked := map[episodeKey]bool{}
	var out []cluster.AlertVerdict
	for _, a := range alerts {
		key := episodeKey{a.NodeID, a.ChannelID, a.TenantID, a.MonitorID}
		if blocked[key] {
			continue
		}
		d, ok, err := j.decide(ctx, now, a)
		if err != nil {
			j.opts.Logger.Warn("degraded: judge alert", "node", a.NodeID, "local_id", a.LocalID, "err", err)
		}
		if !ok {
			blocked[key] = true
			continue
		}
		if d.verdict == store.VerdictConfirmed && a.Status == string(store.StatusUp) && episodes[key] {
			d.verdict = store.VerdictFalseAlarm
		}
		notify, err := j.notify(ctx, a, d)
		if err != nil {
			// The outbox could not be read: decide this alert in a later round.
			j.opts.Logger.Warn("degraded: read outbox for alert", "node", a.NodeID, "local_id", a.LocalID, "err", err)
			blocked[key] = true
			continue
		}
		switch {
		case a.Status == string(store.StatusUp):
			delete(episodes, key)
		case d.verdict == store.VerdictFalseAlarm:
			episodes[key] = true
		default:
			delete(episodes, key)
		}
		out = append(out, cluster.AlertVerdict{NodeID: a.NodeID, LocalID: a.LocalID, Verdict: d.verdict, Notify: notify})
	}
	return out, episodes
}

// decide judges one alert against the confirmed history of its window. ok is
// false while the window is not yet confirmed.
func (j *Judge) decide(ctx context.Context, now time.Time, a store.UnconfirmedAlert) (decision, bool, error) {
	m, err := j.opts.State.GetMonitor(ctx, a.TenantID, a.MonitorID)
	if errors.Is(err, store.ErrNotFound) {
		return decision{verdict: store.VerdictUnverifiable}, true, nil
	}
	if err != nil {
		return decision{}, false, fmt.Errorf("read monitor: %w", err)
	}
	intervalMs := int64(m.IntervalSeconds) * j.opts.Unit.Milliseconds()
	if intervalMs <= 0 {
		return decision{verdict: store.VerdictUnverifiable}, true, nil
	}
	if j.opts.RetentionRaw > 0 && a.SinceMs < now.Add(-j.opts.RetentionRaw).UnixMilli() {
		return decision{verdict: store.VerdictUnverifiable}, true, nil
	}

	fromMs := a.SinceMs - 2*intervalMs
	toMs := a.SinceMs + int64(m.Retries+2)*intervalMs
	if toMs > now.Add(-judgeSettle).UnixMilli() {
		return decision{}, false, nil
	}
	changes, err := j.opts.State.StatusHistory(ctx, a.TenantID, a.MonitorID, fromMs, toMs)
	if err != nil {
		return decision{}, false, fmt.Errorf("read status history: %w", err)
	}

	// The change of the alert's status that is nearest to its start confirms it.
	var match *store.StatusChange
	for i := range changes {
		c := &changes[i]
		if c.Status != store.MonitorStatus(a.Status) {
			continue
		}
		if match == nil || absInt64(c.SinceMs-a.SinceMs) < absInt64(match.SinceMs-a.SinceMs) {
			match = c
		}
	}
	if match == nil {
		return decision{verdict: store.VerdictFalseAlarm}, true, nil
	}
	return decision{verdict: store.VerdictConfirmed, matchSinceMs: match.SinceMs}, true, nil
}

// notify reports whether the verdict is sent to the channel of the alert (ADR
// 0024, section 6). A false alarm is announced if the alert was sent. A
// confirmation only if the alert was sent and the channel did not receive the
// confirmed change already.
func (j *Judge) notify(ctx context.Context, a store.UnconfirmedAlert, d decision) (bool, error) {
	switch d.verdict {
	case store.VerdictFalseAlarm:
		return a.Outcome == alert.LocalOutcomeSent, nil
	case store.VerdictConfirmed:
		if a.Outcome != alert.LocalOutcomeSent {
			return false, nil
		}
		_, err := j.opts.State.GetAlert(ctx, alertKey(a.MonitorID, d.matchSinceMs, store.MonitorStatus(a.Status)), a.ChannelID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return true, nil
		case err != nil:
			return false, err
		default:
			return false, nil
		}
	default:
		return false, nil
	}
}

// alertKey is the outbox key of a status change, as cluster uses it.
func alertKey(monitorID string, sinceMs int64, status store.MonitorStatus) string {
	return fmt.Sprintf("%s:%d:%s", monitorID, sinceMs, status)
}

// GapReevaluator replays the slots of a monitor in a time range (see
// aggregator.Aggregator.ReevaluateGap), and hands the live aggregator the end
// of the re-evaluated window.
type GapReevaluator interface {
	ReevaluateGap(ctx context.Context, m store.Monitor, from store.MonitorStatus, fromMs, toMs int64) ([]aggregator.Change, error)
	// HandOver tells the live aggregator where the gap job's window ends
	// (ADR 0033). The live rebuild of the monitor replays from toMs on.
	HandOver(tenantID, monitorID string, toMs int64)
	// ClearHandOvers drops the handover points of an earlier gap job run.
	ClearHandOvers()
}

// GapOptions configures a GapJob. State, Reevaluator, Proposer, IsLeader and
// CommandLevel are required. Replication nil means there are no peers to wait
// for. The zero values of the others select the defaults.
type GapOptions struct {
	NodeID       string
	State        store.State
	Reevaluator  GapReevaluator
	Proposer     Proposer
	IsLeader     func() bool
	CommandLevel func(ctx context.Context) (uint32, error)
	// Replication returns the state of the replication to the peers.
	Replication func() []replication.PeerStatus
	// MinAge is the age of the last command from which a gap is suspected.
	// Zero means gapMinAge.
	MinAge time.Duration
	// Unit is the length of one interval second, as aggregator.Options.Unit;
	// tests shrink it. Zero means 1 s.
	Unit time.Duration
	// StartLastSeen is the command time of the last applied command, read
	// before Raft started (store.State.LastIssuedAt, ADR 0033 option 2A). Run
	// uses it instead of its first sample when it is not zero, so a node that
	// becomes leader before Run starts still sees the time before the quorum
	// returned. Zero means Run samples at its start.
	StartLastSeen int64
	// Trigger requests an anti-entropy round with a peer (see
	// replication.Replicator.Trigger). It is called once for every reachable
	// peer when a gap is suspected. Nil means none.
	Trigger func(nodeID string)
	// Peers returns what this node knows about the peer voters (see
	// clusterhost.Host.PeerViews). Nil means there are no peers to wait for.
	// A peer counts as reachable if it answered within DefaultReachableWithin.
	Peers func() map[string]PeerView
	// Degraded reports whether this node is in the degraded mode (ADR 0024).
	// A finished gap is announced as restored (ADR 0025, section 5) only if the
	// node was degraded while it had no leader: then a quorum-lost alert went
	// out. A quiet cluster has old commands without any outage. Nil means never.
	Degraded func() bool
	// Tick is the sampling and polling period, default 1 s.
	Tick time.Duration
	// CatchUpWait bounds the wait for the anti-entropy round of every connected
	// peer after the leadership start, default 2 min.
	CatchUpWait time.Duration
	// Now is the clock (default time.Now).
	Now    func() time.Time
	Logger *slog.Logger
}

// GapJob corrects the history after a leader without quorum (FA-UPT-07, ADR
// 0024, section 7). While the node is no leader it samples the last issued
// command time. When it becomes leader, it re-evaluates the gap between that
// time and the live window of the aggregator, and then records the current
// status with the usual change command.
type GapJob struct {
	opts GapOptions

	// The fields below are used by the Run goroutine only.
	leader      bool
	leaderSince time.Time
	lastSeen    int64
	// degradedSeen is set while the node has no leader and is degraded;
	// afterDegraded is the value it had when this leadership began.
	degradedSeen  bool
	afterDegraded bool

	mu   sync.Mutex
	done bool
}

// NewGapJob returns a GapJob with defaults applied to opts.
func NewGapJob(opts GapOptions) *GapJob {
	if opts.Tick <= 0 {
		opts.Tick = gapTick
	}
	if opts.CatchUpWait <= 0 {
		opts.CatchUpWait = gapCatchUpWait
	}
	if opts.MinAge <= 0 {
		opts.MinAge = gapMinAge
	}
	if opts.Unit <= 0 {
		opts.Unit = time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &GapJob{opts: opts}
}

// Done reports whether the job has finished for the current leadership. It is
// also true at once when there is no gap or the cluster is below command level
// 5. It is false while the node is no leader, and it is safe for concurrent use.
func (g *GapJob) Done() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.done
}

func (g *GapJob) setDone(v bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.done = v
}

// start records the state the first tick works from: the command time of
// StartLastSeen if it is set, otherwise the one the store holds now.
func (g *GapJob) start(ctx context.Context) {
	if g.opts.StartLastSeen == 0 {
		g.sample(ctx)
		return
	}
	g.recordDegraded()
	g.lastSeen = g.opts.StartLastSeen
}

// Run ticks until ctx ends.
func (g *GapJob) Run(ctx context.Context) {
	g.start(ctx)
	ticker := time.NewTicker(g.opts.Tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.tick(ctx)
		}
	}
}

// tick samples while the node is no leader, and runs the job once per leadership.
func (g *GapJob) tick(ctx context.Context) {
	if !g.opts.IsLeader() {
		g.leader = false
		g.setDone(false)
		g.sample(ctx)
		return
	}
	if !g.leader {
		g.leader = true
		g.leaderSince = g.opts.Now()
		g.setDone(false)
		g.afterDegraded, g.degradedSeen = g.degradedSeen, false
		// Points of an earlier, aborted run must not survive (ADR 0033).
		g.opts.Reevaluator.ClearHandOvers()
	}
	if g.Done() {
		return
	}
	g.run(ctx)
}

// sample stores the command time of the last applied command, and whether
// the node is degraded.
func (g *GapJob) sample(ctx context.Context) {
	g.recordDegraded()
	v, err := g.opts.State.LastIssuedAt(ctx)
	if err != nil {
		g.opts.Logger.Warn("degraded: read last command time", "err", err)
		return
	}
	g.lastSeen = v
}

// recordDegraded remembers that the node was degraded while it had no leader.
func (g *GapJob) recordDegraded() {
	if g.opts.Degraded != nil && g.opts.Degraded() {
		g.degradedSeen = true
	}
}

// run performs the job of the current leadership. It returns with Done still
// false if the context ends, leadership is lost or an error occurs; the next
// tick then retries.
func (g *GapJob) run(ctx context.Context) {
	level, err := g.opts.CommandLevel(ctx)
	if err != nil {
		g.opts.Logger.Warn(logReadCommandLevel, "err", err)
		return
	}
	if level < cluster.DegradedCommandLevel || !g.suspectsGap() {
		g.setDone(true)
		return
	}
	g.triggerReachablePeers()
	if !g.awaitCatchUp(ctx) {
		return
	}
	tenants, err := g.opts.State.ListTenants(ctx)
	if err != nil {
		g.opts.Logger.Warn("degraded: list tenants for gap job", "err", err)
		return
	}
	for _, t := range tenants {
		if !g.reevaluateTenant(ctx, t.ID) {
			return
		}
	}
	if ctx.Err() != nil || !g.opts.IsLeader() {
		return
	}
	if g.afterDegraded && level >= cluster.SystemAlertCommandLevel {
		g.announceRestored(ctx)
	}
	g.setDone(true)
}

// triggerReachablePeers asks every reachable peer for a fresh round now,
// instead of waiting for the periodic anti-entropy pass.
func (g *GapJob) triggerReachablePeers() {
	if g.opts.Trigger == nil {
		return
	}
	for _, id := range g.reachablePeers() {
		g.opts.Trigger(id)
	}
}

func (g *GapJob) reevaluateTenant(ctx context.Context, tenantID string) bool {
	afterID := ""
	for {
		page, err := g.opts.State.ListMonitors(ctx, tenantID, store.MonitorQuery{AfterID: afterID, Limit: gapPageSize})
		if err != nil {
			g.opts.Logger.Warn("degraded: list monitors for gap job", "tenant", tenantID, "err", err)
			return false
		}
		for _, m := range page.Items {
			if !g.reevaluateMonitor(ctx, m) {
				return false
			}
		}
		if page.NextAfterID == "" {
			return true
		}
		afterID = page.NextAfterID
	}
}

func (g *GapJob) reevaluateMonitor(ctx context.Context, m store.Monitor) bool {
	if ctx.Err() != nil || !g.opts.IsLeader() {
		return false
	}
	err := g.reevaluate(ctx, m)
	switch {
	case err == nil:
		return true
	case errors.Is(err, store.ErrConflict):
		// The status moved on meanwhile; the live aggregator covers it.
		g.opts.Logger.Info("degraded: gap changed meanwhile, skipped", "tenant", m.TenantID, "monitor", m.ID, "err", err)
		return true
	default:
		// Any other failure leaves the job open: the next tick retries it.
		g.opts.Logger.Warn("degraded: re-evaluate gap, retrying", "tenant", m.TenantID, "monitor", m.ID, "err", err)
		return false
	}
}

// announceRestored closes the quorum alert of the outage (ADR 0025, section 5).
// The restored alert is the clear of the system alert "quorum": it changes the
// state only if the alert is active, so the leader first proposes it active and
// then inactive. The active step also notifies, as the confirmed record of the
// outage. A failure is logged; the gap itself is already corrected.
func (g *GapJob) announceRestored(ctx context.Context) {
	gapSeconds := int64(g.opts.Now().Sub(time.UnixMilli(g.lastSeen)) / time.Second)
	details := json.RawMessage(fmt.Sprintf(`{"gapSeconds":%d}`, gapSeconds))
	for _, active := range []bool{true, false} {
		cmd := cluster.NewSetSystemAlert(sysalert.KindQuorum, sysalert.SubjectCluster, active, 0, details)
		if _, err := g.opts.Proposer.Apply(ctx, cmd); err != nil {
			g.opts.Logger.Warn("degraded: record quorum restored", "active", active, "err", err)
			return
		}
	}
	g.opts.Logger.Info("quorum restored", "gap_seconds", gapSeconds)
}

// suspectsGap reports whether the last applied command is old enough to hide a
// gap. Without a known command time, or with a recent one, there is none.
func (g *GapJob) suspectsGap() bool {
	if g.lastSeen == 0 {
		return false
	}
	return g.opts.Now().Sub(time.UnixMilli(g.lastSeen)) >= g.opts.MinAge
}

// awaitCatchUp waits until every reachable peer completed an anti-entropy round
// after the leadership start, at most CatchUpWait. The push stream does not
// count: a restarted peer is reachable before its stream is connected. It
// returns false if the context ends or leadership is lost.
func (g *GapJob) awaitCatchUp(ctx context.Context) bool {
	if g.opts.Peers == nil {
		return true
	}
	deadline := time.NewTimer(g.opts.CatchUpWait)
	defer deadline.Stop()
	ticker := time.NewTicker(g.opts.Tick)
	defer ticker.Stop()
	for {
		if g.caughtUp() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			g.opts.Logger.Warn("degraded: reachable peers not caught up in time, re-evaluating the gap anyway", "wait", g.opts.CatchUpWait)
			return true
		case <-ticker.C:
			if !g.opts.IsLeader() {
				return false
			}
		}
	}
}

// caughtUp reports whether every reachable peer completed an anti-entropy round
// that started after the leadership start. Peers without a replication status
// are waited for as well.
func (g *GapJob) caughtUp() bool {
	status := map[string]replication.PeerStatus{}
	if g.opts.Replication != nil {
		for _, p := range g.opts.Replication() {
			status[p.ID] = p
		}
	}
	for _, id := range g.reachablePeers() {
		if !status[id].CaughtUpAt.After(g.leaderSince) {
			return false
		}
	}
	return true
}

// reachablePeers returns the IDs of the peers that answered within
// DefaultReachableWithin, sorted.
func (g *GapJob) reachablePeers() []string {
	if g.opts.Peers == nil {
		return nil
	}
	now := g.opts.Now()
	var ids []string
	for id, v := range g.opts.Peers() {
		if v.AnsweredAt.IsZero() || now.Sub(v.AnsweredAt) > DefaultReachableWithin {
			continue
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// reevaluate corrects the history of one monitor. It writes the gap before the
// last change with RecordMonitorHistory and the last change with
// SetMonitorState, so that a still running outage is alerted with quorum.
func (g *GapJob) reevaluate(ctx context.Context, m store.Monitor) error {
	cur := m.State
	if m.Paused || cur.Status == store.StatusPaused || cur.Status == store.StatusMaintenance {
		return nil
	}
	if m.IntervalSeconds <= 0 {
		return nil
	}
	interval := time.Duration(m.IntervalSeconds) * g.opts.Unit
	// The live aggregator rebuilds from this many slots back (aggregator/watch.go).
	lookback := int64(max(m.Retries+1, m.RecoveryThreshold) + 2)
	liveStartMs := g.opts.Now().Add(-time.Duration(lookback+1) * interval).UnixMilli()
	// The live rebuild replays from the end of this window on (ADR 0033).
	g.opts.Reevaluator.HandOver(m.TenantID, m.ID, liveStartMs)

	gapStartMs := max(g.lastSeen, cur.SinceMs, liveStartMs-gapMaxLength.Milliseconds())
	if liveStartMs-gapStartMs < interval.Milliseconds() {
		return nil
	}
	changes, err := g.opts.Reevaluator.ReevaluateGap(ctx, m, cur.Status, gapStartMs, liveStartMs)
	if err != nil {
		return fmt.Errorf("re-evaluate gap: %w", err)
	}
	if len(changes) == 0 {
		return nil
	}
	last := changes[len(changes)-1]
	from := cur.Status
	if len(changes) > 1 {
		history := changes[:len(changes)-1]
		entries := make([]cluster.HistoryChange, 0, len(history))
		for _, c := range history {
			entries = append(entries, historyChange(c))
		}
		from = history[len(history)-1].Status
		cmd := cluster.NewRecordMonitorHistory(m.TenantID, m.ID, cur.Status, gapStartMs, last.SinceMs, entries)
		if _, err := g.opts.Proposer.Apply(ctx, cmd); err != nil {
			return fmt.Errorf("record monitor history: %w", err)
		}
	}
	_, err = g.opts.Proposer.Apply(ctx, cluster.NewSetMonitorState(cluster.StateChange{
		TenantID:     m.TenantID,
		MonitorID:    m.ID,
		From:         from,
		To:           last.Status,
		SinceMs:      last.SinceMs,
		Incident:     last.Incident,
		Reporters:    last.Reporters,
		SenderNodeID: g.opts.NodeID,
	}))
	if err != nil {
		return fmt.Errorf("set monitor state: %w", err)
	}
	return nil
}

// historyChange maps a re-evaluated change to a history entry.
func historyChange(c aggregator.Change) cluster.HistoryChange {
	h := cluster.HistoryChange{SinceMs: c.SinceMs, Status: c.Status}
	if c.Incident != nil {
		h.IncidentID = c.Incident.ID
		h.Cause = c.Incident.Cause
		h.ErrorClass = c.Incident.ErrorClass
	}
	return h
}

func compareInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func compareString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
