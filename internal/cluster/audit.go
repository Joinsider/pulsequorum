// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"time"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
)

// AuditCommandLevel is the command level of API tokens, the audit log, the
// PruneAudit command and Batch (ADR 0031). Below it those commands are rejected
// and the FSM writes no audit entries, so that voters of different versions do
// not diverge (ADR 0018).
const AuditCommandLevel = 9

// auditPruneInterval is how often the leader prunes the audit log and the
// expired API tokens (ADR 0031, section 2).
const auditPruneInterval = time.Hour

// Actor kinds as they are stored in the audit log.
const (
	auditActorUser     = "user"
	auditActorAPIToken = "api_token"
	auditActorSystem   = "system"
	auditActorUnknown  = "unknown"
)

// auditPolicy says whether the audit log records a payload type (ADR 0031, section 2).
type auditPolicy bool

const (
	auditNever  auditPolicy = false
	auditAlways auditPolicy = true
)

// auditPolicies decides for every payload type of the Command oneof whether an
// applied command gets audit entries. A test fails for a payload without an
// entry, so a new payload cannot be added without this decision.
var auditPolicies = map[reflect.Type]auditPolicy{
	// Changes made by users, API tokens and administrative system commands.
	reflect.TypeFor[*pb.Command_PutEntity]():          auditAlways,
	reflect.TypeFor[*pb.Command_DeleteEntity]():       auditAlways,
	reflect.TypeFor[*pb.Command_Batch]():              auditAlways,
	reflect.TypeFor[*pb.Command_CreateApiToken]():     auditAlways,
	reflect.TypeFor[*pb.Command_RevokeApiToken]():     auditAlways,
	reflect.TypeFor[*pb.Command_BindTenantOrg]():      auditAlways,
	reflect.TypeFor[*pb.Command_CreateTenant]():       auditAlways,
	reflect.TypeFor[*pb.Command_UpdateTenant]():       auditAlways,
	reflect.TypeFor[*pb.Command_SetTenantSuspended](): auditAlways,
	reflect.TypeFor[*pb.Command_DeleteTenant]():       auditAlways,
	reflect.TypeFor[*pb.Command_PutPlan]():            auditAlways,
	reflect.TypeFor[*pb.Command_DeletePlan]():         auditAlways,
	reflect.TypeFor[*pb.Command_RedeemJoinToken]():    auditAlways,
	reflect.TypeFor[*pb.Command_RevokeCertificate]():  auditAlways,
	reflect.TypeFor[*pb.Command_RevokeNode]():         auditAlways,
	reflect.TypeFor[*pb.Command_RegisterNode]():       auditAlways,
	reflect.TypeFor[*pb.Command_SetFeatureLevel]():    auditAlways,

	// Operational commands: state, alert delivery, sessions, health, history,
	// system alerts, checkpoints and the pruning itself.
	reflect.TypeFor[*pb.Command_SetMonitorState]():          auditNever,
	reflect.TypeFor[*pb.Command_EnqueueAlert]():             auditNever,
	reflect.TypeFor[*pb.Command_AckAlertDelivery]():         auditNever,
	reflect.TypeFor[*pb.Command_CreateSession]():            auditNever,
	reflect.TypeFor[*pb.Command_RotateSession]():            auditNever,
	reflect.TypeFor[*pb.Command_RevokeSessions]():           auditNever,
	reflect.TypeFor[*pb.Command_SetNodeHealth]():            auditNever,
	reflect.TypeFor[*pb.Command_SetNodeConnectivity]():      auditNever,
	reflect.TypeFor[*pb.Command_ClearMonitorFlapping]():     auditNever,
	reflect.TypeFor[*pb.Command_ReportDegradedAlerts]():     auditNever,
	reflect.TypeFor[*pb.Command_ResolveUnconfirmedAlerts](): auditNever,
	reflect.TypeFor[*pb.Command_RecordMonitorHistory]():     auditNever,
	reflect.TypeFor[*pb.Command_SetSystemAlert]():           auditNever,
	reflect.TypeFor[*pb.Command_RecordIncarnation]():        auditNever,
	reflect.TypeFor[*pb.Command_Checkpoint]():               auditNever,
	reflect.TypeFor[*pb.Command_PruneAudit]():               auditNever,
}

// auditHints are the values of audit_action that refine the verb of a monitor
// PutEntity (ADR 0031, section 2).
var auditHints = map[string]bool{"pause": true, "resume": true, "tags": true, "clone": true}

// entityObjects names the object of the audit log per entity kind.
var entityObjects = map[pb.EntityKind]string{
	pb.EntityKind_ENTITY_KIND_MONITOR:              "monitor",
	pb.EntityKind_ENTITY_KIND_NOTIFICATION_CHANNEL: "channel",
	pb.EntityKind_ENTITY_KIND_MAINTENANCE_WINDOW:   "maintenance_window",
	pb.EntityKind_ENTITY_KIND_USER:                 "user",
	pb.EntityKind_ENTITY_KIND_TENANT:               "tenant",
	pb.EntityKind_ENTITY_KIND_JOIN_TOKEN:           "join_token",
	pb.EntityKind_ENTITY_KIND_SECRET_KEY:           "secret_key",
	pb.EntityKind_ENTITY_KIND_SIGNING_KEY:          "signing_key",
	pb.EntityKind_ENTITY_KIND_CLUSTER_CA:           "cluster_ca",
}

// tenantEntities are the entity kinds whose audit entries belong to the tenant
// of the command. The others are platform objects and go to the system tenant.
var tenantEntities = map[pb.EntityKind]bool{
	pb.EntityKind_ENTITY_KIND_MONITOR:              true,
	pb.EntityKind_ENTITY_KIND_NOTIFICATION_CHANNEL: true,
	pb.EntityKind_ENTITY_KIND_MAINTENANCE_WINDOW:   true,
	pb.EntityKind_ENTITY_KIND_USER:                 true,
}

// auditRecord is what an audit entry says about an applied command. writeAudit
// adds the fields the command has in common (time, actor, request ID).
type auditRecord struct {
	tenantID   string
	action     string
	objectKind string
	objectID   string
}

// checkAuditHints rejects an audit_action the FSM does not know, and one on a
// payload that cannot carry it. It runs before the command is applied, so an
// invalid hint never changes state (ADR 0031, section 2). The rule holds at
// every command level, so that all voters reject the same commands.
//
// Like every rule of ADR 0031 it applies only from AuditCommandLevel on:
// binaries of level 8 do not know the field and apply such a command, so
// rejecting it below level 9 would let mixed voters diverge (ADR 0018).
func checkAuditHints(ctx context.Context, tx store.StateTx, cmd *pb.Command) error {
	if !hasAuditHint(cmd) {
		return nil
	}
	level, err := currentCommandLevel(ctx, tx)
	if err != nil {
		return err
	}
	if level < AuditCommandLevel {
		return nil
	}
	hint := cmd.GetAuditAction()
	switch p := cmd.GetPayload().(type) {
	case *pb.Command_PutEntity:
		if msg := hintProblem(p.PutEntity.GetKind(), hint); msg != "" {
			return invalid("%s", msg)
		}
		return nil
	case *pb.Command_Batch:
		if hint != "" {
			return invalid("audit_action of a batch is set per operation")
		}
		for i, op := range p.Batch.GetOps() {
			if msg := batchOpHintProblem(op); msg != "" {
				return invalid("operation %d: %s", i, msg)
			}
		}
		return nil
	}
	if hint != "" {
		return invalid("audit_action is only allowed for monitor puts")
	}
	return nil
}

// hasAuditHint reports whether cmd or one of its batch operations carries an
// audit_action.
func hasAuditHint(cmd *pb.Command) bool {
	if cmd.GetAuditAction() != "" {
		return true
	}
	for _, op := range cmd.GetBatch().GetOps() {
		if op.GetAuditAction() != "" {
			return true
		}
	}
	return false
}

// hintProblem returns why hint cannot refine a PutEntity of kind, or "".
func hintProblem(kind pb.EntityKind, hint string) string {
	switch {
	case hint == "":
		return ""
	case kind != pb.EntityKind_ENTITY_KIND_MONITOR:
		return fmt.Sprintf("audit_action %q is only allowed for monitors", hint)
	case !auditHints[hint]:
		return fmt.Sprintf("unknown audit_action %q", hint)
	}
	return ""
}

// batchOpHintProblem returns why the audit_action of a batch operation is not
// allowed, or "".
func batchOpHintProblem(op *pb.BatchOp) string {
	if put := op.GetPutEntity(); put != nil {
		return hintProblem(put.GetKind(), op.GetAuditAction())
	}
	if op.GetAuditAction() != "" {
		return "audit_action is only allowed for monitor puts"
	}
	return ""
}

// writeAudit appends the audit entries of an applied command to the caller's
// transaction (ADR 0031, section 2). It runs only from AuditCommandLevel on,
// and only after the command was applied, so rejected commands and
// deduplicated replays write nothing. It must not read the clock.
func writeAudit(ctx context.Context, tx store.StateTx, index uint64, cmd *pb.Command) error {
	policy, ok := auditPolicies[reflect.TypeOf(cmd.GetPayload())]
	if !ok || policy == auditNever {
		return nil
	}
	records := auditRecords(cmd)
	if len(records) == 0 {
		return nil
	}
	actor := cmd.GetActor()
	entries := make([]store.AuditEntry, 0, len(records))
	for i, rec := range records {
		entries = append(entries, store.AuditEntry{
			Index:      index,
			Sub:        i,
			AtMs:       cmd.GetIssuedAtMs(),
			TenantID:   rec.tenantID,
			ActorKind:  auditActorKind(actor.GetKind()),
			ActorID:    actor.GetId(),
			ActorName:  actor.GetName(),
			Action:     rec.action,
			ObjectKind: rec.objectKind,
			ObjectID:   rec.objectID,
			RequestID:  cmd.GetRequestId(),
		})
	}
	return tx.InsertAudit(ctx, entries)
}

// auditRecords returns one record per audit entry of cmd: one for most
// commands, one per operation for a Batch, numbered by the position.
func auditRecords(cmd *pb.Command) []auditRecord {
	tenantID := cmd.GetTenantId()
	switch p := cmd.GetPayload().(type) {
	case *pb.Command_PutEntity:
		put := p.PutEntity
		return []auditRecord{entityRecord(tenantID, put.GetKind(), put.GetId(), putVerb(cmd.GetAuditAction(), put.GetExpectedRevision()))}
	case *pb.Command_DeleteEntity:
		del := p.DeleteEntity
		return []auditRecord{entityRecord(tenantID, del.GetKind(), del.GetId(), "delete")}
	case *pb.Command_Batch:
		records := make([]auditRecord, 0, len(p.Batch.GetOps()))
		for _, op := range p.Batch.GetOps() {
			if put := op.GetPutEntity(); put != nil {
				records = append(records, entityRecord(tenantID, put.GetKind(), put.GetId(), putVerb(op.GetAuditAction(), put.GetExpectedRevision())))
			} else if del := op.GetDeleteEntity(); del != nil {
				records = append(records, entityRecord(tenantID, del.GetKind(), del.GetId(), "delete"))
			}
		}
		return records

	case *pb.Command_CreateApiToken:
		return []auditRecord{{tenantID: tenantID, action: "api_token.create", objectKind: "api_token", objectID: p.CreateApiToken.GetId()}}
	case *pb.Command_RevokeApiToken:
		return []auditRecord{{tenantID: tenantID, action: "api_token.revoke", objectKind: "api_token", objectID: p.RevokeApiToken.GetId()}}

	case *pb.Command_BindTenantOrg:
		return []auditRecord{systemRecord("tenant.bind_org", "tenant", p.BindTenantOrg.GetTenantId())}
	case *pb.Command_CreateTenant:
		return []auditRecord{systemRecord("tenant.create", "tenant", p.CreateTenant.GetTenantId())}
	case *pb.Command_UpdateTenant:
		return []auditRecord{systemRecord("tenant.update", "tenant", p.UpdateTenant.GetTenantId())}
	case *pb.Command_SetTenantSuspended:
		verb := "resume"
		if p.SetTenantSuspended.GetSuspended() {
			verb = "suspend"
		}
		return []auditRecord{systemRecord("tenant."+verb, "tenant", p.SetTenantSuspended.GetTenantId())}
	case *pb.Command_DeleteTenant:
		return []auditRecord{systemRecord("tenant.delete", "tenant", p.DeleteTenant.GetTenantId())}

	case *pb.Command_PutPlan:
		return []auditRecord{systemRecord("plan.put", "plan", p.PutPlan.GetPlanId())}
	case *pb.Command_DeletePlan:
		return []auditRecord{systemRecord("plan.delete", "plan", p.DeletePlan.GetPlanId())}

	case *pb.Command_RedeemJoinToken:
		return []auditRecord{systemRecord("join_token.redeem", "join_token", p.RedeemJoinToken.GetTokenId())}
	case *pb.Command_RevokeCertificate:
		return []auditRecord{systemRecord("certificate.revoke", "certificate", p.RevokeCertificate.GetSerial())}
	case *pb.Command_RevokeNode:
		return []auditRecord{systemRecord("node.revoke", "node", p.RevokeNode.GetNodeId())}
	case *pb.Command_RegisterNode:
		return []auditRecord{systemRecord("node.register", "node", p.RegisterNode.GetNode().GetId())}
	case *pb.Command_SetFeatureLevel:
		return []auditRecord{systemRecord("cluster.set_feature_level", "cluster", strconv.FormatUint(uint64(p.SetFeatureLevel.GetLevel()), 10))}
	}
	return nil
}

// entityRecord describes a PutEntity or DeleteEntity of kind.
func entityRecord(tenantID string, kind pb.EntityKind, id, verb string) auditRecord {
	object := entityObjects[kind]
	rec := auditRecord{tenantID: store.SystemTenantID, action: object + "." + verb, objectKind: object, objectID: id}
	if tenantEntities[kind] {
		rec.tenantID = tenantID
	}
	return rec
}

// putVerb is the verb of a PutEntity: the audit_action hint if set, otherwise
// create for a new object and update for a replaced one.
func putVerb(hint string, expectedRevision uint64) string {
	switch {
	case hint != "":
		return hint
	case expectedRevision == 0:
		return "create"
	}
	return "update"
}

// systemRecord describes a platform action, recorded under the system tenant.
func systemRecord(action, objectKind, objectID string) auditRecord {
	return auditRecord{tenantID: store.SystemTenantID, action: action, objectKind: objectKind, objectID: objectID}
}

// auditActorKind maps an actor kind to the value stored in the audit log.
func auditActorKind(kind pb.Actor_Kind) string {
	switch kind {
	case pb.Actor_KIND_USER:
		return auditActorUser
	case pb.Actor_KIND_API_TOKEN:
		return auditActorAPIToken
	case pb.Actor_KIND_SYSTEM:
		return auditActorSystem
	}
	return auditActorUnknown
}

// RunAuditPrune proposes PruneAudit once per hour on the leader, from command
// level AuditCommandLevel on (ADR 0031, section 2). The cut-offs are computed
// from the leader clock; the FSM checks them against the command time. It runs
// until ctx ends.
func RunAuditPrune(ctx context.Context, n *Node, state store.State, retention time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(auditPruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !n.IsLeader() {
			continue
		}
		level, err := state.CommandLevel(ctx)
		if err != nil {
			log.Warn("read command level for audit pruning", "err", err)
			continue
		}
		if EffectiveCommandLevel(level) < AuditCommandLevel {
			continue
		}
		now := n.now().UnixMilli()
		cmd := NewPruneAudit(now-retention.Milliseconds(), now-APITokenRetentionAfterExpiry.Milliseconds())
		if _, err := n.Apply(ctx, cmd); err != nil {
			log.Warn("audit pruning failed", "err", err)
		}
	}
}
