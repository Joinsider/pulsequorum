// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

// Thresholds for IsStale (ADR 0012 A9, ADR 0036).
const (
	// staleContactMs is the age in milliseconds of the last contact with the
	// leader above which a follower counts as stale.
	staleContactMs = 10_000
	// staleApplyLag is the number of committed but not yet applied entries
	// above which a follower counts as stale.
	staleApplyLag = 1_000
)

// IsStale reports whether this voter lags too far behind the leader, so reads
// from it may be outdated. The leader is never stale. A voter without a known
// leader, a voter whose last contact with the leader is too old, or a voter
// that has too many committed entries left to apply is stale.
func IsStale(st Status) bool {
	if st.RaftState == "Leader" {
		return false
	}
	if st.LeaderID == "" {
		return true
	}
	if st.LastContactMs > staleContactMs {
		return true
	}
	return st.CommitIndex > st.AppliedIndex && st.CommitIndex-st.AppliedIndex > staleApplyLag
}
