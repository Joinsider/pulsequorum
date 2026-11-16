// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package clusterhost

import (
	"context"
	"sync"
	"time"

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

// watchClocks measures the offset of every peer voter every
// clockSampleInterval until ctx ends. Every voter measures, not only the
// leader, because the node list is answered by any voter.
func (h *Host) watchClocks(ctx context.Context) {
	ticker := time.NewTicker(h.clockInterval)
	defer ticker.Stop()
	for {
		h.measureClocks(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (h *Host) measureClocks(ctx context.Context) {
	for _, p := range h.replicationPeers() {
		callCtx, cancel := context.WithTimeout(ctx, clockCallTimeout)
		t0 := h.clock()
		st, err := h.status(callCtx, p.Address)
		t1 := h.clock()
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil || st.GetWallClockUnixMs() == 0 { // zero: a binary without the field
			continue
		}
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
			continue
		}
		h.clocks.record(p.ID, clockSample{offset: offset, at: t1})
	}
}
