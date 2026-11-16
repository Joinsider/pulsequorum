// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package cluster contains the Raft consensus layer: the FSM that materializes
// data class A into state.db and the node that runs Raft (ADR 0005, 0012).
package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/google/uuid"

	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
)

// CommandVersion is the highest Command.version this binary can apply.
// It is raised together with a feature gate (FA-CLU-12). Version 2 adds
// RegisterNode and SetNodeHealth (ADR 0017), version 3 SetNodeConnectivity (ADR 0022),
// version 4 ClearMonitorFlapping and the flapping and channel-failed alerts (ADR 0023).
const CommandVersion = 4

// BaseCommandLevel is the command level of a cluster that never stored one:
// the first release with the feature gate (ADR 0018). Commands up to this
// version are always allowed.
const BaseCommandLevel = 2

// NewSetNodeConnectivity builds the command that records a connectivity change
// of a node (ADR 0022). Only the leader proposes it, and only on a change.
//
// The request ID is random per command, not derived from the change: the FSM
// deduplicates request IDs for 24 hours, so a repeated flip of the same node
// would otherwise return the stored result without being applied. A retry of
// the same command keeps its request ID, because the caller reuses the command.
func NewSetNodeConnectivity(nodeID string, impaired bool, reason string) *pb.Command {
	return stamp(&pb.Command{
		RequestId: uuid.NewString(),
		Payload: &pb.Command_SetNodeConnectivity{SetNodeConnectivity: &pb.SetNodeConnectivity{
			NodeId: nodeID, Impaired: impaired, Reason: reason,
		}},
	})
}

// NewClearMonitorFlapping builds the command that ends the flapping period
// started at expectedSinceMs (ADR 0023). The request ID is derived from the
// period, so a retry of the same period is deduplicated, and a later period
// gets its own ID.
func NewClearMonitorFlapping(tenantID, monitorID string, expectedSinceMs int64) *pb.Command {
	return stamp(&pb.Command{
		RequestId: fmt.Sprintf("flapping-clear:%s:%d", monitorID, expectedSinceMs),
		TenantId:  tenantID,
		Actor:     &pb.Actor{Kind: pb.Actor_KIND_SYSTEM},
		Payload: &pb.Command_ClearMonitorFlapping{ClearMonitorFlapping: &pb.ClearMonitorFlapping{
			MonitorId: monitorID, ExpectedSinceMs: expectedSinceMs,
		}},
	})
}

// EffectiveCommandLevel returns the command level the cluster may use, given
// the level stored in state.db (0 if unset).
func EffectiveCommandLevel(stored uint32) uint32 {
	return max(stored, BaseCommandLevel)
}

// Stable codes of deterministic command errors. They are stored in the dedup
// table, so a retry returns the same error.
const (
	CodeConflict    = "conflict"
	CodeNotFound    = "not_found"
	CodeInvalid     = "invalid"
	CodeUnsupported = "unsupported"
	// CodeNotEnabled rejects a command before it is proposed because not all
	// voters support its version yet (ADR 0018). It is never stored.
	CodeNotEnabled = "not_enabled"
)

// ErrInvalidCommand is matched by CommandErrors with the codes invalid and unsupported.
var ErrInvalidCommand = errors.New("invalid command")

// CommandError is a deterministic rejection of a command: every replica rejects
// it the same way, so it is a result, not a failure of the node.
type CommandError struct {
	Code    string
	Message string
}

func (e *CommandError) Error() string { return e.Code + ": " + e.Message }

// Is lets callers use errors.Is with the store and cluster sentinel errors.
func (e *CommandError) Is(target error) bool {
	switch e.Code {
	case CodeConflict:
		return target == store.ErrConflict
	case CodeNotFound:
		return target == store.ErrNotFound
	case CodeInvalid, CodeUnsupported:
		return target == ErrInvalidCommand
	}
	return false
}

func invalid(format string, args ...any) *CommandError {
	return &CommandError{Code: CodeInvalid, Message: fmt.Sprintf(format, args...)}
}

// MonitorDocument is the JSON document of a monitor inside PutEntity. It is
// part of the Raft log, so fields may only be added, never renamed (NFA-MNT-04).
type MonitorDocument struct {
	Name                  string           `json:"name"`
	Description           string           `json:"description,omitempty"`
	Type                  string           `json:"type"`
	Config                json.RawMessage  `json:"config,omitempty"`
	IntervalSeconds       int              `json:"intervalSeconds"`
	TimeoutSeconds        int              `json:"timeoutSeconds"`
	Retries               int              `json:"retries"`
	RecoveryThreshold     int              `json:"recoveryThreshold"`
	ResendIntervalMinutes int              `json:"resendIntervalMinutes"`
	Assignment            store.Assignment `json:"assignment"`
	ConsensusPolicy       string           `json:"consensusPolicy"`
	Tags                  []string         `json:"tags,omitempty"`
	Paused                bool             `json:"paused,omitempty"`
	// NotificationChannelIDs must name channels of the same tenant (FA-ALR-01).
	NotificationChannelIDs []string `json:"notificationChannelIds,omitempty"`
	// Secrets are ciphertexts by JSON pointer into Config (ADR 0015); the API
	// encrypts them before proposing, the FSM rejects plaintext.
	Secrets map[string]string `json:"secrets,omitempty"`
}

// ChannelDocument is the JSON document of a notification channel. The leader
// encrypts URLEnc and the header values before proposing (ADR 0011, 0014);
// the FSM rejects plaintext.
type ChannelDocument struct {
	Name            string            `json:"name"`
	Kind            string            `json:"kind"`
	URLEnc          string            `json:"urlEnc"`
	WebhookMethod   string            `json:"webhookMethod,omitempty"`
	WebhookHeaders  map[string]string `json:"webhookHeaders,omitempty"`
	WebhookTemplate string            `json:"webhookTemplate,omitempty"`
	Events          []string          `json:"events"`
	IsDefault       bool              `json:"isDefault,omitempty"`
}

// AlertPayload is the content of an alert, built by the FSM from replicated
// data and stored with every outbox entry (ADR 0014).
type AlertPayload struct {
	// AlertID is stable across retries, so receivers can deduplicate.
	AlertID        string `json:"alertId"`
	TenantID       string `json:"tenantId"`
	MonitorID      string `json:"monitorId"`
	MonitorName    string `json:"monitorName"`
	MonitorType    string `json:"monitorType"`
	Event          string `json:"event"`
	Status         string `json:"status"`
	PreviousStatus string `json:"previousStatus"`
	SinceMs        int64  `json:"sinceMs"`
	Cause          string `json:"cause,omitempty"`
	ErrorClass     string `json:"errorClass,omitempty"`
	IncidentID     string `json:"incidentId,omitempty"`
	// Reminder counts repeated DOWN alerts; 0 for the change itself.
	Reminder  int  `json:"reminder,omitempty"`
	Confirmed bool `json:"confirmed"`
	// Flapping is true when a flapping period starts and false at its end;
	// both use Event "flapping". ChangeCount is the number of status changes
	// in the flapping window (start) or during the period (end) (ADR 0023).
	Flapping    bool `json:"flapping,omitempty"`
	ChangeCount int  `json:"changeCount,omitempty"`
	// ChannelID, ChannelName and ChannelError describe the failed channel of
	// an Event "channel_failed" (FA-ALR-05). ChannelError is masked like the
	// channel's last_error and holds no secrets.
	ChannelID    string `json:"channelId,omitempty"`
	ChannelName  string `json:"channelName,omitempty"`
	ChannelError string `json:"channelError,omitempty"`
}

// TenantDocument is the JSON document of a tenant inside PutEntity.
type TenantDocument struct {
	Name         string          `json:"name"`
	Plan         string          `json:"plan"`
	ZitadelOrgID string          `json:"zitadelOrgId,omitempty"`
	Limits       json.RawMessage `json:"limits,omitempty"`
	Suspended    bool            `json:"suspended,omitempty"`
}

// SecretKeyDocument is a data encryption key wrapped with the master key
// (ADR 0011). The leader generates and wraps it; the FSM only stores it.
type SecretKeyDocument struct {
	WrappedDEK     []byte `json:"wrappedDek"`
	KEKFingerprint string `json:"kekFingerprint"`
	RetiredMs      int64  `json:"retiredMs,omitempty"`
}

// PutResult is the result of a PutEntity command.
type PutResult struct {
	ID       string `json:"id"`
	Revision uint64 `json:"revision,omitempty"`
}

// NewPutTenant builds a platform-level command that creates or updates a tenant.
func NewPutTenant(requestID, tenantID string, doc TenantDocument) (*pb.Command, error) {
	return newPut(requestID, "", pb.EntityKind_ENTITY_KIND_TENANT, tenantID, doc, 0)
}

// MonitorDocumentFrom returns the document that recreates a stored monitor,
// e.g. to pause it or to rewrite its secrets. Secrets stay encrypted.
func MonitorDocumentFrom(m store.Monitor) MonitorDocument {
	return MonitorDocument{
		Name: m.Name, Description: m.Description, Type: m.Type, Config: m.Config,
		IntervalSeconds: m.IntervalSeconds, TimeoutSeconds: m.TimeoutSeconds, Retries: m.Retries,
		RecoveryThreshold: m.RecoveryThreshold, ResendIntervalMinutes: m.ResendIntervalMinutes,
		Assignment: m.Assignment, ConsensusPolicy: m.ConsensusPolicy, Tags: m.Tags, Paused: m.Paused,
		NotificationChannelIDs: slices.Clone(m.ChannelIDs), Secrets: maps.Clone(m.Secrets),
	}
}

// NewPutMonitor builds a command that creates (expectedRevision 0) or replaces a monitor.
func NewPutMonitor(requestID, tenantID, monitorID string, doc MonitorDocument, expectedRevision uint64) (*pb.Command, error) {
	return newPut(requestID, tenantID, pb.EntityKind_ENTITY_KIND_MONITOR, monitorID, doc, expectedRevision)
}

// MaintenanceDocument is the JSON document of a maintenance window inside
// PutEntity (FA-MNT-01). Monitor IDs are normalized by the store; the FSM checks
// only the structure of the schedule, time zones and cron expressions are
// checked by the API before the command is proposed.
type MaintenanceDocument struct {
	Name        string                    `json:"name"`
	Description string                    `json:"description,omitempty"`
	MonitorIDs  []string                  `json:"monitorIds,omitempty"`
	Schedule    store.MaintenanceSchedule `json:"schedule"`
	Active      bool                      `json:"active"`
}

// NewPutSecretKey builds a platform-level command that stores a data key.
func NewPutSecretKey(requestID, keyID string, doc SecretKeyDocument) (*pb.Command, error) {
	return newPut(requestID, "", pb.EntityKind_ENTITY_KIND_SECRET_KEY, keyID, doc, 0)
}

// NewPutChannel builds a command that creates (expectedRevision 0) or replaces a channel.
func NewPutChannel(requestID, tenantID, channelID string, doc ChannelDocument, expectedRevision uint64) (*pb.Command, error) {
	return newPut(requestID, tenantID, pb.EntityKind_ENTITY_KIND_NOTIFICATION_CHANNEL, channelID, doc, expectedRevision)
}

// NewDeleteChannel builds a command that deletes a channel.
func NewDeleteChannel(requestID, tenantID, channelID string) *pb.Command {
	return stamp(&pb.Command{
		RequestId: requestID, TenantId: tenantID,
		Payload: &pb.Command_DeleteEntity{DeleteEntity: &pb.DeleteEntity{Kind: pb.EntityKind_ENTITY_KIND_NOTIFICATION_CHANNEL, Id: channelID}},
	})
}

// NewPutMaintenanceWindow builds a command that creates (expectedRevision 0) or replaces a maintenance window.
func NewPutMaintenanceWindow(requestID, tenantID, id string, doc MaintenanceDocument, expectedRevision uint64) (*pb.Command, error) {
	return newPut(requestID, tenantID, pb.EntityKind_ENTITY_KIND_MAINTENANCE_WINDOW, id, doc, expectedRevision)
}

// NewDeleteMaintenanceWindow builds a command that deletes a maintenance window.
func NewDeleteMaintenanceWindow(requestID, tenantID, id string) *pb.Command {
	return stamp(&pb.Command{
		RequestId: requestID, TenantId: tenantID,
		Payload: &pb.Command_DeleteEntity{DeleteEntity: &pb.DeleteEntity{Kind: pb.EntityKind_ENTITY_KIND_MAINTENANCE_WINDOW, Id: id}},
	})
}

// NewEnqueueReminder builds the command for the n-th repeated DOWN alert.
func NewEnqueueReminder(tenantID, monitorID string, downSinceMs int64, n uint32) *pb.Command {
	return stamp(&pb.Command{
		RequestId: fmt.Sprintf("reminder:%s:%d:%d", monitorID, downSinceMs, n),
		TenantId:  tenantID,
		Actor:     &pb.Actor{Kind: pb.Actor_KIND_SYSTEM},
		Payload: &pb.Command_EnqueueAlert{EnqueueAlert: &pb.EnqueueAlert{
			MonitorId: monitorID, DownSinceMs: downSinceMs, Reminder: n,
		}},
	})
}

// DeliveryOutcome is the result of one delivery attempt, decided by the leader.
type DeliveryOutcome struct {
	Delivered bool
	Permanent bool
	GiveUp    bool
	// Error must already be cleaned of secrets.
	Error         string
	NextAttemptMs int64
}

// NewAckAlertDelivery builds the command that records a delivery attempt.
func NewAckAlertDelivery(a store.AlertDelivery, attempt uint32, o DeliveryOutcome) *pb.Command {
	return stamp(&pb.Command{
		RequestId: fmt.Sprintf("ack:%s:%s:%d", a.AlertKey, a.ChannelID, attempt),
		TenantId:  a.TenantID,
		Actor:     &pb.Actor{Kind: pb.Actor_KIND_SYSTEM},
		Payload: &pb.Command_AckAlertDelivery{AckAlertDelivery: &pb.AckAlertDelivery{
			AlertKey: a.AlertKey, ChannelId: a.ChannelID, Delivered: o.Delivered, PermanentFailure: o.Permanent,
			Error: o.Error, NextAttemptMs: o.NextAttemptMs, Attempt: attempt, GiveUp: o.GiveUp,
		}},
	})
}

// NewDeleteMonitor builds a command that deletes a monitor.
func NewDeleteMonitor(requestID, tenantID, monitorID string) *pb.Command {
	return stamp(&pb.Command{
		RequestId: requestID,
		TenantId:  tenantID,
		Payload: &pb.Command_DeleteEntity{DeleteEntity: &pb.DeleteEntity{
			Kind: pb.EntityKind_ENTITY_KIND_MONITOR,
			Id:   monitorID,
		}},
	})
}

func newPut(requestID, tenantID string, kind pb.EntityKind, id string, doc any, expectedRevision uint64) (*pb.Command, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode %s document: %w", kind, err)
	}
	return stamp(&pb.Command{
		RequestId: requestID,
		TenantId:  tenantID,
		Payload: &pb.Command_PutEntity{PutEntity: &pb.PutEntity{
			Kind:             kind,
			Id:               id,
			DocumentJson:     raw,
			ExpectedRevision: expectedRevision,
		}},
	}), nil
}

// StateChange is a consensus status change decided by the leader.
type StateChange struct {
	TenantID  string
	MonitorID string
	From      store.MonitorStatus
	To        store.MonitorStatus
	SinceMs   int64
	// Incident is required for a change to DOWN.
	Incident *store.Incident
}

// NewSetMonitorState builds the command for a status change. The request ID is
// derived from the change, so retrying an unknown outcome cannot apply it twice.
func NewSetMonitorState(c StateChange) *pb.Command {
	set := &pb.SetMonitorState{
		MonitorId:      c.MonitorID,
		Status:         string(c.To),
		Confirmed:      true,
		SinceMs:        c.SinceMs,
		ExpectedStatus: string(c.From),
	}
	if c.Incident != nil {
		set.Incident = &pb.IncidentStart{
			IncidentId:      c.Incident.ID,
			Cause:           c.Incident.Cause,
			ErrorClass:      ErrorClassToProto(c.Incident.ErrorClass),
			AffectedNodeIds: c.Incident.AffectedNodes,
		}
	}
	return stamp(&pb.Command{
		RequestId: fmt.Sprintf("state:%s:%s:%s:%d", c.MonitorID, c.From, c.To, c.SinceMs),
		TenantId:  c.TenantID,
		Actor:     &pb.Actor{Kind: pb.Actor_KIND_SYSTEM},
		Payload:   &pb.Command_SetMonitorState{SetMonitorState: set},
	})
}

// ErrorClassToProto maps a checker error class ("dns", "http_status", …) to
// the protobuf enum; unknown classes map to UNSPECIFIED.
func ErrorClassToProto(class string) pb.ErrorClass {
	return pb.ErrorClass(pb.ErrorClass_value["ERROR_CLASS_"+strings.ToUpper(class)])
}

// ErrorClassFromProto is the inverse of ErrorClassToProto; UNSPECIFIED maps to "".
func ErrorClassFromProto(c pb.ErrorClass) string {
	if c == pb.ErrorClass_ERROR_CLASS_UNSPECIFIED {
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(c.String(), "ERROR_CLASS_"))
}
