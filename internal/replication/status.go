// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package replication

import "time"

// PeerStatus describes the replication state towards one peer (FA-REP-05).
type PeerStatus struct {
	ID      string
	Address string
	// Connected reports whether the push stream is open.
	Connected bool
	// CaughtUpAt is the start of the last anti-entropy round that completed.
	// Zero if the peer was never caught up.
	CaughtUpAt time.Time
	// LastError is the most recent push or anti-entropy error; empty after success.
	LastError string
	// Dropped counts batches that push did not deliver (FA-REP-07).
	Dropped uint64
}

// Lag returns how long ago the peer was last fully caught up. ok is false if
// the peer was never caught up.
func (s PeerStatus) Lag(now time.Time) (lag time.Duration, ok bool) {
	if s.CaughtUpAt.IsZero() {
		return 0, false
	}
	return now.Sub(s.CaughtUpAt), true
}
