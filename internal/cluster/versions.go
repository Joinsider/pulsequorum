// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package cluster

import (
	"errors"

	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
)

// MinCommandLevel is the oldest cluster command level whose log entries this
// binary can still apply (ADR 0018). A cluster below it must first be updated
// through the previous minor version.
const MinCommandLevel = 2

// ErrIncompatibleVersion means this binary cannot run the cluster state: its
// command version is older than the cluster's command level, or the level is
// older than MinCommandLevel (ADR 0018).
var ErrIncompatibleVersion = errors.New("incompatible command version")

// minVersion returns the minimum Command.version at which this binary applies
// the payload of cmd the way it is applied now (ADR 0018). Every payload has
// an entry, and a test enforces it. A nil or unknown payload returns 0.
func minVersion(cmd *pb.Command) uint32 {
	switch cmd.GetPayload().(type) {
	case *pb.Command_PutEntity,
		*pb.Command_DeleteEntity,
		*pb.Command_SetMonitorState,
		*pb.Command_EnqueueAlert,
		*pb.Command_AckAlertDelivery,
		*pb.Command_CreateSession,
		*pb.Command_RotateSession,
		*pb.Command_RevokeSessions,
		*pb.Command_RedeemJoinToken,
		*pb.Command_RevokeCertificate,
		*pb.Command_RevokeNode:
		return 1
	case *pb.Command_RegisterNode,
		*pb.Command_SetNodeHealth,
		*pb.Command_SetFeatureLevel:
		return 2
	case *pb.Command_SetNodeConnectivity:
		return 3
	case *pb.Command_ClearMonitorFlapping:
		return 4
	case *pb.Command_ReportDegradedAlerts,
		*pb.Command_ResolveUnconfirmedAlerts,
		*pb.Command_RecordMonitorHistory:
		return 5
	case *pb.Command_SetSystemAlert,
		*pb.Command_RecordIncarnation,
		*pb.Command_Checkpoint:
		return SystemAlertCommandLevel
	case *pb.Command_BindTenantOrg:
		return OIDCCommandLevel
	case *pb.Command_CreateTenant,
		*pb.Command_UpdateTenant,
		*pb.Command_SetTenantSuspended,
		*pb.Command_DeleteTenant,
		*pb.Command_PutPlan,
		*pb.Command_DeletePlan:
		return TenantAdminCommandLevel
	case *pb.Command_CreateApiToken,
		*pb.Command_RevokeApiToken,
		*pb.Command_PruneAudit,
		*pb.Command_Batch:
		return AuditCommandLevel
	}
	return 0
}

// stamp sets the version of cmd from its payload and returns cmd. Constructors
// use it instead of a fixed CommandVersion, so that an old binary keeps
// accepting commands that do not need the new version (ADR 0018).
func stamp(cmd *pb.Command) *pb.Command {
	cmd.Version = minVersion(cmd)
	return cmd
}
