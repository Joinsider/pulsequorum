// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package membership

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/pulsequorum/internal/cluster"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
)

// Timings of probe health decisions (ADR 0034 section 7).
const (
	ProbeUnhealthyAfter = 60 * time.Second
	ProbeHealthyAfter   = 30 * time.Second
	ProbeLeaderGrace    = 30 * time.Second
	probeHealthInterval = 5 * time.Second
)

// ProbeSessionInfo describes a live probe session reported by a voter.
type ProbeSessionInfo struct {
	NodeID           string
	ConnectedVoterID string
	ConnectedSinceMs int64
	LastMessageMs    int64
}

type probeHealthState struct {
	connectedSince    time.Time
	disconnectedSince time.Time
}

// ProbeHealthTracker turns merged voter probe-session observations into probe
// health decisions (ADR 0034). It does no I/O: the caller feeds it, asks
// Decide, and proposes the changes it returns. It is safe for concurrent use.
// The zero value is ready to use.
type ProbeHealthTracker struct {
	mu          sync.Mutex
	leaderSince time.Time
	probes      map[string]probeHealthState
}

// BecameLeader records when this node became leader. Until ProbeLeaderGrace
// has passed no probe health change is decided.
func (t *ProbeHealthTracker) BecameLeader(at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.leaderSince = at
}

// Reset forgets the leadership and all probe observations.
func (t *ProbeHealthTracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.leaderSince = time.Time{}
	t.probes = nil
}

// Update records which probe IDs currently have a live session on any voter.
func (t *ProbeHealthTracker) Update(now time.Time, active map[string]bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.probes == nil {
		t.probes = map[string]probeHealthState{}
	}
	for id, isActive := range active {
		if !isActive {
			continue
		}
		p := t.probes[id]
		if p.connectedSince.IsZero() {
			p.connectedSince = now
		}
		p.disconnectedSince = time.Time{}
		t.probes[id] = p
	}
	for id, p := range t.probes {
		if active[id] {
			continue
		}
		if p.disconnectedSince.IsZero() {
			p.disconnectedSince = now
		}
		p.connectedSince = time.Time{}
		t.probes[id] = p
	}
}

// Decide returns desired health changes for probes in registry where the
// decided health differs from n.Healthy. Non-probe nodes are ignored.
func (t *ProbeHealthTracker) Decide(now time.Time, registry []store.Node) map[string]bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	changes := map[string]bool{}
	if t.leaderSince.IsZero() || now.Sub(t.leaderSince) < ProbeLeaderGrace {
		return changes
	}
	for _, n := range registry {
		if n.Role != store.NodeRoleProbe {
			continue
		}
		p, seen := t.probes[n.ID]
		if !seen {
			if now.Sub(t.leaderSince) >= ProbeUnhealthyAfter && n.Healthy {
				changes[n.ID] = false
			}
			continue
		}
		switch {
		case !p.disconnectedSince.IsZero():
			if now.Sub(p.disconnectedSince) >= ProbeUnhealthyAfter && n.Healthy {
				changes[n.ID] = false
			}
		case !p.connectedSince.IsZero():
			if now.Sub(p.connectedSince) >= ProbeHealthyAfter && !n.Healthy {
				changes[n.ID] = true
			}
		}
	}
	return changes
}

// ProbeHealthWatcher runs probe health decisions on the leader (ADR 0034): it
// reads merged probe sessions reported by voters, feeds ProbeHealthTracker, and
// proposes SetNodeHealth commands to the cluster.
type ProbeHealthWatcher struct {
	Node interface {
		IsLeader() bool
		Apply(context.Context, *pb.Command) (cluster.ApplyResult, error)
	}
	State    store.State
	Sessions func() map[string]ProbeSessionInfo
	Log      *slog.Logger
	Now      func() time.Time
	Interval time.Duration

	tracker ProbeHealthTracker
	leader  bool
}

func (w *ProbeHealthWatcher) now() time.Time {
	if w.Now == nil {
		return time.Now()
	}
	return w.Now()
}

func (w *ProbeHealthWatcher) interval() time.Duration {
	if w.Interval <= 0 {
		return probeHealthInterval
	}
	return w.Interval
}

func (w *ProbeHealthWatcher) log() *slog.Logger {
	if w.Log == nil {
		return slog.Default()
	}
	return w.Log
}

// Run decides on probe health changes every Interval until ctx ends.
func (w *ProbeHealthWatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval())
	defer ticker.Stop()
	for {
		w.tick(ctx, w.now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *ProbeHealthWatcher) tick(ctx context.Context, now time.Time) {
	if w.Node == nil || !w.Node.IsLeader() {
		if w.leader {
			w.leader = false
			w.tracker.Reset()
		}
		return
	}
	if !w.leader {
		w.leader = true
		w.tracker.Reset()
		w.tracker.BecameLeader(now)
	}

	active := map[string]bool{}
	if w.Sessions != nil {
		for id, info := range w.Sessions() {
			nodeID := info.NodeID
			if nodeID == "" {
				nodeID = id
			}
			active[nodeID] = true
		}
	}

	nodes, err := w.State.ListNodes(ctx)
	if err != nil {
		if ctx.Err() == nil {
			w.log().Warn("reading the node registry for probe health failed", "err", err)
		}
		return
	}

	w.tracker.Update(now, active)
	changes := w.tracker.Decide(now, nodes)
	for _, id := range slices.Sorted(maps.Keys(changes)) {
		w.propose(ctx, id, changes[id])
	}
}

func (w *ProbeHealthWatcher) propose(ctx context.Context, id string, healthy bool) {
	callCtx, cancel := context.WithTimeout(ctx, changeTimeout)
	defer cancel()
	if _, err := w.Node.Apply(callCtx, cluster.NewSetNodeHealth(uuid.NewString(), id, healthy)); err != nil {
		if ctx.Err() == nil {
			w.log().Warn("changing probe health failed, retrying", "node_id", id, "healthy", healthy, "err", err)
		}
		return
	}
	if healthy {
		w.log().Info("probe healthy", "node_id", id)
		return
	}
	w.log().Warn("probe unhealthy: no live session reported by any voter", "node_id", id)
}
