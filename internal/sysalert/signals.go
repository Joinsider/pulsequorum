// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package sysalert evaluates the system alerts of the cluster (FA-OBS-01,
// FA-REP-05, FA-REP-06, FA-PKI-06, ADR 0025). The leader reads the signals of
// every voter from the GetStatus rounds, decides the transitions with
// hysteresis and proposes only the changes through Raft. The state lives in
// the replicated system_alerts table, so a leader change is no change.
package sysalert

import (
	"time"

	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
)

// Kinds of system alerts (ADR 0025, section 3). The quorum kind is raised by
// the degraded package, the others by the Collector.
const (
	KindVoterOffline   = "voter_offline"
	KindNodeImpaired   = "node_impaired"
	KindReplicationLag = "replication_lag"
	KindClockSkew      = "clock_skew"
	KindDiskLow        = "disk_low"
	KindWALLarge       = "wal_large"
	KindRaftStale      = "raft_stale"
	KindCertExpiry     = "cert_expiry"
	KindIncarnation    = "incarnation_changed"
	KindFingerprint    = "fingerprint_mismatch"
	KindMixedVersions  = "mixed_versions"
	KindQuorum         = "quorum"
)

// Kinds lists every kind, so that the metrics export a zero for each kind
// without an active alert.
var Kinds = []string{
	KindVoterOffline, KindNodeImpaired, KindReplicationLag, KindClockSkew, KindDiskLow, KindWALLarge,
	KindRaftStale, KindCertExpiry, KindIncarnation, KindFingerprint, KindMixedVersions, KindQuorum,
}

// Subjects that are not node IDs.
const (
	SubjectCluster = "cluster"
	SubjectCA      = "ca"
)

// Stale thresholds of a voter (ADR 0025, kind raft_stale). ADR 0012 A9 asks
// for a threshold but names no value, and no code decides the stale state
// yet, so these are the values of this package.
const (
	// StaleApplyGap is the number of log entries a voter may lag behind the
	// leader's commit index.
	StaleApplyGap = 1000
	// StaleLastContact is the time since the last contact with the leader from
	// which a voter counts as stale. It equals the health threshold of ADR 0017.
	StaleLastContact = 10 * time.Second
)

// Signals are the signals of one voter as its last GetStatus answer reported
// them (ADR 0025, section 2). They come from the cluster host's polling.
type Signals struct {
	NodeID string
	// AnsweredAt is the local time of the answer.
	AnsweredAt    time.Time
	RaftState     string
	LeaderID      string
	AppliedIndex  uint64
	CommitIndex   uint64
	LastContactMs int64
	// Incarnation is the incarnation of metrics.db; empty for older nodes.
	Incarnation string
	// DiskFreeRatio is the free share of the data directory; negative if unknown.
	DiskFreeRatio   float64
	StateWALBytes   int64
	MetricsWALBytes int64
	// CertNotAfter is the expiry of the node certificate; zero if unknown.
	CertNotAfter time.Time
	// ReplicationLagMs is the largest lag to a connected peer, -1 if a connected
	// peer never caught up.
	ReplicationLagMs int64
	// FingerprintIndex and Fingerprint describe the last FSM checkpoint.
	FingerprintIndex uint64
	Fingerprint      string
}

// FromStatus converts a GetStatus answer of the node nodeID.
func FromStatus(nodeID string, st *pb.GetStatusResponse, answeredAt time.Time) Signals {
	s := Signals{
		NodeID: nodeID, AnsweredAt: answeredAt,
		RaftState: st.GetRaftState(), LeaderID: st.GetLeaderId(),
		AppliedIndex: st.GetAppliedIndex(), CommitIndex: st.GetCommitIndex(), LastContactMs: st.GetLastContactMs(),
		Incarnation: st.GetIncarnation(), DiskFreeRatio: st.GetDiskFreeRatio(),
		StateWALBytes: st.GetStateWalBytes(), MetricsWALBytes: st.GetMetricsWalBytes(),
		ReplicationLagMs: st.GetReplicationLagMs(),
		FingerprintIndex: st.GetFingerprintIndex(), Fingerprint: st.GetFingerprint(),
	}
	if ms := st.GetCertNotAfterMs(); ms > 0 {
		s.CertNotAfter = time.UnixMilli(ms)
	}
	return s
}

// HasSystemFields reports whether the node sends the fields of ADR 0025. Every
// node with them has an incarnation, so an empty one marks an older node. Its
// zero values must not read as a signal (disk full, no lag, no certificate).
func (s Signals) HasSystemFields() bool { return s.Incarnation != "" }

// RaftStale reports whether a voter is stale: it lags behind the leader's
// commit index by more than StaleApplyGap entries, or its last contact with the
// leader is older than StaleLastContact.
func RaftStale(s Signals, leaderCommit uint64) bool {
	var gap uint64
	if leaderCommit > s.AppliedIndex {
		gap = leaderCommit - s.AppliedIndex
	}
	return gap > StaleApplyGap || time.Duration(s.LastContactMs)*time.Millisecond > StaleLastContact
}
