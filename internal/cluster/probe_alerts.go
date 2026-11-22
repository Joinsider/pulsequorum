// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
)

// ProbeAlertResult is the result of SetProbeAlert. Changed is false if the
// alert already had the active state of the command (or the tenant is suspended).
type ProbeAlertResult struct {
	Changed bool `json:"changed"`
}

// NewSetProbeAlert builds the command that records a change of an offline
// alert for a private probe (FA-PRB-06, ADR 0035). Only the leader proposes it.
// The request ID is random per command so repeated transitions within the dedup
// window are still applied.
func NewSetProbeAlert(tenantID, probeID string, active bool, senderNodeID string) *pb.Command {
	return stamp(&pb.Command{
		RequestId: uuid.NewString(),
		TenantId:  tenantID,
		Actor:     &pb.Actor{Kind: pb.Actor_KIND_SYSTEM},
		Payload: &pb.Command_SetProbeAlert{SetProbeAlert: &pb.SetProbeAlert{
			ProbeId:      probeID,
			Active:       active,
			SenderNodeId: senderNodeID,
		}},
	})
}

func probeAlertKey(probeID string, since int64, active bool) string {
	suffix := "clear"
	if active {
		suffix = "active"
	}
	return fmt.Sprintf("probe:%s:%d:%s", probeID, since, suffix)
}

// applySetProbeAlert stores a change of a private probe offline alert and
// enqueues notifications for channels of the owning tenant subscribed to
// "probe_offline" (FA-PRB-06, ADR 0035).
func applySetProbeAlert(ctx context.Context, tx store.StateTx, cmd *pb.Command, s *pb.SetProbeAlert) (json.RawMessage, error) {
	if err := requireCommandLevel(ctx, tx, "probe alerts", ProbeCommandLevel); err != nil {
		return nil, err
	}
	if cmd.GetActor().GetKind() != pb.Actor_KIND_SYSTEM {
		return nil, invalid("probe alerts can only be set by the system")
	}
	tenantID, probeID := cmd.GetTenantId(), s.GetProbeId()
	switch {
	case tenantID == "" || tenantID == store.SystemTenantID:
		return nil, invalid("probe alert requires a non-system tenant")
	case !isNodeID(probeID):
		return nil, invalid("invalid probe ID %q", probeID)
	}
	n, err := tx.GetNode(ctx, probeID)
	if err != nil {
		return nil, storeError(err)
	}
	if n.Role != store.NodeRoleProbe || n.OwnerTenantID != tenantID {
		return nil, invalid("probe %s does not belong to tenant %s", probeID, tenantID)
	}
	tn, err := tx.GetTenant(ctx, tenantID)
	if err != nil {
		return nil, storeError(err)
	}
	if tn.Suspended {
		return json.Marshal(ProbeAlertResult{Changed: false})
	}

	prev, err := tx.GetProbeAlert(ctx, tenantID, probeID)
	exists := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	prevActive := exists && prev.Active
	active := s.GetActive()
	if prevActive == active {
		return json.Marshal(ProbeAlertResult{Changed: false})
	}

	now := cmd.GetIssuedAtMs()
	since := now
	if !active && exists {
		since = prev.SinceMs
	}
	key := probeAlertKey(probeID, since, active)

	if err := tx.PutProbeAlert(ctx, store.ProbeAlert{
		TenantID:  tenantID,
		ProbeID:   probeID,
		Active:    active,
		SinceMs:   since,
		UpdatedMs: now,
	}); err != nil {
		return nil, storeError(err)
	}

	nodes, err := tx.ListNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	sender := &AlertSender{NodeID: s.GetSenderNodeId(), Confirmed: true}
	for _, node := range nodes {
		if node.Role != store.NodeRoleVoter {
			continue
		}
		sender.TotalVoters++
		if node.Healthy {
			sender.ReachableVoters++
		}
	}

	probeName := n.Name
	if probeName == "" {
		probeName = n.ID
	}
	payload, err := json.Marshal(AlertPayload{
		AlertID:   key,
		TenantID:  tenantID,
		Event:     store.EventProbeOffline,
		SinceMs:   since,
		Confirmed: true,
		Sender:    sender,
		Probe:     &ProbeAlertInfo{ID: n.ID, Name: probeName, Active: active},
	})
	if err != nil {
		return nil, err
	}
	channels, err := tx.ListChannels(ctx, tenantID)
	if err != nil {
		return nil, storeError(err)
	}
	for _, ch := range channels {
		if ch.State == store.ChannelDisabled || !ch.Subscribes(store.EventProbeOffline) {
			continue
		}
		// A probe alert has no monitor; the outbox stores NULL for monitor_id.
		if err := enqueueAlertDelivery(ctx, tx, now, tenantID, "", ch.ID, key, payload); err != nil {
			return nil, storeError(err)
		}
	}
	return json.Marshal(ProbeAlertResult{Changed: true})
}
