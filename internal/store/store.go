// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package store defines the persistence interfaces and records (ADR 0004).
//
// Data class A (configuration and state) is read through State and written only
// by the Raft FSM through State.Update. Data class B (heartbeats) lives in Metrics.
// Every tenant-scoped method takes the tenant ID explicitly; an object of another
// tenant is reported as ErrNotFound (FA-TEN-01/02).
package store

import (
	"context"
	"encoding/json"
	"errors"
)

var (
	// ErrNotFound is returned when an object does not exist or belongs to another tenant.
	ErrNotFound = errors.New("not found")
	// ErrConflict is returned on duplicate creation or a revision mismatch.
	ErrConflict = errors.New("conflict")
)

// Tenant is an isolated customer area (Konzept Kap. 2).
type Tenant struct {
	ID           string
	Name         string
	ZitadelOrgID string // empty in local mode
	Plan         string
	Limits       json.RawMessage
	Suspended    bool
	CreatedMs    int64
	UpdatedMs    int64
}

// Assignment selects the nodes that run a monitor (E1).
type Assignment struct {
	Mode  string   `json:"mode"` // all | count | nodes
	Count int      `json:"count,omitempty"`
	Nodes []string `json:"nodes,omitempty"`
}

// Monitor is a configured check.
type Monitor struct {
	TenantID              string
	ID                    string
	Name                  string
	Description           string
	Type                  string
	Config                json.RawMessage
	IntervalSeconds       int
	TimeoutSeconds        int
	Retries               int
	RecoveryThreshold     int
	ResendIntervalMinutes int
	Assignment            Assignment
	ConsensusPolicy       string
	Tags                  []string
	Paused                bool
	// Revision starts at 1 and increases with every update (optimistic concurrency).
	Revision  uint64
	CreatedMs int64
	UpdatedMs int64
	// State is filled on reads; PutMonitor ignores it (the FSM maintains it).
	State MonitorState
}

// MonitorStatus is the consensus status of a monitor (REST MonitorStatus).
type MonitorStatus string

// Monitor statuses.
const (
	StatusUp          MonitorStatus = "UP"
	StatusDown        MonitorStatus = "DOWN"
	StatusDegraded    MonitorStatus = "DEGRADED"
	StatusPartial     MonitorStatus = "PARTIAL"
	StatusPending     MonitorStatus = "PENDING"
	StatusMaintenance MonitorStatus = "MAINTENANCE"
	StatusPaused      MonitorStatus = "PAUSED"
	StatusUnknown     MonitorStatus = "UNKNOWN"
)

// Valid reports whether s is a known status.
func (s MonitorStatus) Valid() bool {
	switch s {
	case StatusUp, StatusDown, StatusDegraded, StatusPartial, StatusPending, StatusMaintenance, StatusPaused, StatusUnknown:
		return true
	}
	return false
}

// MonitorState is the last status change of a monitor (FA-CON-06).
type MonitorState struct {
	Status MonitorStatus
	// Confirmed is false if the change was made without quorum (phase 3).
	Confirmed bool
	SinceMs   int64
}

// StatusChange is one entry of the consensus status history.
type StatusChange struct {
	SinceMs int64
	Status  MonitorStatus
}

// Incident is a DOWN period of a monitor (FA-INC-01).
type Incident struct {
	TenantID    string
	ID          string
	MonitorID   string
	MonitorName string // filled on reads
	StartedMs   int64
	ResolvedMs  int64 // 0 while open
	Cause       string
	ErrorClass  string
	// AffectedNodes are the IDs of the nodes that reported DOWN.
	AffectedNodes []string
	Confirmed     bool
}

// IncidentCursor is the keyset position after which ListIncidents continues.
type IncidentCursor struct {
	StartedMs int64
	ID        string
}

// IncidentQuery filters and pages ListIncidents. Results are newest first.
type IncidentQuery struct {
	MonitorID string // empty = all monitors
	// FromMs and ToMs select incidents overlapping the period; 0 = open end.
	FromMs   int64
	ToMs     int64
	OpenOnly bool
	After    *IncidentCursor
	Limit    int
}

// IncidentPage is one page of incidents.
type IncidentPage struct {
	Items []Incident
	Next  *IncidentCursor // nil if there is no further page
}

// MonitorQuery filters and pages ListMonitors. Results are ordered by ID.
type MonitorQuery struct {
	Tag     string        // exact tag match; empty = any
	Status  MonitorStatus // empty = any
	Search  string        // case-insensitive substring of the name; empty = any
	AfterID string        // keyset cursor: return monitors with ID > AfterID
	Limit   int
}

// MonitorPage is one page of monitors.
type MonitorPage struct {
	Items []Monitor
	// NextAfterID is the cursor for the next page; empty if there is none.
	NextAfterID string
}

// State gives read access to data class A and the single write path for the FSM.
type State interface {
	GetTenant(ctx context.Context, id string) (Tenant, error)
	ListTenants(ctx context.Context) ([]Tenant, error)
	GetMonitor(ctx context.Context, tenantID, id string) (Monitor, error)
	ListMonitors(ctx context.Context, tenantID string, q MonitorQuery) (MonitorPage, error)
	GetIncident(ctx context.Context, tenantID, id string) (Incident, error)
	ListIncidents(ctx context.Context, tenantID string, q IncidentQuery) (IncidentPage, error)
	// StatusHistory returns the status changes in [fromMs, toMs), preceded by
	// the last change before fromMs if it is still stored, ordered by time.
	StatusHistory(ctx context.Context, tenantID, monitorID string, fromMs, toMs int64) ([]StatusChange, error)

	// ListSecretKeys returns all data encryption keys ordered by creation (ADR 0011).
	ListSecretKeys(ctx context.Context) ([]SecretKey, error)

	// AppliedIndex returns the last Raft log index applied to the state (0 if none).
	AppliedIndex(ctx context.Context) (uint64, error)

	// Update runs fn in one write transaction. Only the Raft FSM calls it, so
	// that every replica applies the same changes (ADR 0005, FA-CLU-07).
	Update(ctx context.Context, fn func(tx StateTx) error) error
}

// StateTx writes data class A inside State.Update. Implementations must not read
// the clock or a random source: timestamps and IDs come with the records.
type StateTx interface {
	PutTenant(ctx context.Context, t Tenant) error
	// PutMonitor creates the monitor if expectedRevision is 0, otherwise it
	// replaces the monitor at that revision. On success m.Revision is stored as
	// expectedRevision+1.
	PutMonitor(ctx context.Context, m Monitor, expectedRevision uint64) error
	DeleteMonitor(ctx context.Context, tenantID, id string) error

	// GetMonitorState returns the stored state or ErrNotFound.
	GetMonitorState(ctx context.Context, tenantID, monitorID string) (MonitorState, error)
	// PutMonitorState stores the state and records it in the status history.
	PutMonitorState(ctx context.Context, tenantID, monitorID string, st MonitorState) error
	// PruneStatusHistory deletes a monitor's changes before beforeMs except
	// the last one, which still defines the status at beforeMs.
	PruneStatusHistory(ctx context.Context, tenantID, monitorID string, beforeMs int64) error
	// OpenIncident stores a new incident; a second open incident of the same
	// monitor is ErrConflict.
	OpenIncident(ctx context.Context, inc Incident) error
	// ResolveIncident ends the open incident of a monitor, if there is one.
	ResolveIncident(ctx context.Context, tenantID, monitorID string, resolvedMs int64) error

	// PutSecretKey creates a DEK or updates its wrapping or retirement; the
	// creation time of an existing key is kept.
	PutSecretKey(ctx context.Context, k SecretKey) error

	// SetAppliedIndex records the Raft index applied by this transaction (ADR 0005).
	SetAppliedIndex(ctx context.Context, index uint64) error
	// GetDedup returns the stored outcome of a request ID or ErrNotFound.
	GetDedup(ctx context.Context, requestID string) (DedupEntry, error)
	PutDedup(ctx context.Context, e DedupEntry) error
	// PruneDedup removes entries issued before beforeMs (deterministic retention).
	PruneDedup(ctx context.Context, beforeMs int64) error
}

// SecretKey is a data encryption key wrapped with the master key (ADR 0011).
type SecretKey struct {
	KeyID          string
	WrappedDEK     []byte
	KEKFingerprint string
	CreatedMs      int64
	RetiredMs      int64 // 0 = not retired
}

// DedupEntry is the stored outcome of an applied command, so that a retry with
// the same request ID returns the original result (FA-CLU-06).
type DedupEntry struct {
	RequestID    string
	AppliedIndex uint64
	IssuedAtMs   int64
	Result       json.RawMessage
	// ErrorCode is empty on success, otherwise a stable code such as "conflict".
	ErrorCode    string
	ErrorMessage string
}

// CheckStatus is the result of one check on one node (proto CheckStatus).
type CheckStatus int

// Check results.
const (
	CheckUp       CheckStatus = 1
	CheckDown     CheckStatus = 2
	CheckDegraded CheckStatus = 3
)

// Heartbeat is one check result, unique by (NodeID, Incarnation, Seq) (FA-REP-01).
type Heartbeat struct {
	NodeID      string
	Incarnation string
	Seq         uint64
	TenantID    string
	MonitorID   string
	TsMs        int64
	Status      CheckStatus
	LatencyMs   int
	ErrorClass  string
	Message     string
	Details     json.RawMessage
}

// HeartbeatCursor is the keyset position after which ListHeartbeats continues.
type HeartbeatCursor struct {
	TsMs        int64
	NodeID      string
	Incarnation string
	Seq         uint64
}

// HeartbeatQuery selects heartbeats of one monitor in [FromMs, ToMs].
type HeartbeatQuery struct {
	MonitorID string
	NodeID    string // empty = all nodes
	FromMs    int64
	ToMs      int64
	After     *HeartbeatCursor
	Limit     int
}

// HeartbeatPage is one page of heartbeats ordered by time.
type HeartbeatPage struct {
	Items []Heartbeat
	Next  *HeartbeatCursor // nil if there is no further page
}

// Rollup bucket sizes (FA-UPT-03).
const (
	BucketHour int64 = 3_600_000
	BucketDay  int64 = 86_400_000
)

// Rollup is the uptime and latency summary of one monitor in one bucket.
// NodeID is empty for the consensus view. Durations are milliseconds.
type Rollup struct {
	TenantID  string
	MonitorID string
	NodeID    string
	BucketMs  int64
	StartMs   int64
	UpMs      int64
	DownMs    int64
	MaintMs   int64
	UnknownMs int64
	// Latency over successful checks; LatencyCount = 0 means no data.
	LatencyCount int64
	LatencyAvg   float64
	LatencyP95   float64
}

// RollupQuery selects a monitor's rollups of one bucket size with StartMs in [FromMs, ToMs).
type RollupQuery struct {
	MonitorID string
	// NodeID "" is the consensus view; AllNodes returns the consensus and every node.
	NodeID   string
	AllNodes bool
	BucketMs int64
	FromMs   int64
	ToMs     int64
}

// LatencyMinute aggregates one node's results of a monitor in one minute (FA-UPT-04).
type LatencyMinute struct {
	TenantID  string
	MonitorID string
	NodeID    string
	MinuteMs  int64
	Min       int
	Max       int
	Avg       float64
	P95       float64
	OKCount   int
	ErrCount  int
}

// HeartbeatKey identifies a heartbeat.
type HeartbeatKey struct {
	NodeID      string
	Incarnation string
	Seq         uint64
}

// Metrics stores data class B.
type Metrics interface {
	// Incarnation identifies this metrics database; it changes whenever the
	// database is recreated (ADR 0012 B4).
	Incarnation() string
	// InsertHeartbeats stores heartbeats idempotently and returns how many were new.
	InsertHeartbeats(ctx context.Context, hbs []Heartbeat) (int, error)
	// LastSeq returns the highest stored seq of a stream (0 if none).
	LastSeq(ctx context.Context, nodeID, incarnation string) (uint64, error)
	ListHeartbeats(ctx context.Context, tenantID string, q HeartbeatQuery) (HeartbeatPage, error)
	// LatestHeartbeats returns up to limit of the newest heartbeats of a
	// monitor, newest first; nodeID empty = all nodes.
	LatestHeartbeats(ctx context.Context, tenantID, monitorID, nodeID string, limit int) ([]Heartbeat, error)
	// DeleteHeartbeatsBefore deletes at most batchSize heartbeats older than
	// beforeMs and returns the number deleted (FA-UPT-06). Callers loop until 0.
	DeleteHeartbeatsBefore(ctx context.Context, beforeMs int64, batchSize int) (int64, error)

	// PutRollups stores rollups (replacing existing ones) and the named
	// watermarks in one transaction.
	PutRollups(ctx context.Context, rollups []Rollup, watermarks map[string]int64) error
	// ListRollups returns a monitor's rollups ordered by node and time.
	ListRollups(ctx context.Context, tenantID string, q RollupQuery) ([]Rollup, error)
	DeleteRollupsBefore(ctx context.Context, bucketMs, beforeMs int64, batchSize int) (int64, error)

	// Watermark returns a stored watermark, 0 if unset.
	Watermark(ctx context.Context, name string) (int64, error)
	SetWatermark(ctx context.Context, name string, value int64) error

	// CompactHeartbeats stores latency minutes and deletes heartbeats in one
	// transaction. Existing minutes are kept: they were computed from the full
	// data before an interrupted compaction (FA-UPT-04).
	CompactHeartbeats(ctx context.Context, minutes []LatencyMinute, deleted []HeartbeatKey) error
	DeleteLatencyMinutesBefore(ctx context.Context, beforeMs int64, batchSize int) (int64, error)
}
