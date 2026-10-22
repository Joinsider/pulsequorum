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
	TenantID    string
	ID          string
	Name        string
	Description string
	Type        string
	Config      json.RawMessage
	// Secrets maps JSON pointers into Config to ciphertexts (ADR 0015); Config
	// holds the mask at these positions. Values are never plaintext.
	Secrets               map[string]string
	IntervalSeconds       int
	TimeoutSeconds        int
	Retries               int
	RecoveryThreshold     int
	ResendIntervalMinutes int
	Assignment            Assignment
	ConsensusPolicy       string
	Tags                  []string
	// ChannelIDs are the notification channels of the monitor (FA-ALR-01).
	ChannelIDs []string
	Paused     bool
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

// Channel kinds (E7).
const (
	ChannelShoutrrr = "shoutrrr"
	ChannelWebhook  = "webhook"
)

// Channel delivery states (FA-ALR-05).
const (
	ChannelOK       = "ok"
	ChannelFailing  = "failing"
	ChannelDisabled = "disabled"
)

// Notification events a channel subscribes to.
const (
	EventDown     = "down"
	EventUp       = "up"
	EventDegraded = "degraded"
	EventPartial  = "partial"
)

// NotificationChannel is a target for alerts (FA-ALR-01). URLEnc and the
// header values are ciphertext (ADR 0011); only internal/alert decrypts them.
type NotificationChannel struct {
	TenantID string
	ID       string
	Name     string
	Kind     string
	// URLEnc is the Shoutrrr URL or the webhook URL, encrypted.
	URLEnc          string
	WebhookMethod   string
	WebhookHeaders  map[string]string // name → encrypted value
	WebhookTemplate string
	Events          []string
	IsDefault       bool
	State           string
	LastError       string
	Revision        uint64
	CreatedMs       int64
	UpdatedMs       int64
}

// Subscribes reports whether the channel wants the event.
func (c NotificationChannel) Subscribes(event string) bool {
	for _, e := range c.Events {
		if e == event {
			return true
		}
	}
	return false
}

// Alert delivery states in the outbox.
const (
	AlertPending   = "pending"
	AlertDelivered = "delivered"
	AlertFailed    = "failed"
)

// AlertDelivery is one alert for one channel in the replicated outbox (FA-ALR-03).
type AlertDelivery struct {
	TenantID      string
	AlertKey      string
	ChannelID     string
	MonitorID     string
	Payload       json.RawMessage
	Attempts      int
	NextAttemptMs int64
	State         string
	LastError     string
	CreatedMs     int64
	UpdatedMs     int64
}

// Maintenance schedule kinds (FA-MNT-01).
const (
	ScheduleOnce      = "once"
	ScheduleRecurring = "recurring"
)

// MaintenanceSchedule says when a maintenance window is active. "once" uses
// StartsMs/EndsMs. "recurring" starts at every match of Cron (5 fields, wall
// time in Timezone) and lasts DurationMinutes; StartsMs/EndsMs optionally
// bound the starts (0 = open).
type MaintenanceSchedule struct {
	Kind            string `json:"kind"`
	StartsMs        int64  `json:"startsMs,omitempty"`
	EndsMs          int64  `json:"endsMs,omitempty"`
	Cron            string `json:"cron,omitempty"`
	DurationMinutes int    `json:"durationMinutes,omitempty"`
	Timezone        string `json:"timezone,omitempty"`
}

// MaintenanceWindow suppresses alerts and uptime accounting for monitors of
// its tenant while active (FA-MNT-01/02). Empty MonitorIDs = all monitors of
// the tenant.
type MaintenanceWindow struct {
	TenantID    string
	ID          string
	Name        string
	Description string
	MonitorIDs  []string
	Schedule    MaintenanceSchedule
	Active      bool
	Revision    uint64
	CreatedMs   int64
	UpdatedMs   int64
}

// Covers reports whether the window applies to the monitor. Windows never
// apply across tenants.
func (w MaintenanceWindow) Covers(m Monitor) bool {
	if w.TenantID == "" || w.TenantID != m.TenantID {
		return false
	}
	if len(w.MonitorIDs) == 0 {
		return true
	}
	for _, id := range w.MonitorIDs {
		if id == m.ID {
			return true
		}
	}
	return false
}

// User is a local account (FA-AUTH-01). PasswordHash is an Argon2id PHC string.
// Usernames are unique across all tenants, compared case-insensitively.
type User struct {
	TenantID     string
	ID           string
	Username     string
	DisplayName  string
	PasswordHash string
	Role         string
	Disabled     bool
	// Revision starts at 1 and increases with every update (optimistic concurrency).
	Revision    uint64
	CreatedMs   int64
	UpdatedMs   int64
	LastLoginMs int64 // 0 = never logged in
}

// SigningKey signs access tokens (FA-AUTH-02). PrivateKeyEnc is ciphertext (ADR 0011).
type SigningKey struct {
	KID           string
	Alg           string
	PublicKey     []byte
	PrivateKeyEnc string
	CreatedMs     int64
	RetiredMs     int64 // 0 = not retired
}

// Session is one refresh token of a token family (FA-AUTH-03). Only its hash
// is stored. UsedMs is 0 until the token is exchanged, RevokedMs 0 while the
// token is valid.
type Session struct {
	TokenHash string
	FamilyID  string
	TenantID  string
	UserID    string
	CreatedMs int64
	ExpiresMs int64
	UsedMs    int64
	RevokedMs int64
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
	GetChannel(ctx context.Context, tenantID, id string) (NotificationChannel, error)
	ListChannels(ctx context.Context, tenantID string) ([]NotificationChannel, error)
	// DueAlerts returns pending deliveries of all tenants due at nowMs.
	DueAlerts(ctx context.Context, nowMs int64, limit int) ([]AlertDelivery, error)
	GetAlert(ctx context.Context, alertKey, channelID string) (AlertDelivery, error)
	GetMaintenanceWindow(ctx context.Context, tenantID, id string) (MaintenanceWindow, error)
	// ListMaintenanceWindows returns the tenant's windows ordered by name, ID.
	ListMaintenanceWindows(ctx context.Context, tenantID string) ([]MaintenanceWindow, error)
	// ListAllMaintenanceWindows returns the windows of all tenants ordered by tenant ID, ID.
	ListAllMaintenanceWindows(ctx context.Context) ([]MaintenanceWindow, error)

	// GetUser returns a user of the tenant.
	GetUser(ctx context.Context, tenantID, id string) (User, error)
	// GetUserByUsername returns the user with the username (case-insensitive, all tenants).
	GetUserByUsername(ctx context.Context, username string) (User, error)
	// ListUsers returns the tenant's users ordered by username.
	ListUsers(ctx context.Context, tenantID string) ([]User, error)
	// CountUsers returns the number of users of all tenants.
	CountUsers(ctx context.Context) (int, error)
	// ListSigningKeys returns all signing keys ordered by creation time and KID.
	ListSigningKeys(ctx context.Context) ([]SigningKey, error)
	// GetSession returns the refresh token with the hash or ErrNotFound.
	GetSession(ctx context.Context, tokenHash string) (Session, error)

	// StatusHistory returns the status changes in [fromMs, toMs), preceded by
	// the last change before fromMs if it is still stored, ordered by time.
	StatusHistory(ctx context.Context, tenantID, monitorID string, fromMs, toMs int64) ([]StatusChange, error)
	// StatusHistoryBatch returns StatusHistory for each listed monitor of the
	// tenant, in one query. Monitors without changes are absent from the map.
	StatusHistoryBatch(ctx context.Context, tenantID string, monitorIDs []string, fromMs, toMs int64) (map[string][]StatusChange, error)

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
	// expectedRevision+1. Channels of another tenant are ErrNotFound.
	PutMonitor(ctx context.Context, m Monitor, expectedRevision uint64) error
	DeleteMonitor(ctx context.Context, tenantID, id string) error

	GetMonitor(ctx context.Context, tenantID, id string) (Monitor, error)

	// PutChannel creates (expectedRevision 0) or replaces a channel; a
	// replacement resets its delivery state to ok.
	PutChannel(ctx context.Context, c NotificationChannel, expectedRevision uint64) error
	DeleteChannel(ctx context.Context, tenantID, id string) error
	GetChannel(ctx context.Context, tenantID, id string) (NotificationChannel, error)
	SetChannelState(ctx context.Context, tenantID, id, state, lastError string) error
	// EnqueueAlert adds a delivery unless the same alert for the channel exists.
	EnqueueAlert(ctx context.Context, a AlertDelivery) error
	GetAlert(ctx context.Context, alertKey, channelID string) (AlertDelivery, error)
	UpdateAlert(ctx context.Context, a AlertDelivery) error
	// PruneAlerts deletes finished deliveries last updated before beforeMs.
	PruneAlerts(ctx context.Context, beforeMs int64) error

	// PutMaintenanceWindow creates the window if expectedRevision is 0, otherwise
	// it replaces the window at that revision (ErrConflict on mismatch, ErrNotFound
	// if missing). Monitor IDs are normalized; created time is kept on replace.
	PutMaintenanceWindow(ctx context.Context, w MaintenanceWindow, expectedRevision uint64) error
	DeleteMaintenanceWindow(ctx context.Context, tenantID, id string) error

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

	// CountUsers returns the number of users of all tenants.
	CountUsers(ctx context.Context) (int, error)
	// GetUser returns a user of the tenant or ErrNotFound.
	GetUser(ctx context.Context, tenantID, id string) (User, error)
	// PutUser creates the user if expectedRevision is 0, otherwise it replaces
	// the user at that revision (ErrConflict on mismatch, ErrNotFound if missing).
	// A duplicate ID or username is ErrConflict. The creation time and the last
	// login are kept on replace.
	PutUser(ctx context.Context, u User, expectedRevision uint64) error
	// DeleteUser deletes the user and, by cascade, its sessions.
	DeleteUser(ctx context.Context, tenantID, id string) error
	// SetLastLogin records the time of the last successful login.
	SetLastLogin(ctx context.Context, tenantID, userID string, ms int64) error
	// CountEnabledUsersWithRole returns how many enabled users of the tenant
	// hold role, not counting excludeUserID.
	CountEnabledUsersWithRole(ctx context.Context, tenantID, role, excludeUserID string) (int, error)

	// PutSigningKey creates a signing key or updates its retirement and private
	// key ciphertext; the creation time and public key of an existing key are kept.
	PutSigningKey(ctx context.Context, k SigningKey) error

	// GetSession returns the refresh token with the hash or ErrNotFound.
	GetSession(ctx context.Context, tokenHash string) (Session, error)
	// InsertSession stores a refresh token: a duplicate hash is ErrConflict, an
	// unknown user ErrNotFound.
	InsertSession(ctx context.Context, s Session) error
	// MarkSessionUsed records the first exchange of a refresh token; later calls
	// keep the first time. ErrNotFound if the token does not exist.
	MarkSessionUsed(ctx context.Context, tokenHash string, ms int64) error
	// RevokeSessionFamily revokes all tokens of the family that are not revoked yet.
	RevokeSessionFamily(ctx context.Context, familyID string, ms int64) error
	// RevokeUserSessions revokes all tokens of the user that are not revoked yet.
	RevokeUserSessions(ctx context.Context, tenantID, userID string, ms int64) error
	// PruneSessions deletes the tokens that expired before beforeMs.
	PruneSessions(ctx context.Context, beforeMs int64) error

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

// RollupBatchQuery selects the rollups of several monitors of one node and
// bucket size with StartMs in [FromMs, ToMs).
type RollupBatchQuery struct {
	MonitorIDs []string
	NodeID     string // "" is the consensus view
	BucketMs   int64
	FromMs     int64
	ToMs       int64
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
	// LatestHeartbeatsByMonitor returns the newest heartbeat of each listed
	// monitor of the tenant over all nodes, as LatestHeartbeats with limit 1 does
	// for one monitor. Monitors without heartbeats are absent from the map.
	LatestHeartbeatsByMonitor(ctx context.Context, tenantID string, monitorIDs []string) (map[string]Heartbeat, error)
	// DeleteHeartbeatsBefore deletes at most batchSize heartbeats older than
	// beforeMs and returns the number deleted (FA-UPT-06). Callers loop until 0.
	DeleteHeartbeatsBefore(ctx context.Context, beforeMs int64, batchSize int) (int64, error)

	// PutRollups stores rollups (replacing existing ones) and the named
	// watermarks in one transaction.
	PutRollups(ctx context.Context, rollups []Rollup, watermarks map[string]int64) error
	// ListRollups returns a monitor's rollups ordered by node and time.
	ListRollups(ctx context.Context, tenantID string, q RollupQuery) ([]Rollup, error)
	// ListRollupsBatch returns the rollups of several monitors for one node
	// ("" = consensus), ordered by monitor and time.
	ListRollupsBatch(ctx context.Context, tenantID string, q RollupBatchQuery) ([]Rollup, error)
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
