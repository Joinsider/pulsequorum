// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package cluster contains the Raft consensus layer: the FSM that materializes
// data class A into state.db and the node that runs Raft (ADR 0005, 0012).
package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
)

// CommandVersion is the highest Command.version this binary can apply.
// It is raised together with a feature gate (FA-CLU-12).
const CommandVersion = 1

// Stable codes of deterministic command errors. They are stored in the dedup
// table, so a retry returns the same error.
const (
	CodeConflict    = "conflict"
	CodeNotFound    = "not_found"
	CodeInvalid     = "invalid"
	CodeUnsupported = "unsupported"
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

// NewPutMonitor builds a command that creates (expectedRevision 0) or replaces a monitor.
func NewPutMonitor(requestID, tenantID, monitorID string, doc MonitorDocument, expectedRevision uint64) (*pb.Command, error) {
	return newPut(requestID, tenantID, pb.EntityKind_ENTITY_KIND_MONITOR, monitorID, doc, expectedRevision)
}

// NewPutSecretKey builds a platform-level command that stores a data key.
func NewPutSecretKey(requestID, keyID string, doc SecretKeyDocument) (*pb.Command, error) {
	return newPut(requestID, "", pb.EntityKind_ENTITY_KIND_SECRET_KEY, keyID, doc, 0)
}

// NewDeleteMonitor builds a command that deletes a monitor.
func NewDeleteMonitor(requestID, tenantID, monitorID string) *pb.Command {
	return &pb.Command{
		Version:   CommandVersion,
		RequestId: requestID,
		TenantId:  tenantID,
		Payload: &pb.Command_DeleteEntity{DeleteEntity: &pb.DeleteEntity{
			Kind: pb.EntityKind_ENTITY_KIND_MONITOR,
			Id:   monitorID,
		}},
	}
}

func newPut(requestID, tenantID string, kind pb.EntityKind, id string, doc any, expectedRevision uint64) (*pb.Command, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode %s document: %w", kind, err)
	}
	return &pb.Command{
		Version:   CommandVersion,
		RequestId: requestID,
		TenantId:  tenantID,
		Payload: &pb.Command_PutEntity{PutEntity: &pb.PutEntity{
			Kind:             kind,
			Id:               id,
			DocumentJson:     raw,
			ExpectedRevision: expectedRevision,
		}},
	}, nil
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
	return &pb.Command{
		Version:   CommandVersion,
		RequestId: fmt.Sprintf("state:%s:%s:%s:%d", c.MonitorID, c.From, c.To, c.SinceMs),
		TenantId:  c.TenantID,
		Actor:     &pb.Actor{Kind: pb.Actor_KIND_SYSTEM},
		Payload:   &pb.Command_SetMonitorState{SetMonitorState: set},
	}
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
