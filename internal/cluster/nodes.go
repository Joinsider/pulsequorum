// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
)

// Limits of the node registry (ADR 0017). Lengths are counted in bytes.
const (
	maxNodeNameLen      = 200
	maxNodeRegionLen    = 64
	maxNodeVersionLen   = 64
	maxNodeCapabilities = 32
	maxCapabilityLen    = 32
	maxNodeLabels       = 32
	maxLabelKeyLen      = 64
	maxLabelValueLen    = 256
)

// NodeResult is the result of RegisterNode, SetNodeHealth and
// SetNodeConnectivity: the registry entry after the command.
type NodeResult struct {
	ID                    string `json:"id"`
	Role                  string `json:"role"`
	Healthy               bool   `json:"healthy"`
	HealthChangedMs       int64  `json:"healthChangedMs"`
	ConnectivityImpaired  bool   `json:"connectivityImpaired"`
	ConnectivityReason    string `json:"connectivityReason"`
	ConnectivityChangedMs int64  `json:"connectivityChangedMs"`
	RegisteredMs          int64  `json:"registeredMs"`
	UpdatedMs             int64  `json:"updatedMs"`
}

// NewRegisterNode builds the command that registers a node or updates its
// registry entry (ADR 0017). The node proposes it itself.
func NewRegisterNode(requestID string, n store.Node) *pb.Command {
	return stamp(&pb.Command{
		RequestId: requestID,
		Payload:   &pb.Command_RegisterNode{RegisterNode: &pb.RegisterNode{Node: nodeInfoFrom(n)}},
	})
}

// NewSetNodeHealth builds the command that records a health change of a node
// (ADR 0017). Only the leader proposes it, and only when the value changes.
func NewSetNodeHealth(requestID, nodeID string, healthy bool) *pb.Command {
	return stamp(&pb.Command{
		RequestId: requestID,
		Payload:   &pb.Command_SetNodeHealth{SetNodeHealth: &pb.SetNodeHealth{NodeId: nodeID, Healthy: healthy}},
	})
}

// nodeInfoFrom converts a registry entry to its wire form. Capabilities are
// sorted and deduplicated, as they are in the registry.
func nodeInfoFrom(n store.Node) *pb.NodeInfo {
	return &pb.NodeInfo{
		Id:                n.ID,
		Name:              n.Name,
		Role:              roleToProto(n.Role),
		Region:            n.Region,
		Labels:            maps.Clone(n.Labels),
		Capabilities:      normalizeCapabilities(n.Capabilities),
		Version:           n.Version,
		MaxCommandVersion: n.MaxCommandVersion,
	}
}

// nodeFromInfo converts and validates the wire form of a node. Fields the
// registry does not keep (incarnation, advertise address, owner) are dropped.
func nodeFromInfo(info *pb.NodeInfo) (store.Node, error) {
	if !isNodeID(info.GetId()) {
		return store.Node{}, invalid("node ID must be 1 to 128 characters of [A-Za-z0-9_-]")
	}
	role, ok := roleFromProto(info.GetRole())
	if !ok {
		return store.Node{}, invalid("node role must be voter or probe")
	}
	if len(info.GetName()) > maxNodeNameLen {
		return store.Node{}, invalid("node name must not exceed %d bytes", maxNodeNameLen)
	}
	if len(info.GetRegion()) > maxNodeRegionLen {
		return store.Node{}, invalid("node region must not exceed %d bytes", maxNodeRegionLen)
	}
	if len(info.GetVersion()) > maxNodeVersionLen {
		return store.Node{}, invalid("node version must not exceed %d bytes", maxNodeVersionLen)
	}
	if len(info.GetCapabilities()) > maxNodeCapabilities {
		return store.Node{}, invalid("node must not have more than %d capabilities", maxNodeCapabilities)
	}
	for _, c := range info.GetCapabilities() {
		if !isCapability(c) {
			return store.Node{}, invalid("capability %q must be 1 to %d characters of [a-z0-9_-]", c, maxCapabilityLen)
		}
	}
	if len(info.GetLabels()) > maxNodeLabels {
		return store.Node{}, invalid("node must not have more than %d labels", maxNodeLabels)
	}
	for k, v := range info.GetLabels() {
		if len(k) > maxLabelKeyLen || len(v) > maxLabelValueLen {
			return store.Node{}, invalid("label keys must not exceed %d bytes and values %d bytes", maxLabelKeyLen, maxLabelValueLen)
		}
	}
	return store.Node{
		ID:                info.GetId(),
		Name:              info.GetName(),
		Role:              role,
		Capabilities:      normalizeCapabilities(info.GetCapabilities()),
		Labels:            maps.Clone(info.GetLabels()),
		Region:            info.GetRegion(),
		Version:           info.GetVersion(),
		MaxCommandVersion: info.GetMaxCommandVersion(),
	}, nil
}

func roleToProto(role string) pb.NodeInfo_Role {
	switch role {
	case store.NodeRoleVoter:
		return pb.NodeInfo_ROLE_VOTER
	case store.NodeRoleProbe:
		return pb.NodeInfo_ROLE_PROBE
	}
	return pb.NodeInfo_ROLE_UNSPECIFIED
}

func roleFromProto(role pb.NodeInfo_Role) (string, bool) {
	switch role {
	case pb.NodeInfo_ROLE_VOTER:
		return store.NodeRoleVoter, true
	case pb.NodeInfo_ROLE_PROBE:
		return store.NodeRoleProbe, true
	}
	return "", false
}

// normalizeCapabilities returns a sorted copy without duplicates.
func normalizeCapabilities(caps []string) []string {
	out := slices.Clone(caps)
	slices.Sort(out)
	return slices.Compact(out)
}

// isCapability reports whether s is 1 to 32 characters of [a-z0-9_-].
func isCapability(s string) bool {
	if s == "" || len(s) > maxCapabilityLen {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// applyRegisterNode upserts a registry entry (ADR 0017). The first registration
// makes the node healthy; an update keeps the health, the connectivity and the
// registration time, which only the leader changes (ADR 0022).
// A revoked node cannot register again.
func applyRegisterNode(ctx context.Context, tx store.StateTx, cmd *pb.Command, r *pb.RegisterNode) (json.RawMessage, error) {
	n, err := nodeFromInfo(r.GetNode())
	if err != nil {
		return nil, err
	}
	_, err = tx.GetRevokedNode(ctx, n.ID)
	switch {
	case err == nil:
		return nil, &CommandError{Code: CodeConflict, Message: "node " + n.ID + " is revoked"}
	case !errors.Is(err, store.ErrNotFound):
		return nil, err
	}

	now := cmd.GetIssuedAtMs()
	existing, err := tx.GetNode(ctx, n.ID)
	switch {
	case err == nil:
		n.Healthy = existing.Healthy
		n.HealthChangedMs = existing.HealthChangedMs
		n.ConnectivityImpaired = existing.ConnectivityImpaired
		n.ConnectivityReason = existing.ConnectivityReason
		n.ConnectivityChangedMs = existing.ConnectivityChangedMs
		n.RegisteredMs = existing.RegisteredMs
	case errors.Is(err, store.ErrNotFound):
		n.Healthy = true
		n.HealthChangedMs = now
		n.RegisteredMs = now
	default:
		return nil, err
	}
	n.UpdatedMs = now
	if err := tx.PutNode(ctx, n); err != nil {
		return nil, storeError(err)
	}
	return json.Marshal(nodeResult(n))
}

// applySetNodeHealth records a health change. An unchanged value is a success
// without a write, so UpdatedMs keeps the time of the last real change.
func applySetNodeHealth(ctx context.Context, tx store.StateTx, cmd *pb.Command, s *pb.SetNodeHealth) (json.RawMessage, error) {
	if !isNodeID(s.GetNodeId()) {
		return nil, invalid("node ID must be 1 to 128 characters of [A-Za-z0-9_-]")
	}
	n, err := tx.GetNode(ctx, s.GetNodeId())
	if err != nil {
		return nil, storeError(err)
	}
	if n.Healthy != s.GetHealthy() {
		now := cmd.GetIssuedAtMs()
		n.Healthy = s.GetHealthy()
		n.HealthChangedMs = now
		n.UpdatedMs = now
		if err := tx.PutNode(ctx, n); err != nil {
			return nil, storeError(err)
		}
	}
	return json.Marshal(nodeResult(n))
}

// Reasons of an impaired connectivity (ADR 0022).
const (
	ConnectivityReasonReference = "reference"
	ConnectivityReasonDeviation = "deviation"
)

// applySetNodeConnectivity records a connectivity change (ADR 0022). A reason
// is required while impaired and forbidden otherwise. An unchanged state is a
// success without a write.
func applySetNodeConnectivity(ctx context.Context, tx store.StateTx, cmd *pb.Command, s *pb.SetNodeConnectivity) (json.RawMessage, error) {
	if !isNodeID(s.GetNodeId()) {
		return nil, invalid("node ID must be 1 to 128 characters of [A-Za-z0-9_-]")
	}
	reason := s.GetReason()
	switch {
	case s.GetImpaired() && reason != ConnectivityReasonReference && reason != ConnectivityReasonDeviation:
		return nil, invalid("connectivity reason must be %q or %q while impaired", ConnectivityReasonReference, ConnectivityReasonDeviation)
	case !s.GetImpaired() && reason != "":
		return nil, invalid("connectivity reason must be empty when not impaired")
	}
	n, err := tx.GetNode(ctx, s.GetNodeId())
	if err != nil {
		return nil, storeError(err)
	}
	if n.ConnectivityImpaired != s.GetImpaired() || n.ConnectivityReason != reason {
		now := cmd.GetIssuedAtMs()
		if err := tx.SetNodeConnectivity(ctx, n.ID, s.GetImpaired(), reason, now); err != nil {
			return nil, storeError(err)
		}
		n.ConnectivityImpaired = s.GetImpaired()
		n.ConnectivityReason = reason
		n.ConnectivityChangedMs = now
		n.UpdatedMs = now
	}
	return json.Marshal(nodeResult(n))
}

func nodeResult(n store.Node) NodeResult {
	return NodeResult{
		ID:                    n.ID,
		Role:                  n.Role,
		Healthy:               n.Healthy,
		HealthChangedMs:       n.HealthChangedMs,
		ConnectivityImpaired:  n.ConnectivityImpaired,
		ConnectivityReason:    n.ConnectivityReason,
		ConnectivityChangedMs: n.ConnectivityChangedMs,
		RegisteredMs:          n.RegisteredMs,
		UpdatedMs:             n.UpdatedMs,
	}
}
