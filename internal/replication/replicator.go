// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package replication distributes heartbeats between voters outside of Raft
// (data class B, ADR 0006, ADR 0012 B2/B3/B6). Pushes are best effort; the
// anti-entropy pass against every peer is what guarantees completeness.
package replication

import (
	"context"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc"

	"github.com/joinsider/uptimecluster/internal/store"
)

// Peer is another voter of the cluster.
type Peer struct {
	ID      string
	Address string
}

// Options configure a Replicator. NodeID, Metrics, Peers and Dial are required.
type Options struct {
	NodeID  string
	Metrics store.Metrics
	// Peers returns the current voters except this node (Raft configuration).
	Peers func() []Peer
	// Dial opens the connection that a peer worker owns (one per peer).
	Dial func(address string) (*grpc.ClientConn, error)
	Log  *slog.Logger
	// Now returns the current time. Default time.Now.
	Now func() time.Time
	// AntiEntropyInterval is the period of the anti-entropy pass per peer. Default 30s.
	AntiEntropyInterval time.Duration
	// PeerRefresh is the period of the peer set refresh. Default 5s.
	PeerRefresh time.Duration
	// PageSize is the maximum number of heartbeats per FetchRange. Default 5000.
	PageSize int
	// PushBuffer is the number of batches queued per peer. Default 64.
	PushBuffer int
	// OnStored is called with the heartbeats of every anti-entropy page after
	// they were stored (ADR 0021). It must not block, and the slice is only valid
	// during the call. It may be nil.
	OnStored func([]store.Heartbeat)

	// Reconnect backoff of push streams and dials (ADR 0006). Defaults 1s and 30s.
	backoffMin time.Duration
	backoffMax time.Duration
}

// Replicator pushes new heartbeats to all peers and repairs gaps by anti-entropy.
type Replicator struct {
	opts Options
	log  *slog.Logger

	mu      sync.RWMutex
	workers map[string]*worker
	wg      sync.WaitGroup
}

// New creates a Replicator. It panics if a required option is missing, since
// that is a wiring error.
func New(opts Options) *Replicator {
	if opts.NodeID == "" || opts.Metrics == nil || opts.Peers == nil || opts.Dial == nil {
		panic("replication: NodeID, Metrics, Peers and Dial are required")
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.AntiEntropyInterval <= 0 {
		opts.AntiEntropyInterval = 30 * time.Second
	}
	if opts.PeerRefresh <= 0 {
		opts.PeerRefresh = 5 * time.Second
	}
	if opts.PageSize <= 0 || opts.PageSize > maxFetchLimit {
		opts.PageSize = maxFetchLimit
	}
	if opts.PushBuffer <= 0 {
		opts.PushBuffer = 64
	}
	if opts.backoffMin <= 0 {
		opts.backoffMin = time.Second
	}
	if opts.backoffMax <= 0 {
		opts.backoffMax = 30 * time.Second
	}
	if opts.backoffMax < opts.backoffMin {
		opts.backoffMax = opts.backoffMin
	}
	return &Replicator{
		opts:    opts,
		log:     opts.Log.With("component", "replication"),
		workers: make(map[string]*worker),
	}
}

// Run keeps one worker per peer until ctx ends and then stops all workers.
func (r *Replicator) Run(ctx context.Context) {
	r.refresh(ctx)
	ticker := time.NewTicker(r.opts.PeerRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.mu.Lock()
			for id, w := range r.workers {
				w.cancel()
				delete(r.workers, id)
			}
			r.mu.Unlock()
			r.wg.Wait()
			return
		case <-ticker.C:
			r.refresh(ctx)
		}
	}
}

// refresh starts workers for new peers, stops workers of vanished peers and
// restarts workers whose address changed.
func (r *Replicator) refresh(ctx context.Context) {
	want := make(map[string]Peer)
	for _, p := range r.opts.Peers() {
		if p.ID == "" || p.ID == r.opts.NodeID {
			continue
		}
		want[p.ID] = p
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, w := range r.workers {
		if p, ok := want[id]; !ok || p.Address != w.peer.Address {
			w.cancel()
			delete(r.workers, id)
		}
	}
	for id, p := range want {
		if _, ok := r.workers[id]; !ok {
			r.startWorker(ctx, p)
		}
	}
}

// startWorker must be called with r.mu held for writing.
func (r *Replicator) startWorker(ctx context.Context, p Peer) {
	wctx, cancel := context.WithCancel(ctx)
	w := newWorker(r, p, cancel)
	r.workers[p.ID] = w
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		w.run(wctx)
	}()
}

// Publish offers freshly stored heartbeats to every peer. It never blocks: a
// peer whose buffer is full loses the batch and reconnects, and anti-entropy
// repairs the gap (FA-REP-04, FA-REP-07). The caller may reuse hbs afterwards.
func (r *Replicator) Publish(hbs []store.Heartbeat) {
	if len(hbs) == 0 {
		return
	}
	batch := slices.Clone(hbs)
	for _, w := range r.snapshot() {
		w.offer(batch)
	}
}

// Trigger requests an anti-entropy pass with the peer soon. Requests coalesce;
// unknown peers are ignored.
func (r *Replicator) Trigger(nodeID string) {
	r.mu.RLock()
	w := r.workers[nodeID]
	r.mu.RUnlock()
	if w != nil {
		w.requestAntiEntropy()
	}
}

// Status returns the state of every peer, sorted by ID.
func (r *Replicator) Status() []PeerStatus {
	ws := r.snapshot()
	out := make([]PeerStatus, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.status())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ReplicatedUntil returns the time up to which all current peers were fully
// caught up: the minimum of their last successful anti-entropy start. A peer
// that was never caught up counts as the zero time. Without peers it returns
// now.
func (r *Replicator) ReplicatedUntil() time.Time {
	ws := r.snapshot()
	if len(ws) == 0 {
		return r.opts.Now()
	}
	until := ws[0].caughtUp()
	for _, w := range ws[1:] {
		if t := w.caughtUp(); t.Before(until) {
			until = t
		}
	}
	return until
}

func (r *Replicator) snapshot() []*worker {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ws := make([]*worker, 0, len(r.workers))
	for _, w := range r.workers {
		ws = append(ws, w)
	}
	return ws
}
