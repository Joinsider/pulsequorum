// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
)

// SystemAlertCommandLevel is the command level of system alerts, the incarnation
// record and the FSM checkpoint (FA-OBS-01, FA-REP-06, FA-CLU-07, ADR 0025).
// Below that level the commands are rejected and nothing is written, so
// mixed-version clusters do not diverge.
const SystemAlertCommandLevel = 6

// SystemAlertResult is the result of SetSystemAlert. Changed is false if the
// alert already had the active flag and level of the command (a no-op).
type SystemAlertResult struct {
	Changed bool `json:"changed"`
}

// SystemAlertInfo is the system part of an AlertPayload (ADR 0025).
type SystemAlertInfo struct {
	Kind    string          `json:"kind"`
	Subject string          `json:"subject"`
	Active  bool            `json:"active"`
	Level   int             `json:"level"`
	Details json.RawMessage `json:"details"`
}

// NewSetSystemAlert builds the command that records a change of a system alert
// (ADR 0025). Only the leader proposes it, and only on a change. The request
// ID is random per command: an alert may change back and forth within the
// dedup retention, and each change must still be applied.
func NewSetSystemAlert(kind, subject string, active bool, level int, details json.RawMessage) *pb.Command {
	return stamp(&pb.Command{
		RequestId: uuid.NewString(),
		Actor:     &pb.Actor{Kind: pb.Actor_KIND_SYSTEM},
		Payload: &pb.Command_SetSystemAlert{SetSystemAlert: &pb.SetSystemAlert{
			Kind:        kind,
			Subject:     subject,
			Active:      active,
			Level:       int32(level), //nolint:gosec // levels are small counters; the FSM rejects negative ones
			DetailsJson: details,
		}},
	})
}

// NewRecordIncarnation builds the command that stores the incarnation a node
// reported (FA-REP-06, ADR 0025). The request ID is random for the same reason
// as in NewSetSystemAlert: a node may return to an older incarnation.
func NewRecordIncarnation(nodeID, incarnation string) *pb.Command {
	return stamp(&pb.Command{
		RequestId: uuid.NewString(),
		Actor:     &pb.Actor{Kind: pb.Actor_KIND_SYSTEM},
		Payload: &pb.Command_RecordIncarnation{RecordIncarnation: &pb.RecordIncarnation{
			NodeId: nodeID, Incarnation: incarnation,
		}},
	})
}

// NewCheckpoint builds the command that asks every replica for the fingerprint
// of its state at this log index (ADR 0025). It changes no replicated data.
func NewCheckpoint() *pb.Command {
	return stamp(&pb.Command{
		RequestId: uuid.NewString(),
		Actor:     &pb.Actor{Kind: pb.Actor_KIND_SYSTEM},
		Payload:   &pb.Command_Checkpoint{Checkpoint: &pb.Checkpoint{}},
	})
}

// SystemTenantRequestID makes the creation of the system tenant idempotent: a
// retry or a second leader is deduplicated by the FSM.
const SystemTenantRequestID = "bootstrap:tenant:" + store.SystemTenantID

// NewPutSystemTenant builds the command that creates the system tenant (ADR
// 0025). The FSM accepts the reserved ID only from the system actor, so the
// command carries it.
func NewPutSystemTenant(requestID string) (*pb.Command, error) {
	cmd, err := NewPutTenant(requestID, store.SystemTenantID, TenantDocument{Name: "System", Plan: "system"})
	if err != nil {
		return nil, err
	}
	cmd.Actor = &pb.Actor{Kind: pb.Actor_KIND_SYSTEM}
	return cmd, nil
}

// currentCommandLevel returns the effective command level of the transaction.
func currentCommandLevel(ctx context.Context, tx store.StateTx) (uint32, error) {
	stored, err := tx.CommandLevel(ctx)
	if err != nil {
		return 0, fmt.Errorf("read command level: %w", err)
	}
	return EffectiveCommandLevel(stored), nil
}

// requireCommandLevel rejects a command below level with a deterministic error.
func requireCommandLevel(ctx context.Context, tx store.StateTx, what string, level uint32) error {
	current, err := currentCommandLevel(ctx, tx)
	if err != nil {
		return err
	}
	if current < level {
		return &CommandError{Code: CodeUnsupported, Message: fmt.Sprintf("%s needs command level %d", what, level)}
	}
	return nil
}

// systemDetails checks that raw is a JSON object (empty means {}) and returns it
// compacted, so that the stored bytes do not depend on the formatting of the sender.
func systemDetails(raw []byte) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage("{}"), nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, invalid("system alert details are not a JSON object")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, invalid("system alert details: %v", err)
	}
	return json.RawMessage(buf.Bytes()), nil
}

// systemAlertKey returns the alert key of a change of a system alert. The key
// contains the start of the active period, so that each period has its own
// key, and it ends with the kind of change.
func systemAlertKey(kind, subject string, since int64, active, prevActive, levelChanged bool, level int) string {
	suffix := "clear"
	if active {
		suffix = "active"
		if prevActive && levelChanged {
			suffix = fmt.Sprintf("L%d", level)
		}
	}
	return fmt.Sprintf("sys:%s:%s:%d:%s", kind, subject, since, suffix)
}

// applySetSystemAlert stores a change of a system alert and enqueues it for the
// channels of the system tenant that subscribe to "system" (FA-OBS-01, ADR 0025).
//
// A command is a no-op (Changed false) if the alert stays inactive, or stays
// active at the same level: an inactive alert has no level to change, and a
// repeated state or new details alone notify nobody. The start of the period
// (since_ms) is the command time when the alert becomes active or, while it is
// active, changes its level. A clear keeps the start and its level is not
// compared, so the clear matches the period it ends.
func applySetSystemAlert(ctx context.Context, tx store.StateTx, cmd *pb.Command, s *pb.SetSystemAlert) (json.RawMessage, error) {
	if err := requireCommandLevel(ctx, tx, "system alerts", SystemAlertCommandLevel); err != nil {
		return nil, err
	}
	kind, subject := s.GetKind(), s.GetSubject()
	switch {
	case kind == "" || subject == "":
		return nil, invalid("system alert without kind or subject")
	case s.GetLevel() < 0:
		return nil, invalid("system alert %s/%s has level %d", kind, subject, s.GetLevel())
	}
	details, err := systemDetails(s.GetDetailsJson())
	if err != nil {
		return nil, err
	}
	level, active := int(s.GetLevel()), s.GetActive()

	prev, err := tx.GetSystemAlert(ctx, kind, subject)
	exists := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	prevActive := exists && prev.Active
	levelChanged := active && prevActive && prev.Level != level
	if prevActive == active && (!active || !levelChanged) {
		return json.Marshal(SystemAlertResult{Changed: false})
	}

	since := cmd.GetIssuedAtMs()
	if !active && exists {
		since = prev.SinceMs
	}
	key := systemAlertKey(kind, subject, since, active, prevActive, levelChanged, level)

	if err := tx.PutSystemAlert(ctx, store.SystemAlert{
		Kind: kind, Subject: subject, Active: active, Level: level,
		SinceMs: since, UpdatedMs: cmd.GetIssuedAtMs(), DetailsJSON: details,
	}); err != nil {
		return nil, storeError(err)
	}

	payload, err := json.Marshal(AlertPayload{
		AlertID: key, TenantID: store.SystemTenantID, Event: store.EventSystem,
		SinceMs: since, Confirmed: true,
		System: &SystemAlertInfo{Kind: kind, Subject: subject, Active: active, Level: level, Details: details},
	})
	if err != nil {
		return nil, err
	}
	channels, err := tx.ListChannels(ctx, store.SystemTenantID)
	if err != nil {
		return nil, storeError(err)
	}
	for _, ch := range channels {
		if ch.State == store.ChannelDisabled || !ch.Subscribes(store.EventSystem) {
			continue
		}
		// A system alert has no monitor; the outbox stores NULL for it.
		if err := enqueueAlertDelivery(ctx, tx, cmd.GetIssuedAtMs(), store.AlertDelivery{
			TenantID: store.SystemTenantID, AlertKey: key, ChannelID: ch.ID, Payload: payload,
		}); err != nil {
			return nil, storeError(err)
		}
	}
	return json.Marshal(SystemAlertResult{Changed: true})
}

// applyRecordIncarnation stores the incarnation a node reported (FA-REP-06).
// A repeated report of the same incarnation changes only the seen time.
func applyRecordIncarnation(ctx context.Context, tx store.StateTx, cmd *pb.Command, r *pb.RecordIncarnation) (json.RawMessage, error) {
	if err := requireCommandLevel(ctx, tx, "recording incarnations", SystemAlertCommandLevel); err != nil {
		return nil, err
	}
	if r.GetNodeId() == "" || r.GetIncarnation() == "" {
		return nil, invalid("incarnation without node or incarnation")
	}
	if err := tx.PutNodeIncarnation(ctx, store.NodeIncarnation{
		NodeID: r.GetNodeId(), Incarnation: r.GetIncarnation(), SeenMs: cmd.GetIssuedAtMs(),
	}); err != nil {
		return nil, storeError(err)
	}
	return json.Marshal(PutResult{ID: r.GetNodeId()})
}

// applyCheckpoint only checks the command level. The fingerprint is computed by
// FSM.Apply after the last write of the command, in the same transaction, and
// kept in memory (ADR 0025): it covers every replicated table including the
// dedup entry of this command, and it is the same on every replica.
func applyCheckpoint(ctx context.Context, tx store.StateTx) (json.RawMessage, error) {
	if err := requireCommandLevel(ctx, tx, "checkpoints", SystemAlertCommandLevel); err != nil {
		return nil, err
	}
	return json.RawMessage("{}"), nil
}
