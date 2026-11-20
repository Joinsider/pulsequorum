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
	"github.com/joinsider/pulsequorum/internal/store"
)

// Timings of the health decisions (ADR 0017). healthThreshold is 10 × the
// HeartbeatTimeout of the default configuration.
const (
	healthThreshold = 10 * time.Second
	healthInterval  = time.Second
)

// peerHealth is the heartbeat history of one peer as the leader saw it.
type peerHealth struct {
	// failingSince is the first failed heartbeat since the last resume; zero
	// if the peer does not currently fail.
	failingSince time.Time
	// resumedAt is the last resume; zero if the peer never failed.
	resumedAt time.Time
}

// HealthTracker turns the heartbeat observations of the leader into health
// decisions (ADR 0017). It does no I/O: the caller feeds it, asks Decide and
// proposes the changes it returns. It is safe for concurrent use. The zero
// value is ready to use.
type HealthTracker struct {
	mu sync.Mutex
	// leaderSince is when this node became leader; zero if it is not.
	leaderSince time.Time
	peers       map[string]peerHealth
}

// Failed records a failed heartbeat to peer at the given time.
func (t *HealthTracker) Failed(peer string, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.peers[peer]
	if p.failingSince.IsZero() {
		p.failingSince = at
	}
	t.set(peer, p)
}

// Resumed records that heartbeats to peer succeed again.
func (t *HealthTracker) Resumed(peer string, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.set(peer, peerHealth{resumedAt: at})
}

// BecameLeader records when this node became leader. Until healthThreshold
// has passed no peer is decided healthy, because the heartbeats of the
// previous leader tell nothing about the new one.
func (t *HealthTracker) BecameLeader(at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.leaderSince = at
}

// Reset forgets the leadership and all observations. The caller resets when
// this node stops being leader and before it becomes leader again.
func (t *HealthTracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.leaderSince = time.Time{}
	t.peers = nil
}

// Decide returns the health changes to propose at now. Only peers that are in
// peers and in registry are decided; registry maps node IDs to their current
// health. A change is returned only if the decided value differs from the
// registry. Without leadership nothing is decided.
//
// A peer is unhealthy once its heartbeats failed for healthThreshold. It is
// healthy again once it has been without failure for healthThreshold, and the
// node has been leader for that long too; a peer that never failed counts as
// healthy after the leader grace. Failures shorter than the threshold change
// nothing, which keeps brief interruptions from making a node flap.
func (t *HealthTracker) Decide(now time.Time, peers []string, registry map[string]bool) map[string]bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	changes := map[string]bool{}
	if t.leaderSince.IsZero() {
		return changes
	}
	for _, id := range peers {
		current, registered := registry[id]
		if !registered {
			continue
		}
		if want, decided := t.verdict(now, id); decided && want != current {
			changes[id] = want
		}
	}
	return changes
}

// verdict returns the health of peer at now and whether it is decided yet.
// The caller holds the lock.
func (t *HealthTracker) verdict(now time.Time, peer string) (healthy, decided bool) {
	p := t.peers[peer]
	switch {
	case !p.failingSince.IsZero():
		return false, now.Sub(p.failingSince) >= healthThreshold
	case now.Sub(t.leaderSince) < healthThreshold:
		return false, false
	case p.resumedAt.IsZero() || now.Sub(p.resumedAt) >= healthThreshold:
		return true, true
	}
	return false, false
}

// set stores the history of peer. The caller holds the lock.
func (t *HealthTracker) set(peer string, p peerHealth) {
	if t.peers == nil {
		t.peers = map[string]peerHealth{}
	}
	t.peers[peer] = p
}

// HealthWatcher runs the health decisions of the leader (ADR 0017): it feeds
// the heartbeat observations of Raft into a HealthTracker and proposes the
// changes to the node registry. Only the leader proposes.
type HealthWatcher struct {
	Node  *cluster.Node
	State store.State
	Log   *slog.Logger
	// Now is the wall clock of this node; nil means time.Now. It must be the
	// clock the heartbeat observations are stamped with (ADR 0020).
	Now func() time.Time

	tracker HealthTracker
	// leader is true between BecameLeader and the next step down.
	leader bool
}

// Observe records one heartbeat observation of the leader. It is the callback
// of cluster.Node.OnHeartbeat.
func (w *HealthWatcher) Observe(peerID string, ok bool, at time.Time) {
	if ok {
		w.tracker.Resumed(peerID, at)
		return
	}
	w.tracker.Failed(peerID, at)
}

// now returns the clock of the watcher.
func (w *HealthWatcher) now() time.Time {
	if w.Now == nil {
		return time.Now()
	}
	return w.Now()
}

// Run decides on health changes every healthInterval until ctx ends.
func (w *HealthWatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(healthInterval)
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

// tick runs one decision at now.
func (w *HealthWatcher) tick(ctx context.Context, now time.Time) {
	if !w.Node.IsLeader() {
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

	members, _, err := w.Node.Members()
	if err != nil {
		w.Log.Debug("reading the cluster configuration for health failed", "err", err)
		return
	}
	nodes, err := w.State.ListNodes(ctx)
	if err != nil {
		if ctx.Err() == nil {
			w.Log.Warn("reading the node registry for health failed", "err", err)
		}
		return
	}
	registry := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		registry[n.ID] = n.Healthy
	}
	self := w.Node.ID()
	var peers []string
	for _, m := range members {
		if m.Voter && m.ID != self {
			peers = append(peers, m.ID)
		}
	}
	changes := w.tracker.Decide(now, peers, registry)
	// This node is healthy by definition; a leader that was marked unhealthy
	// before it took over corrects that itself.
	if healthy, registered := registry[self]; registered && !healthy {
		changes[self] = true
	}
	for _, id := range slices.Sorted(maps.Keys(changes)) {
		w.propose(ctx, id, changes[id])
	}
}

// propose records one health change in the registry.
func (w *HealthWatcher) propose(ctx context.Context, id string, healthy bool) {
	callCtx, cancel := context.WithTimeout(ctx, changeTimeout)
	defer cancel()
	if _, err := w.Node.Apply(callCtx, cluster.NewSetNodeHealth(uuid.NewString(), id, healthy)); err != nil {
		if ctx.Err() == nil {
			w.Log.Warn("changing node health failed, retrying", "node_id", id, "healthy", healthy, "err", err)
		}
		return
	}
	if healthy {
		w.Log.Info("node healthy", "node_id", id)
		return
	}
	w.Log.Warn("node unhealthy: heartbeats from the leader failed", "node_id", id)
}
