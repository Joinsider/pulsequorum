// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package clusterhost

import (
	"context"
	"sync"
	"time"

	"github.com/joinsider/uptimecluster/internal/degraded"
	"github.com/joinsider/uptimecluster/internal/replication"
	"github.com/joinsider/uptimecluster/internal/selfcheck"
)

// Timings of the clock offset measurement (ADR 0020).
const (
	// clockSampleInterval is the period of the measurement per peer.
	clockSampleInterval = 15 * time.Second
	// clockMaxRTT discards samples whose round trip is too long to bound the error.
	clockMaxRTT = time.Second
	// clockMaxAge drops a sample that is no longer fresh.
	clockMaxAge = 2 * time.Minute
	// clockCallTimeout bounds one GetStatus call of the measurement.
	clockCallTimeout = 5 * time.Second
	// leaderlessPollInterval is the period of the GetStatus polling while this
	// node knows no leader (ADR 0024, section 2).
	leaderlessPollInterval = 5 * time.Second
)

// estimateOffset returns how far the remote wall clock is ahead of this node,
// from the local clock before (t0) and after (t1) a call and the remote time
// the callee reported in its answer. The remote time is assumed to be read in
// the middle of the call (ADR 0020). It returns false for a round trip longer
// than clockMaxRTT or a clock that went backwards during the call.
func estimateOffset(t0, t1, remote time.Time) (time.Duration, bool) {
	rtt := t1.Sub(t0)
	if rtt < 0 || rtt > clockMaxRTT {
		return 0, false
	}
	return remote.Sub(t0.Add(rtt / 2)), true
}

// clockSample is the last good offset estimate of a peer and the local time
// it was taken.
type clockSample struct {
	offset time.Duration
	at     time.Time
}

// clockTracker keeps the last good sample per peer. Failed or slow calls do
// not replace a sample; a sample older than clockMaxAge is dropped.
type clockTracker struct {
	mu      sync.Mutex
	samples map[string]clockSample
}

func (t *clockTracker) record(peerID string, s clockSample) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.samples == nil {
		t.samples = map[string]clockSample{}
	}
	t.samples[peerID] = s
}

// offsets returns the fresh offsets in milliseconds at now, by peer ID.
func (t *clockTracker) offsets(now time.Time) map[string]int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]int64, len(t.samples))
	for id, s := range t.samples {
		if now.Sub(s.at) > clockMaxAge {
			delete(t.samples, id)
			continue
		}
		out[id] = s.offset.Milliseconds()
	}
	return out
}

// ClockOffsets returns the clock offset of each peer in milliseconds, positive
// when the peer is ahead of this node. Only peers with a fresh sample appear
// (ADR 0020).
func (h *Host) ClockOffsets() map[string]int64 {
	return h.clocks.offsets(h.clock())
}

// PeerSelfChecks returns the self-check state of each peer voter as the last
// GetStatus answer reported it, by node ID (ADR 0022). Only peers with an
// answer in the last clockMaxAge appear.
func (h *Host) PeerSelfChecks() map[string]selfcheck.Remote {
	return h.selfChecks.snapshot(h.clock())
}

// selfCheckTracker keeps the last self-check answer per peer.
type selfCheckTracker struct {
	mu      sync.Mutex
	remotes map[string]selfcheck.Remote
}

func (t *selfCheckTracker) record(peerID string, r selfcheck.Remote) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.remotes == nil {
		t.remotes = map[string]selfcheck.Remote{}
	}
	t.remotes[peerID] = r
}

// snapshot returns a copy of the answers received within clockMaxAge of now.
func (t *selfCheckTracker) snapshot(now time.Time) map[string]selfcheck.Remote {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]selfcheck.Remote, len(t.remotes))
	for id, r := range t.remotes {
		if now.Sub(r.ReceivedAt) > clockMaxAge {
			delete(t.remotes, id)
			continue
		}
		out[id] = r
	}
	return out
}

// clockWait returns the pause between two rounds of measureClocks. While the
// node knows no leader, the pause is at most leaderlessPollInterval, so that
// the degraded detector sees fresh peer views (ADR 0024, section 2).
func clockWait(normal time.Duration, leaderless bool) time.Duration {
	if leaderless && normal > leaderlessPollInterval {
		return leaderlessPollInterval
	}
	return normal
}

// watchClocks measures the offset of every peer voter every
// clockSampleInterval until ctx ends. Every voter measures, not only the
// leader, because the node list is answered by any voter.
func (h *Host) watchClocks(ctx context.Context) {
	for {
		h.measureClocks(ctx)
		timer := time.NewTimer(clockWait(h.clockInterval, h.LeaderID() == ""))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// LeaderID returns the ID of the leader known to this node, "" if none.
func (h *Host) LeaderID() string {
	_, id := h.Node.Leader()
	return id
}

// CaughtUp reports whether this node has applied the commit index it knows
// of. It does not check for a leader; the detector does.
func (h *Host) CaughtUp() bool {
	st := h.Node.Status()
	return st.AppliedIndex >= st.CommitIndex
}

// PeerViews returns the last GetStatus answer of each peer voter, by node ID,
// for the degraded detector (ADR 0024). Only answers within clockMaxAge
// appear.
func (h *Host) PeerViews() map[string]degraded.PeerView {
	return h.peerViews.snapshot(h.clock())
}

// peerViewTracker keeps the last GetStatus answer per peer as a
// degraded.PeerView.
type peerViewTracker struct {
	mu    sync.Mutex
	views map[string]degraded.PeerView
}

func (t *peerViewTracker) record(peerID string, v degraded.PeerView) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.views == nil {
		t.views = map[string]degraded.PeerView{}
	}
	t.views[peerID] = v
}

// snapshot returns a copy of the views answered within clockMaxAge of now.
func (t *peerViewTracker) snapshot(now time.Time) map[string]degraded.PeerView {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]degraded.PeerView, len(t.views))
	for id, v := range t.views {
		if now.Sub(v.AnsweredAt) > clockMaxAge {
			delete(t.views, id)
			continue
		}
		out[id] = v
	}
	return out
}

// measureClocks polls all peers concurrently, so that unreachable peers,
// which wait for the call timeout, do not delay the views of reachable ones
// (ADR 0024: a reachable peer must stay fresher than ReachableWithin).
func (h *Host) measureClocks(ctx context.Context) {
	var wg sync.WaitGroup
	for _, p := range h.replicationPeers() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.measurePeer(ctx, p)
		}()
	}
	wg.Wait()
}

func (h *Host) measurePeer(ctx context.Context, p replication.Peer) {
	callCtx, cancel := context.WithTimeout(ctx, clockCallTimeout)
	t0 := h.clock()
	st, err := h.status(callCtx, p.Address)
	t1 := h.clock()
	cancel()
	if ctx.Err() != nil {
		return
	}
	if err != nil || st.GetWallClockUnixMs() == 0 { // zero: a binary without the field
		return
	}
	// A peer without leader_id would look leaderless, so it is recorded
	// only with the field set (same guard as the wall clock above).
	h.peerViews.record(p.ID, degraded.PeerView{
		AnsweredAt: t1, LeaderID: st.GetLeaderId(), SelfCheckFailed: st.GetSelfCheckFailed(),
	})
	// The self-check answer rides on the same poll (ADR 0022). Its age is
	// taken on the peer's clock, so a skewed clock does not distort it.
	h.selfChecks.record(p.ID, selfcheck.Remote{
		Status: selfcheck.Status{
			Configured: st.GetSelfCheckConfigured(), Failed: st.GetSelfCheckFailed(), AtMs: st.GetSelfCheckAtMs(),
		},
		ReceivedAt: t1,
		Age:        time.Duration(st.GetWallClockUnixMs()-st.GetSelfCheckAtMs()) * time.Millisecond,
	})
	offset, ok := estimateOffset(t0, t1, time.UnixMilli(st.GetWallClockUnixMs()))
	if !ok {
		h.log.Debug("clock sample discarded: round trip too long", "peer", p.ID, "rtt", t1.Sub(t0))
		return
	}
	h.clocks.record(p.ID, clockSample{offset: offset, at: t1})
}
