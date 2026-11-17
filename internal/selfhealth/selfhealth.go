// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package selfhealth decides on the leader which voters are impaired in their
// connectivity (FA-CON-04, ADR 0022). A voter with a broken egress reports its
// reference targets as failed, or its heartbeats deviate from its peers. The
// leader excludes such a voter from the consensus and records the change
// through Raft. Only the leader decides; the decisions are kept in memory
// until they are recorded.
package selfhealth

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/joinsider/uptimecluster/internal/aggregator"
	"github.com/joinsider/uptimecluster/internal/cluster"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/selfcheck"
	"github.com/joinsider/uptimecluster/internal/store"
)

// Timings and thresholds of the decisions (ADR 0022).
const (
	// DefaultInterval is the assessment period, the same as the GetStatus poll.
	DefaultInterval = 15 * time.Second
	// DefaultRecovery is how long a node must show no signal before it counts
	// as connected again.
	DefaultRecovery = 2 * time.Minute

	// deviationWindow is the window of the deviation statistics (the aggregator keeps it).
	deviationWindow = 5 * time.Minute
	// minDeviationSlots is the number of compared slots needed for a deviation signal.
	minDeviationSlots = 10
	// referenceFreshness bounds the age of a self-check round that counts as a signal.
	referenceFreshness = 90 * time.Second
	// confirmRounds is the number of consecutive assessments with a signal before a node is impaired.
	confirmRounds = 2
	// applyTimeout bounds one proposal of a connectivity change.
	applyTimeout = 5 * time.Second
)

// Reasons of an impaired connectivity (cluster.ConnectivityReasonReference, ...).
const (
	reasonReference = cluster.ConnectivityReasonReference
	reasonDeviation = cluster.ConnectivityReasonDeviation
)

// Registry reads the node registry (ADR 0017).
type Registry interface {
	ListNodes(ctx context.Context) ([]store.Node, error)
}

// Applier proposes Raft commands.
type Applier interface {
	Apply(ctx context.Context, cmd *pb.Command) (cluster.ApplyResult, error)
}

// Options configure an Assessor.
type Options struct {
	// NodeID is this node. Its own self-check comes from Self.
	NodeID string
	// IsLeader reports whether this node leads. Only the leader assesses.
	IsLeader func() bool
	// Registry returns the node registry; required.
	Registry Registry
	// Cluster records the connectivity changes; required.
	Cluster Applier
	// Self returns the latest self-check of this node. Nil means not configured.
	Self func() selfcheck.Status
	// Peers returns the self-check answers of the peer voters, by node ID. Nil means none.
	Peers func() map[string]selfcheck.Remote
	// Deviation returns the deviation statistics of the slots from sinceMs on
	// (aggregator, ADR 0022). Nil means no deviation signal.
	Deviation func(sinceMs int64) map[string]aggregator.DeviationStats
	// FailRatio is SELF_HEALTH_FAIL_RATIO: the share of DOWN slots that counts as a deviation.
	FailRatio float64
	// Interval is the assessment period. Zero means DefaultInterval.
	Interval time.Duration
	// Recovery is the time without a signal before an impaired node is connected again.
	// Zero means DefaultRecovery.
	Recovery time.Duration
	// Now is the clock of this node (ADR 0020). Nil means time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// Assessor runs the connectivity decisions of the leader.
type Assessor struct {
	opts Options

	// mu guards the fields below. Exclude takes it, so the aggregator never
	// waits for I/O; the assessor never calls the aggregator while holding it.
	mu          sync.Mutex
	leader      bool
	leaderSince time.Time
	// registry is the node registry as last read.
	registry map[string]store.Node
	// states holds the streaks of the voters.
	states map[string]*nodeState
	// decisions are the connectivity decisions of this leader. A decision that
	// is not persisted yet overrides the registry in Exclude.
	decisions map[string]*decision
}

// nodeState is the assessment history of one voter.
type nodeState struct {
	// streak counts the consecutive assessments with a signal.
	streak int
	// lastSignal is the time of the last assessment with a signal. A node
	// without a signal is first seen at the start of the decisions.
	lastSignal time.Time
}

// decision is an impaired or connected verdict of this leader.
type decision struct {
	impaired  bool
	reason    string
	cmd       *pb.Command // created at the first proposal and kept for retries
	persisted bool
}

// signal is the assessment of one voter at one point in time.
type signal struct {
	on     bool
	reason string
}

// New creates an Assessor; call Run to start it.
func New(opts Options) *Assessor {
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Recovery <= 0 {
		opts.Recovery = DefaultRecovery
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Assessor{
		opts:      opts,
		registry:  map[string]store.Node{},
		states:    map[string]*nodeState{},
		decisions: map[string]*decision{},
	}
}

// Exclude reports whether the aggregator must leave the node out of the
// consensus: the leader's decision if there is one, else the registry (ADR 0022).
func (a *Assessor) Exclude(nodeID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if d, ok := a.decisions[nodeID]; ok {
		return d.impaired
	}
	return a.registry[nodeID].ConnectivityImpaired
}

// Run assesses every Interval until ctx ends.
func (a *Assessor) Run(ctx context.Context) {
	ticker := time.NewTicker(a.opts.Interval)
	defer ticker.Stop()
	for {
		a.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// tick runs one assessment at the current time. Every voter keeps the registry
// as read, so Exclude is right the moment the voter becomes leader. A follower
// only drops its decisions; the leader decides and proposes what is not
// recorded yet.
func (a *Assessor) tick(ctx context.Context) {
	now := a.opts.Now()
	nodes, err := a.opts.Registry.ListNodes(ctx)
	if err != nil {
		if ctx.Err() == nil {
			a.opts.Logger.Warn("reading the node registry for connectivity failed", "err", err)
		}
	} else {
		a.setRegistry(nodes)
	}
	if a.opts.IsLeader == nil || !a.opts.IsLeader() {
		a.stepDown()
		return
	}
	if err != nil {
		return
	}
	a.mu.Lock()
	if !a.leader {
		a.leader = true
		a.leaderSince = now
	}
	since := a.leaderSince
	a.mu.Unlock()

	// Decisions wait for one full round after the leadership began, so that
	// the first answers of all peers are in (ADR 0022).
	if now.Sub(since) >= a.opts.Interval {
		a.assess(now, nodes)
	}
	a.propose(ctx)
}

// stepDown drops the decisions and streaks of a former leader. The registry
// state is the reference again.
func (a *Assessor) stepDown() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.leader {
		return
	}
	a.leader = false
	a.leaderSince = time.Time{}
	a.states = map[string]*nodeState{}
	a.decisions = map[string]*decision{}
	a.opts.Logger.Info("no longer leader, connectivity decisions dropped")
}

// setRegistry stores the registry as read, for Exclude.
func (a *Assessor) setRegistry(nodes []store.Node) {
	reg := make(map[string]store.Node, len(nodes))
	for _, n := range nodes {
		reg[n.ID] = n
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.registry = reg
}

// assess reads the signals and changes the decisions that the hysteresis and
// the guard allow. The inputs are read before the lock is taken: they may call
// the aggregator, which must not wait for the assessor.
func (a *Assessor) assess(now time.Time, nodes []store.Node) {
	var voters []store.Node
	for _, n := range nodes {
		if n.Role == store.NodeRoleVoter {
			voters = append(voters, n)
		}
	}
	sig := a.signals(now, voters)

	a.mu.Lock()
	defer a.mu.Unlock()
	a.updateStates(now, voters, sig)

	type change struct {
		id       string
		impaired bool
		reason   string
	}
	var changes []change
	for _, n := range voters {
		st, s := a.states[n.ID], sig[n.ID]
		impaired := a.impairedLocked(n.ID)
		switch {
		case !impaired && s.on && st.streak >= confirmRounds && a.othersHealthyLocked(n.ID, voters, sig):
			changes = append(changes, change{n.ID, true, s.reason})
		case impaired && !s.on && now.Sub(st.lastSignal) >= a.opts.Recovery:
			changes = append(changes, change{n.ID, false, ""})
		}
	}
	// The changes are decided on the state before this assessment, so that
	// two nodes that signal together see each other as not healthy.
	for _, c := range changes {
		a.decisions[c.id] = &decision{impaired: c.impaired, reason: c.reason}
		if c.impaired {
			a.opts.Logger.Warn("node connectivity changed", "node_id", c.id, "impaired", true, "reason", c.reason)
		} else {
			a.opts.Logger.Info("node connectivity changed", "node_id", c.id, "impaired", false, "reason", "")
		}
	}
}

// signals returns the assessment of every voter at now: a reference signal
// from the self-checks (all targets failed, round fresh) or a deviation signal
// from the heartbeats.
func (a *Assessor) signals(now time.Time, voters []store.Node) map[string]signal {
	var self selfcheck.Status
	if a.opts.Self != nil {
		self = a.opts.Self()
	}
	var peers map[string]selfcheck.Remote
	if a.opts.Peers != nil {
		peers = a.opts.Peers()
	}
	var dev map[string]aggregator.DeviationStats
	if a.opts.Deviation != nil {
		dev = a.opts.Deviation(now.Add(-deviationWindow).UnixMilli())
	}

	out := make(map[string]signal, len(voters))
	for _, n := range voters {
		var ref bool
		if n.ID == a.opts.NodeID {
			ref = referenceFailed(self, selfAge(now, self))
		} else if r, ok := peers[n.ID]; ok {
			ref = referenceFailed(r.Status, r.AgeAt(now))
		}
		d := dev[n.ID]
		switch {
		case ref:
			out[n.ID] = signal{on: true, reason: reasonReference}
		case d.Slots >= minDeviationSlots && float64(d.Down) >= a.opts.FailRatio*float64(d.Slots):
			out[n.ID] = signal{on: true, reason: reasonDeviation}
		}
	}
	return out
}

// selfAge is the age of this node's own self-check round at now.
func selfAge(now time.Time, st selfcheck.Status) time.Duration {
	if st.AtMs == 0 {
		return 0
	}
	return now.Sub(time.UnixMilli(st.AtMs))
}

// referenceFailed reports whether a round of a node is a reference signal: the
// node has targets, all of them failed in the round, and the round is fresh.
func referenceFailed(st selfcheck.Status, age time.Duration) bool {
	return st.Configured && st.Failed && st.AtMs != 0 && age <= referenceFreshness
}

// updateStates counts the streaks of the voters and forgets nodes that left
// the registry. The caller holds the lock.
func (a *Assessor) updateStates(now time.Time, voters []store.Node, sig map[string]signal) {
	present := make(map[string]bool, len(voters))
	for _, n := range voters {
		present[n.ID] = true
	}
	for id := range a.states {
		if !present[id] {
			delete(a.states, id)
		}
	}
	for _, n := range voters {
		st := a.states[n.ID]
		if st == nil {
			st = &nodeState{lastSignal: now}
			a.states[n.ID] = st
		}
		if sig[n.ID].on {
			st.streak++
			st.lastSignal = now
		} else {
			st.streak = 0
		}
	}
}

// impairedLocked is the connectivity of a node as the leader sees it now. The
// caller holds the lock.
func (a *Assessor) impairedLocked(id string) bool {
	if d, ok := a.decisions[id]; ok {
		return d.impaired
	}
	return a.registry[id].ConnectivityImpaired
}

// othersHealthyLocked is the guard of ADR 0022: a node is excluded only if the
// voters that are neither impaired nor showing a signal are at least as many
// as the others plus the node itself, i.e. 2*good > others. If as many voters
// signal as stay healthy, the target or the internet is the likelier cause and
// nobody is excluded (3 voters: 1 of 3 may be excluded, 2 of 3 not). A single
// voter is never excluded. The caller holds the lock.
func (a *Assessor) othersHealthyLocked(id string, voters []store.Node, sig map[string]signal) bool {
	if len(voters) < 2 {
		return false
	}
	others, good := 0, 0
	for _, o := range voters {
		if o.ID == id {
			continue
		}
		others++
		if !a.impairedLocked(o.ID) && !sig[o.ID].on {
			good++
		}
	}
	return 2*good > others
}

// propose records the decisions that are not persisted yet. A decision whose
// command the cluster does not accept yet (not_enabled, ADR 0018) stays in
// memory and is proposed again on the next assessment.
func (a *Assessor) propose(ctx context.Context) {
	a.mu.Lock()
	var ids []string
	for id, d := range a.decisions {
		if !d.persisted {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	type job struct {
		id  string
		cmd *pb.Command
	}
	jobs := make([]job, 0, len(ids))
	for _, id := range ids {
		d := a.decisions[id]
		if d.cmd == nil {
			d.cmd = cluster.NewSetNodeConnectivity(id, d.impaired, d.reason)
		}
		jobs = append(jobs, job{id, d.cmd})
	}
	a.mu.Unlock()

	for _, j := range jobs {
		callCtx, cancel := context.WithTimeout(ctx, applyTimeout)
		_, err := a.opts.Cluster.Apply(callCtx, j.cmd)
		cancel()
		if ctx.Err() != nil {
			return
		}
		a.settle(j.id, err)
	}
}

// settle stores the outcome of a proposal for the decision of id.
func (a *Assessor) settle(id string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	d, ok := a.decisions[id]
	if !ok {
		return
	}
	var cmdErr *cluster.CommandError
	switch {
	case err == nil:
		d.persisted = true
	case errors.Is(err, store.ErrNotFound):
		// The node left the registry; there is nothing to decide any more.
		delete(a.decisions, id)
	case errors.As(err, &cmdErr) && cmdErr.Code == cluster.CodeNotEnabled:
		a.opts.Logger.Debug("connectivity change waits for command level 3", "node_id", id, "impaired", d.impaired)
	default:
		a.opts.Logger.Warn("recording the connectivity change failed, retrying", "node_id", id, "impaired", d.impaired, "err", err)
	}
}
