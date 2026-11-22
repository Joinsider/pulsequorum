// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

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
	"slices"
)

// SystemTenantID is the reserved tenant that holds the system channels of the
// platform admins (ADR 0025). It has no monitors and no users; principals of
// it are rejected, and only the system actor may create it.
const SystemTenantID = "_system"

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
	// Source is how the tenant was created: admin, auto or bootstrap (ADR 0029).
	Source    string
	CreatedMs int64
	UpdatedMs int64
}

// Plan is a named set of tenant limits (ADR 0029). Limits is the canonical
// JSON of tenant.Limits.
type Plan struct {
	ID        string
	Name      string
	Limits    json.RawMessage
	CreatedMs int64
	UpdatedMs int64
}

// TenantUsage counts what a tenant uses of its limits (ADR 0029).
type TenantUsage struct {
	Monitors int
	Channels int
	// PrivateProbes counts registered private probes plus open join tokens when
	// populated for API/limit views (ADR 0035).
	PrivateProbes int
	// MinIntervalSeconds is the shortest monitor interval, 0 without monitors.
	MinIntervalSeconds int
}

// DeletedTenant is the tombstone of a deleted tenant (ADR 0029). Its ID is
// never reused.
type DeletedTenant struct {
	ID        string
	DeletedMs int64
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
	// Flapping is true while the monitor flaps (FA-ALR-06, ADR 0023);
	// FlappingSinceMs is the time the period started. Only SetMonitorFlapping
	// writes them, so PutMonitorState keeps them.
	Flapping        bool
	FlappingSinceMs int64
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
	// EventFlapping starts and ends a flapping period (FA-ALR-06, ADR 0023).
	EventFlapping = "flapping"
	// EventChannelFailed reports another channel of the tenant that failed
	// permanently or gave up (FA-ALR-05, ADR 0023).
	EventChannelFailed = "channel_failed"
	// EventProbeOffline reports that a private probe of the tenant went offline
	// or recovered (FA-PRB-06, ADR 0035).
	EventProbeOffline = "probe_offline"
	// EventSystem reports a system alert (FA-OBS-01, ADR 0025). Only channels
	// of the system tenant subscribe to it.
	EventSystem = "system"
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

// SessionUser names a user that holds a refresh token (ADR 0032).
type SessionUser struct {
	TenantID string
	UserID   string
}

// APIToken is a long-lived service token of a tenant (ADR 0031, FA-AUTH-10).
// Only the SHA-256 of the secret is stored.
type APIToken struct {
	ID         string // public ID, "tok_" + base32
	TenantID   string
	Scope      string // "tenant"; other scopes need a later ADR
	Name       string
	Role       string // tenant-viewer, tenant-editor or tenant-admin
	SecretHash string
	CreatedMs  int64
	ExpiresMs  int64
	// CreatedBy is who created the token.
	CreatedByKind string
	CreatedByID   string
	CreatedByName string
}

// AuditEntry is one entry of the audit log (ADR 0031, FA-DAT-01). It names
// the action and the object, never contents or secrets.
type AuditEntry struct {
	// Index is the Raft index of the command, Sub the position within a
	// Batch command (0 otherwise). Together they order the log.
	Index      uint64
	Sub        int
	AtMs       int64
	TenantID   string
	ActorKind  string // user, api_token or system
	ActorID    string
	ActorName  string
	Action     string // e.g. monitor.create
	ObjectKind string
	ObjectID   string
	RequestID  string
}

// AuditCursor continues an audit listing after the given entry.
type AuditCursor struct {
	Index uint64
	Sub   int
}

// AuditQuery filters the audit log. Empty fields do not filter. Results are
// ordered newest first.
type AuditQuery struct {
	// TenantID restricts the result to one tenant; empty means all tenants
	// (platform admins only, enforced by the API).
	TenantID   string
	FromMs     int64 // inclusive, 0 = unbounded
	ToMs       int64 // exclusive, 0 = unbounded
	Action     string
	ObjectKind string
	ObjectID   string
	ActorID    string
	After      *AuditCursor
	Limit      int
}

// AuditPage is one page of the audit log; Next is nil on the last page.
type AuditPage struct {
	Entries []AuditEntry
	Next    *AuditCursor
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
	// Reevaluated is true for an incident added to a gap in the history after
	// the quorum returned (FA-UPT-07, ADR 0024). It never caused a notification.
	Reevaluated bool
}

// Verdicts of an unconfirmed alert, judged by the leader after the quorum
// returned (FA-ALR-10, ADR 0024).
const (
	VerdictConfirmed    = "confirmed"
	VerdictFalseAlarm   = "false_alarm"
	VerdictUnverifiable = "unverifiable"
)

// UnconfirmedAlert is an alert a voter sent while it was degraded, reported to
// the leader after the quorum returned (ADR 0024). Verdict and ResolvedMs are
// empty while the alert is open.
type UnconfirmedAlert struct {
	NodeID     string
	LocalID    int64
	TenantID   string
	MonitorID  string
	ChannelID  string
	Status     string
	SinceMs    int64
	SentMs     int64
	Outcome    string
	Reachable  int
	Total      int
	Reporters  []string
	ReportedMs int64
	Verdict    string
	ResolvedMs int64
}

// DegradedAlert is a row of the node-local log of alerts a voter sent while it
// was degraded (ADR 0024). It is not replicated.
type DegradedAlert struct {
	ID        int64
	TenantID  string
	MonitorID string
	ChannelID string
	Status    string
	SinceMs   int64
	SentMs    int64
	Outcome   string
	Reachable int
	Total     int
	Reporters []string
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
	// GetTenantByOrg returns the tenant bound to the identity-provider organization,
	// or ErrNotFound. An empty orgID is always ErrNotFound (ADR 0027).
	GetTenantByOrg(ctx context.Context, orgID string) (Tenant, error)
	ListTenants(ctx context.Context) ([]Tenant, error)
	// TenantUsage counts the tenant's monitors and channels (ADR 0029).
	TenantUsage(ctx context.Context, tenantID string) (TenantUsage, error)
	// GetPlan returns a plan or ErrNotFound.
	GetPlan(ctx context.Context, id string) (Plan, error)
	// ListPlans returns all plans ordered by ID.
	ListPlans(ctx context.Context) ([]Plan, error)
	// ListDeletedTenants returns the tombstones ordered by ID (ADR 0029).
	ListDeletedTenants(ctx context.Context) ([]DeletedTenant, error)
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
	// ListUnrevokedSessionUsers returns the users that hold a refresh token
	// that is not revoked, ordered by tenant and user (ADR 0032).
	ListUnrevokedSessionUsers(ctx context.Context) ([]SessionUser, error)

	// ListOpenUnconfirmedAlerts returns the alerts without verdict ordered by
	// since time, node and local ID (ADR 0024).
	ListOpenUnconfirmedAlerts(ctx context.Context, limit int) ([]UnconfirmedAlert, error)

	// StatusHistory returns the status changes in [fromMs, toMs), preceded by
	// the last change before fromMs if it is still stored, ordered by time.
	StatusHistory(ctx context.Context, tenantID, monitorID string, fromMs, toMs int64) ([]StatusChange, error)
	// StatusHistoryBatch returns StatusHistory for each listed monitor of the
	// tenant, in one query. Monitors without changes are absent from the map.
	StatusHistoryBatch(ctx context.Context, tenantID string, monitorIDs []string, fromMs, toMs int64) (map[string][]StatusChange, error)

	// ListSecretKeys returns all data encryption keys ordered by creation (ADR 0011).
	ListSecretKeys(ctx context.Context) ([]SecretKey, error)

	// ListClusterCAs returns the cluster CAs ordered by creation (ADR 0006).
	ListClusterCAs(ctx context.Context) ([]ClusterCA, error)
	// GetJoinToken returns the join token with the ID or ErrNotFound.
	GetJoinToken(ctx context.Context, id string) (JoinToken, error)
	// ListJoinTokens returns all join tokens ordered by creation and ID.
	ListJoinTokens(ctx context.Context) ([]JoinToken, error)
	// ListRevokedCerts returns the revoked certificates ordered by serial.
	ListRevokedCerts(ctx context.Context) ([]RevokedCert, error)
	// ListRevokedNodes returns the revoked nodes ordered by node ID.
	ListRevokedNodes(ctx context.Context) ([]RevokedNode, error)
	// ListNodes returns the node registry ordered by node ID (ADR 0017).
	ListNodes(ctx context.Context) ([]Node, error)
	// ListSystemAlerts returns the system alerts ordered by kind and subject;
	// with activeOnly only the active ones (ADR 0025).
	ListSystemAlerts(ctx context.Context, activeOnly bool) ([]SystemAlert, error)
	// ListProbeAlerts returns the private probe alerts of tenantID ordered by
	// probe ID; with activeOnly only the active ones (ADR 0035).
	ListProbeAlerts(ctx context.Context, tenantID string, activeOnly bool) ([]ProbeAlert, error)
	// ListAllProbeAlerts returns all private probe alerts ordered by tenant ID
	// and probe ID; with activeOnly only the active ones (ADR 0035).
	ListAllProbeAlerts(ctx context.Context, activeOnly bool) ([]ProbeAlert, error)
	// CountTenantPrivateProbes returns the number of registered private probes
	// owned by tenantID (ADR 0035).
	CountTenantPrivateProbes(ctx context.Context, tenantID string) (int, error)
	// CountOpenTenantJoinTokens returns the number of unused, unexpired probe
	// join tokens bound to tenantID at nowMs (ADR 0035).
	CountOpenTenantJoinTokens(ctx context.Context, tenantID string, nowMs int64) (int, error)
	// ListNodeIncarnations returns the last known incarnations ordered by node ID (ADR 0025).
	ListNodeIncarnations(ctx context.Context) ([]NodeIncarnation, error)

	// GetAPITokenBySecretHash returns the API token with the hash or
	// ErrNotFound (ADR 0031).
	GetAPITokenBySecretHash(ctx context.Context, secretHash string) (APIToken, error)
	// ListAPITokens returns the API tokens of the tenant, newest first.
	ListAPITokens(ctx context.Context, tenantID string) ([]APIToken, error)
	// ListAudit returns a page of the audit log (ADR 0031).
	ListAudit(ctx context.Context, q AuditQuery) (AuditPage, error)
	// AppliedIndex returns the last Raft log index applied to the state (0 if none).
	AppliedIndex(ctx context.Context) (uint64, error)
	// CommandLevel returns the command version the cluster may use (ADR 0018),
	// 0 if it was never set.
	CommandLevel(ctx context.Context) (uint32, error)
	// LastIssuedAt returns the command time of the last applied command, 0 if
	// none was applied (ADR 0024).
	LastIssuedAt(ctx context.Context) (int64, error)

	// Update runs fn in one write transaction. Only the Raft FSM calls it, so
	// that every replica applies the same changes (ADR 0005, FA-CLU-07).
	Update(ctx context.Context, fn func(tx StateTx) error) error
}

// StateTx writes data class A inside State.Update. Implementations must not read
// the clock or a random source: timestamps and IDs come with the records.
type StateTx interface {
	// ListNodes returns the node registry ordered by node ID (ADR 0017).
	ListNodes(ctx context.Context) ([]Node, error)
	PutTenant(ctx context.Context, t Tenant) error
	GetTenant(ctx context.Context, id string) (Tenant, error)
	// GetTenantByOrg returns the tenant bound to the organization, or ErrNotFound.
	GetTenantByOrg(ctx context.Context, orgID string) (Tenant, error)
	// CountTenants returns the number of tenants created with the source.
	CountTenants(ctx context.Context, source string) (int, error)
	// CountTenantsWithPlan returns the number of tenants on the plan.
	CountTenantsWithPlan(ctx context.Context, planID string) (int, error)
	// TenantUsage counts the tenant's monitors and channels (ADR 0029).
	TenantUsage(ctx context.Context, tenantID string) (TenantUsage, error)
	// IsTenantDeleted reports whether a tombstone exists for the ID.
	IsTenantDeleted(ctx context.Context, id string) (bool, error)
	// DeleteTenant removes the tenant and every row of it in every table and
	// records a tombstone with deletedMs, in the caller's transaction (ADR 0029).
	// An unknown tenant is ErrNotFound.
	DeleteTenant(ctx context.Context, id string, deletedMs int64) error
	// GetPlan returns a plan or ErrNotFound.
	GetPlan(ctx context.Context, id string) (Plan, error)
	// PutPlan creates or replaces a plan; created_ms of an existing plan is kept.
	PutPlan(ctx context.Context, p Plan) error
	// DeletePlan deletes a plan or returns ErrNotFound.
	DeletePlan(ctx context.Context, id string) error
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
	// PutMonitorState stores the status, confirmation and since time and records
	// the change in the status history. The flapping fields are not changed.
	PutMonitorState(ctx context.Context, tenantID, monitorID string, st MonitorState) error
	// SetMonitorFlapping stores the flapping flag and its start time of a monitor
	// with a state, or ErrNotFound.
	SetMonitorFlapping(ctx context.Context, tenantID, monitorID string, flapping bool, sinceMs int64) error
	// StatusChangesSince returns the status history of a monitor from fromMs on, ordered by time.
	StatusChangesSince(ctx context.Context, tenantID, monitorID string, fromMs int64) ([]StatusChange, error)
	// ListChannels returns the tenant's channels ordered by name.
	ListChannels(ctx context.Context, tenantID string) ([]NotificationChannel, error)
	// PruneStatusHistory deletes a monitor's changes before beforeMs except
	// the last one, which still defines the status at beforeMs.
	PruneStatusHistory(ctx context.Context, tenantID, monitorID string, beforeMs int64) error
	// OpenIncident stores a new incident; a second open incident of the same
	// monitor is ErrConflict. A non-zero ResolvedMs stores a closed incident,
	// which is how re-evaluated gaps are recorded (ADR 0024).
	OpenIncident(ctx context.Context, inc Incident) error
	// ResolveIncident ends the open incident of a monitor, if there is one.
	ResolveIncident(ctx context.Context, tenantID, monitorID string, resolvedMs int64) error
	// ReplaceStatusHistory deletes the changes of a monitor in [fromMs, toMs)
	// and stores entries instead (FA-UPT-07, ADR 0024). An entry outside the
	// interval is an error.
	ReplaceStatusHistory(ctx context.Context, tenantID, monitorID string, fromMs, toMs int64, entries []StatusChange) error

	// InsertUnconfirmedAlerts stores reported alerts. An alert whose node and
	// local ID are already stored is left unchanged (ADR 0024).
	InsertUnconfirmedAlerts(ctx context.Context, alerts []UnconfirmedAlert) error
	// GetUnconfirmedAlert returns a stored alert or ErrNotFound.
	GetUnconfirmedAlert(ctx context.Context, nodeID string, localID int64) (UnconfirmedAlert, error)
	// ResolveUnconfirmedAlert stores the verdict of an open alert. An unknown
	// alert is ErrNotFound, an alert that already has a verdict is ErrConflict.
	// verdict is one of the Verdict constants.
	ResolveUnconfirmedAlert(ctx context.Context, nodeID string, localID int64, verdict string, resolvedMs int64) error
	// SetLastIssuedAt stores the command time of the last applied command (ADR 0024).
	SetLastIssuedAt(ctx context.Context, ms int64) error
	// LastIssuedAt returns the stored command time, 0 if none (ADR 0024).
	LastIssuedAt(ctx context.Context) (int64, error)

	// PutSecretKey creates a DEK or updates its wrapping or retirement; the
	// creation time of an existing key is kept.
	PutSecretKey(ctx context.Context, k SecretKey) error

	// ListClusterCAs returns the cluster CAs ordered by creation.
	ListClusterCAs(ctx context.Context) ([]ClusterCA, error)
	// GetJoinToken returns the join token with the ID or ErrNotFound.
	GetJoinToken(ctx context.Context, id string) (JoinToken, error)
	// PutClusterCA stores a CA. A CA with the same fingerprint is ErrConflict.
	PutClusterCA(ctx context.Context, ca ClusterCA) error
	// InsertJoinToken stores a join token; a duplicate ID is ErrConflict.
	InsertJoinToken(ctx context.Context, t JoinToken) error
	// MarkJoinTokenUsed records the first redemption of a token. A token that
	// is already used is ErrConflict, an unknown one ErrNotFound.
	MarkJoinTokenUsed(ctx context.Context, id string, usedMs int64, usedBy string) error
	// DeleteJoinToken deletes the token or returns ErrNotFound.
	DeleteJoinToken(ctx context.Context, id string) error
	// PruneJoinTokens deletes the tokens that expired before beforeMs.
	PruneJoinTokens(ctx context.Context, beforeMs int64) error
	// PutRevokedCert records a revoked certificate; a repeated serial changes nothing.
	PutRevokedCert(ctx context.Context, c RevokedCert) error
	// PruneRevokedCerts deletes the entries whose certificate expired before beforeMs.
	PruneRevokedCerts(ctx context.Context, beforeMs int64) error
	// PutRevokedNode records a revoked node. A repeated revocation keeps the
	// later RevokedMs.
	PutRevokedNode(ctx context.Context, n RevokedNode) error
	// GetRevokedNode returns the revocation of a node or ErrNotFound.
	GetRevokedNode(ctx context.Context, nodeID string) (RevokedNode, error)
	// GetNode returns a registered node or ErrNotFound.
	GetNode(ctx context.Context, id string) (Node, error)
	// PutNode creates or replaces a registry entry (ADR 0017).
	PutNode(ctx context.Context, n Node) error
	// DeleteNode removes a registry entry; an unknown ID is not an error.
	DeleteNode(ctx context.Context, id string) error
	// SetNodeConnectivity records the connectivity of a registered node and
	// the time of the change (ADR 0022). An unknown node is ErrNotFound.
	SetNodeConnectivity(ctx context.Context, nodeID string, impaired bool, reason string, atMs int64) error

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
	// RevokeTenantSessions revokes the refresh sessions of all users of the
	// tenant at ms (ADR 0029, suspending a tenant).
	RevokeTenantSessions(ctx context.Context, tenantID string, ms int64) error
	// PruneSessions deletes the tokens that expired before beforeMs.
	PruneSessions(ctx context.Context, beforeMs int64) error

	// InsertAPIToken stores a new API token. A duplicate ID, secret hash or
	// name within the tenant is ErrConflict (ADR 0031).
	InsertAPIToken(ctx context.Context, t APIToken) error
	// GetAPIToken returns the API token of the tenant or ErrNotFound.
	GetAPIToken(ctx context.Context, tenantID, id string) (APIToken, error)
	// CountAPITokens returns the number of API tokens of the tenant.
	CountAPITokens(ctx context.Context, tenantID string) (int, error)
	// DeleteAPIToken removes the API token; ErrNotFound if it does not exist.
	DeleteAPIToken(ctx context.Context, tenantID, id string) error
	// PruneAPITokens deletes the API tokens that expired before beforeMs.
	PruneAPITokens(ctx context.Context, beforeMs int64) error
	// InsertAudit appends audit entries. (Index, Sub) is unique.
	InsertAudit(ctx context.Context, entries []AuditEntry) error
	// PruneAudit deletes the audit entries with AtMs before beforeMs.
	PruneAudit(ctx context.Context, beforeMs int64) error

	// SetAppliedIndex records the Raft index applied by this transaction (ADR 0005).
	SetAppliedIndex(ctx context.Context, index uint64) error
	// CommandLevel returns the stored command level (ADR 0018), 0 if unset.
	CommandLevel(ctx context.Context) (uint32, error)
	// SetCommandLevel stores the command level. The caller only ever raises it.
	SetCommandLevel(ctx context.Context, level uint32) error
	// GetDedup returns the stored outcome of a request ID or ErrNotFound.
	GetDedup(ctx context.Context, requestID string) (DedupEntry, error)
	PutDedup(ctx context.Context, e DedupEntry) error
	// PruneDedup removes entries issued before beforeMs (deterministic retention).
	PruneDedup(ctx context.Context, beforeMs int64) error
	// GetSystemAlert returns the stored system alert or ErrNotFound (ADR 0025).
	GetSystemAlert(ctx context.Context, kind, subject string) (SystemAlert, error)
	// PutSystemAlert creates or replaces the system alert with its kind and subject.
	PutSystemAlert(ctx context.Context, a SystemAlert) error
	// GetProbeAlert returns the stored private probe alert or ErrNotFound (ADR 0035).
	GetProbeAlert(ctx context.Context, tenantID, probeID string) (ProbeAlert, error)
	// PutProbeAlert creates or replaces the private probe alert (ADR 0035).
	PutProbeAlert(ctx context.Context, a ProbeAlert) error
	// DeleteProbeAlertsByProbe deletes any probe alert for probeID (ADR 0035).
	DeleteProbeAlertsByProbe(ctx context.Context, probeID string) error
	// CountTenantPrivateProbes returns the number of registered private probes
	// owned by tenantID (ADR 0035).
	CountTenantPrivateProbes(ctx context.Context, tenantID string) (int, error)
	// CountOpenTenantJoinTokens returns the number of unused, unexpired probe
	// join tokens bound to tenantID at nowMs (ADR 0035).
	CountOpenTenantJoinTokens(ctx context.Context, tenantID string, nowMs int64) (int, error)
	// PutNodeIncarnation creates or replaces the last known incarnation of a node (ADR 0025).
	PutNodeIncarnation(ctx context.Context, n NodeIncarnation) error
	// Fingerprint hashes the replicated state inside this transaction, as
	// State.Fingerprint does for the committed state (ADR 0025).
	Fingerprint(ctx context.Context) (string, error)
}

// ClusterCA is the certificate authority of the cluster (ADR 0006). KeyEnc is
// the PKCS#8 private key encrypted with a data key (ADR 0011).
type ClusterCA struct {
	Fingerprint string
	ClusterID   string
	CertDER     []byte
	KeyEnc      string
	CreatedMs   int64
}

// JoinToken is a one-time credential that lets a node join (FA-PKI-02). Only
// the hash of its secret is stored. UsedMs is 0 until the token is redeemed.
type JoinToken struct {
	ID         string
	SecretHash string
	Role       string
	// TenantID is only set for probe tokens of a private tenant.
	TenantID  string
	ExpiresMs int64
	CreatedMs int64
	UsedMs    int64
	UsedBy    string
}

// RevokedCert is a node certificate that must no longer be accepted (FA-PKI-04).
type RevokedCert struct {
	Serial     string
	NodeID     string
	RevokedMs  int64
	NotAfterMs int64
}

// RevokedNode is a node whose certificates issued before RevokedMs must no
// longer be accepted (FA-PKI-04).
type RevokedNode struct {
	NodeID    string
	RevokedMs int64
}

// Node roles in the registry.
const (
	NodeRoleVoter = "voter"
	NodeRoleProbe = "probe"
)

// Node is an entry of the node registry in the Raft state (ADR 0017). Each
// node registers itself; Healthy is set by the leader from its contact with
// the node.
type Node struct {
	ID   string
	Name string
	Role string // NodeRoleVoter or NodeRoleProbe
	// OwnerTenantID is empty for voters and public probes, and the tenant ID for
	// private probes (ADR 0034, ADR 0035).
	OwnerTenantID string
	// Incarnation is the metrics.db incarnation last reported by a probe.
	Incarnation  string
	Capabilities []string // sorted, e.g. "icmp" (FA-CHK-31)
	Labels       map[string]string
	Region       string
	Version      string
	// MaxCommandVersion is the highest command version the node can apply.
	MaxCommandVersion uint32
	Healthy           bool
	// HealthChangedMs is the command time of the last health change, or of
	// the first registration.
	HealthChangedMs int64
	// ConnectivityImpaired is the connectivity state the leader decided (ADR 0022).
	ConnectivityImpaired bool
	// ConnectivityReason is "reference" or "deviation" while impaired, else empty.
	ConnectivityReason string
	// ConnectivityChangedMs is the command time of the last connectivity change.
	ConnectivityChangedMs int64
	RegisteredMs          int64
	UpdatedMs             int64
}

// HasCapability reports whether the node has the capability; "" is always true.
func (n Node) HasCapability(c string) bool {
	return c == "" || slices.Contains(n.Capabilities, c)
}

// SecretKey is a data encryption key wrapped with the master key (ADR 0011).
type SecretKey struct {
	KeyID          string
	WrappedDEK     []byte
	KEKFingerprint string
	CreatedMs      int64
	RetiredMs      int64 // 0 = not retired
}

// SystemAlert is the replicated state of one system alert (FA-OBS-01, ADR 0025),
// identified by its kind and subject. Level is the expiry stage or a counter.
type SystemAlert struct {
	Kind        string
	Subject     string
	Active      bool
	Level       int
	SinceMs     int64
	UpdatedMs   int64
	DetailsJSON json.RawMessage
}

// ProbeAlert is the replicated offline alert state of one private probe
// (FA-PRB-06, ADR 0035), scoped to its owning tenant.
type ProbeAlert struct {
	TenantID  string
	ProbeID   string
	Active    bool
	SinceMs   int64
	UpdatedMs int64
}

// NodeIncarnation is the last incarnation a node reported (FA-REP-06, ADR 0025).
type NodeIncarnation struct {
	NodeID      string
	Incarnation string
	SeenMs      int64
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

// StreamPosition is the replication watermark of one heartbeat stream
// (ADR 0012 B2): every heartbeat of the stream with Seq <= this value that
// still exists anywhere is stored here. Gaps below it are heartbeats that were
// compacted, expired or never stored by the producer, never missing ones.
type StreamPosition struct {
	NodeID      string
	Incarnation string
	Seq         uint64
}

// Metrics stores data class B.
type Metrics interface {
	// PurgeTenant deletes up to batchSize rows of the tenant from each
	// metrics table and returns how many rows it deleted in total; 0 means
	// nothing of the tenant is left (ADR 0029).
	PurgeTenant(ctx context.Context, tenantID string, batchSize int) (int64, error)
	// PurgedTenants returns the IDs of the tenants whose purge finished on
	// this node, ordered by ID.
	PurgedTenants(ctx context.Context) ([]string, error)
	// MarkTenantPurged records that the purge of the tenant finished on this node.
	MarkTenantPurged(ctx context.Context, tenantID string, purgedMs int64) error
	// Incarnation identifies this metrics database; it changes whenever the
	// database is recreated (ADR 0012 B4).
	Incarnation() string
	// InsertHeartbeats stores heartbeats idempotently and returns how many were
	// new. In the same transaction it advances the position of each stream it
	// touches while the next seq is present without a gap (FA-REP-03).
	InsertHeartbeats(ctx context.Context, hbs []Heartbeat) (int, error)
	// LastSeq returns the highest stored seq of a stream (0 if none).
	LastSeq(ctx context.Context, nodeID, incarnation string) (uint64, error)

	// StreamPositions returns the position of every stream known locally.
	StreamPositions(ctx context.Context) ([]StreamPosition, error)
	// AdvanceStreamPosition raises a stream's position to seq; a lower seq is
	// ignored. Anti-entropy calls it after storing a page fetched from a peer
	// whose position covers it.
	AdvanceStreamPosition(ctx context.Context, pos StreamPosition) error
	// HeartbeatsAfter returns up to limit heartbeats of one stream with
	// afterSeq < seq <= untilSeq in seq order, and whether more follow.
	HeartbeatsAfter(ctx context.Context, nodeID, incarnation string, afterSeq, untilSeq uint64, limit int) ([]Heartbeat, bool, error)
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

	// InsertDegradedAlert appends an alert to the node-local degraded log and
	// returns its ID (ADR 0024). a.ID is ignored.
	InsertDegradedAlert(ctx context.Context, a DegradedAlert) (int64, error)
	// ListUnreportedDegradedAlerts returns up to limit alerts not yet reported
	// to the leader, ordered by ID.
	ListUnreportedDegradedAlerts(ctx context.Context, limit int) ([]DegradedAlert, error)
	// MarkDegradedAlertsReported records that the alerts reached the leader.
	MarkDegradedAlertsReported(ctx context.Context, ids []int64) error
	// PruneDegradedAlerts deletes the alerts sent before beforeMs and returns the number deleted.
	PruneDegradedAlerts(ctx context.Context, beforeMs int64) (int64, error)
}
