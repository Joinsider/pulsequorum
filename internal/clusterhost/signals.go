// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package clusterhost

import (
	"context"
	"sync"
	"time"

	"github.com/joinsider/pulsequorum/internal/membership"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/sysalert"
)

// ownSignals returns the GetStatus signals of this node (ADR 0025, section 2).
// The disk and WAL functions come from the caller, which owns the data
// directory and the databases.
func (h *Host) ownSignals(opts Options) membership.OwnSignals {
	own := membership.OwnSignals{
		DiskFreeRatio: opts.DiskFreeRatio,
		WALBytes:      opts.WALBytes,
		CertNotAfter: func() time.Time {
			if leaf := h.Cert.Leaf(); leaf != nil {
				return leaf.NotAfter
			}
			return time.Time{}
		},
		ReplicationLagMs: h.replicationLagMs,
		Checkpoint: func() (uint64, string) {
			return h.Node.LastCheckpoint()
		},
	}
	if own.DiskFreeRatio == nil {
		// A zero would read as a full disk; negative is no signal.
		own.DiskFreeRatio = func() float64 { return -1 }
	}
	if opts.Metrics != nil {
		own.Incarnation = opts.Metrics.Incarnation
	}
	return own
}

// replicationLagMs is the largest lag to a connected peer in milliseconds, 0
// without connected peers, and -1 if a connected peer never caught up.
func (h *Host) replicationLagMs() int64 {
	now := h.clock()
	var lag int64
	for _, p := range h.Replication.Status() {
		if !p.Connected {
			continue
		}
		l, ok := p.Lag(now)
		if !ok {
			return -1
		}
		lag = max(lag, l.Milliseconds())
	}
	return lag
}

// PeerSignals returns the signals of each peer voter from its last GetStatus
// answer, by node ID (ADR 0025). Only answers within clockMaxAge appear.
func (h *Host) PeerSignals() map[string]sysalert.Signals {
	return h.signals.snapshot(h.clock())
}

// SelfSignals returns the signals of this node, as its own GetStatus answer
// reports them (ADR 0025).
func (h *Host) SelfSignals() sysalert.Signals {
	st, err := h.service.GetStatus(context.Background(), &pb.GetStatusRequest{})
	if err != nil {
		return sysalert.Signals{NodeID: h.Node.ID()}
	}
	return sysalert.FromStatus(h.Node.ID(), st, h.clock())
}

// signalTracker keeps the last GetStatus signals per peer.
type signalTracker struct {
	mu      sync.Mutex
	signals map[string]sysalert.Signals
}

func (t *signalTracker) record(peerID string, s sysalert.Signals) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.signals == nil {
		t.signals = map[string]sysalert.Signals{}
	}
	t.signals[peerID] = s
}

// snapshot returns a copy of the signals answered within clockMaxAge of now.
func (t *signalTracker) snapshot(now time.Time) map[string]sysalert.Signals {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]sysalert.Signals, len(t.signals))
	for id, s := range t.signals {
		if now.Sub(s.AnsweredAt) > clockMaxAge {
			delete(t.signals, id)
			continue
		}
		out[id] = s
	}
	return out
}
