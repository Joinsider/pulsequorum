// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package degraded decides whether this voter is in the degraded mode of
// ADR 0024: it has known no leader for a while, so the cluster cannot decide
// alerts. It also elects which reachable voter of the partition sends them.
package degraded

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"
)

// Defaults of the detector (ADR 0024, section 1 and 2).
const (
	DefaultAfter           = 15 * time.Second
	DefaultReachableWithin = 15 * time.Second
	DefaultInterval        = time.Second
)

// PeerView is what this node knows about a peer voter from its last
// GetStatus answer.
type PeerView struct {
	AnsweredAt      time.Time // last successful GetStatus answer (zero: never)
	LeaderID        string    // leader as seen by the peer; "" = none
	SelfCheckFailed bool
}

// Options configures a Detector. Nil functions mean: LeaderID and CaughtUp
// are required; Voters nil means no registry (Total 0, partition is the node
// itself); Peers nil means no peer views; SelfCheckFailed nil means passed.
type Options struct {
	NodeID          string
	LeaderID        func() string                               // leader known to this node, "" if none
	CaughtUp        func() bool                                 // applied index has reached the leader's commit index
	Voters          func(ctx context.Context) ([]string, error) // registered voter IDs from the local (read-only) registry
	Peers           func() map[string]PeerView                  // from the cluster host's GetStatus polling
	SelfCheckFailed func() bool                                 // own reference self-check failed (ADR 0022)
	After           time.Duration                               // DEGRADED_AFTER, default 15s; 0 means default, <0 disables the detector
	ReachableWithin time.Duration                               // default 15s
	Interval        time.Duration                               // evaluation tick, default 1s
	Now             func() time.Time                            // default time.Now
	Logger          *slog.Logger                                // default slog.Default()
}

// State is a snapshot of the detector.
type State struct {
	Degraded  bool
	Since     time.Time // start of the leaderless period (when Degraded)
	Partition []string  // sorted node IDs: self + reachable peers that also report no leader
	Sender    string    // elected sender ID ("" when not degraded)
	Reachable int       // len(Partition)
	Total     int       // registered voters
}

// Detector tracks the degraded state of this node. Update evaluates it once;
// Run calls Update periodically.
type Detector struct {
	opts    Options
	changes chan struct{}

	// evalMu serializes Update. It guards leaderlessSince and lastVoters.
	evalMu          sync.Mutex
	leaderlessSince time.Time
	// lastVoters is the last registry read that succeeded. It is used while
	// the registry cannot be read.
	lastVoters []string

	// mu guards state, so that State and IsSender can run during an Update.
	mu    sync.Mutex
	state State
}

// New returns a Detector with defaults applied to opts.
func New(opts Options) *Detector {
	if opts.After == 0 {
		opts.After = DefaultAfter
	}
	if opts.ReachableWithin <= 0 {
		opts.ReachableWithin = DefaultReachableWithin
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Detector{opts: opts, changes: make(chan struct{}, 1)}
}

// Run evaluates the detector every Interval until ctx ends. A disabled
// detector (negative After) returns at once.
func (d *Detector) Run(ctx context.Context) {
	if d.opts.After < 0 {
		return
	}
	ticker := time.NewTicker(d.opts.Interval)
	defer ticker.Stop()
	d.Update(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.Update(ctx)
		}
	}
}

// Update evaluates the detector once. It does nothing when the detector is
// disabled.
func (d *Detector) Update(ctx context.Context) {
	if d.opts.After < 0 {
		return
	}
	d.evalMu.Lock()
	defer d.evalMu.Unlock()

	now := d.opts.Now()
	prev := d.State()
	next := prev

	// Degraded mode (section 1): leaderless for at least After, ends once a
	// leader is known and its commit index is applied locally.
	leader := d.opts.LeaderID()
	if leader == "" {
		if d.leaderlessSince.IsZero() {
			d.leaderlessSince = now
		}
	} else {
		d.leaderlessSince = time.Time{}
	}
	caughtUp := d.opts.CaughtUp == nil || d.opts.CaughtUp()
	switch {
	case leader == "" && !next.Degraded && now.Sub(d.leaderlessSince) >= d.opts.After:
		next.Degraded = true
		next.Since = d.leaderlessSince
	case leader != "" && next.Degraded && caughtUp:
		next = State{Total: next.Total}
	}

	// Registry: keep the last good read if it fails.
	if d.opts.Voters != nil {
		voters, err := d.opts.Voters(ctx)
		if err != nil {
			d.opts.Logger.Warn("degraded: voter registry unreadable, keeping last known voters", "err", err)
		} else {
			d.lastVoters = voters
		}
	}
	next.Total = len(d.lastVoters)

	// Partition and sender (section 2) are only needed while degraded.
	next.Partition, next.Reachable, next.Sender = nil, 0, ""
	if next.Degraded {
		peers := map[string]PeerView{}
		if d.opts.Peers != nil {
			peers = d.opts.Peers()
		}
		next.Partition = d.partition(now, d.lastVoters, peers)
		next.Reachable = len(next.Partition)
		next.Sender = d.elect(next.Partition, peers)
	}

	d.mu.Lock()
	d.state = next
	d.mu.Unlock()

	d.report(prev, next)
}

// partition returns the sorted node IDs of this node and every voter peer
// that answered within ReachableWithin and reports no leader.
func (d *Detector) partition(now time.Time, voters []string, peers map[string]PeerView) []string {
	out := []string{d.opts.NodeID}
	for _, id := range voters {
		if id == d.opts.NodeID {
			continue
		}
		v, ok := peers[id]
		if !ok || v.AnsweredAt.IsZero() || now.Sub(v.AnsweredAt) > d.opts.ReachableWithin {
			continue
		}
		if v.LeaderID != "" {
			continue
		}
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// elect returns the smallest ID of the partition whose self-check did not
// fail. If every member failed, it returns the smallest ID overall.
func (d *Detector) elect(partition []string, peers map[string]PeerView) string {
	if len(partition) == 0 {
		return ""
	}
	for _, id := range partition {
		if !d.selfCheckFailed(id, peers) {
			return id
		}
	}
	return partition[0]
}

func (d *Detector) selfCheckFailed(id string, peers map[string]PeerView) bool {
	if id == d.opts.NodeID {
		return d.opts.SelfCheckFailed != nil && d.opts.SelfCheckFailed()
	}
	return peers[id].SelfCheckFailed
}

// report logs the transitions between prev and next and signals Changes when
// Degraded or IsSender changed.
func (d *Detector) report(prev, next State) {
	log := d.opts.Logger
	switch {
	case !prev.Degraded && next.Degraded:
		log.Info("entering degraded mode: no leader",
			"since", next.Since, "partition", next.Partition, "sender", next.Sender,
			"reachable", next.Reachable, "total", next.Total)
	case prev.Degraded && !next.Degraded:
		log.Info("leaving degraded mode", "since", prev.Since, "partition", prev.Partition)
	case next.Degraded && prev.Sender != next.Sender:
		log.Info("degraded sender changed",
			"since", next.Since, "partition", next.Partition, "sender", next.Sender)
	}
	wasSender := prev.Degraded && prev.Sender == d.opts.NodeID
	isSender := next.Degraded && next.Sender == d.opts.NodeID
	if prev.Degraded != next.Degraded || wasSender != isSender {
		select {
		case d.changes <- struct{}{}:
		default:
		}
	}
}

// State returns a snapshot of the current state. It is safe for concurrent
// use.
func (d *Detector) State() State {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.state
	s.Partition = slices.Clone(s.Partition)
	return s
}

// IsSender reports whether this node is degraded and elected sender.
func (d *Detector) IsSender() bool {
	s := d.State()
	return s.Degraded && s.Sender == d.opts.NodeID
}

// Changes is signalled, without blocking, on every change of Degraded or
// IsSender. Consecutive changes may share one signal, so readers re-read
// State.
func (d *Detector) Changes() <-chan struct{} {
	return d.changes
}
