// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/joinsider/pulsequorum/internal/api/gen"
	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/clusterpki"
	"github.com/joinsider/pulsequorum/internal/membership"
	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/replication"
	"github.com/joinsider/pulsequorum/internal/store"
)

// Cluster administration (FA-CLU-08, FA-PKI-02). Platform admins only.

const (
	defaultJoinTokenMinutes = 60
	maxJoinTokenMinutes     = 1440
	// maxVoters is the voter count above which FA-CLU-11 warns.
	maxVoters = 5
)

// ClusterAdmin is what the cluster administration endpoints need from the
// cluster host. cmd/pulsequorum fills it from the host's authority, membership
// manager and Raft node.
type ClusterAdmin interface {
	CreateJoinToken(ctx context.Context, role pki.Role, tenantID string, ttl time.Duration) (string, time.Time, error)
	Remove(ctx context.Context, nodeID, requestedBy string) (membership.Removal, error)
	Members() ([]cluster.Member, uint64, error)
	Leader() (address, id string)
	ID() string
}

// clusterAdmin returns the cluster administration or 503 when this node has none.
func (h *Handlers) clusterAdmin() (ClusterAdmin, error) {
	if h.deps.ClusterAdmin == nil {
		return nil, &Problem{Status: http.StatusServiceUnavailable, Title: problemTitleServiceUnavailable,
			Detail: "cluster administration is not available"}
	}
	return h.deps.ClusterAdmin, nil
}

// clusterAdminError maps the errors of the cluster administration to problems.
func clusterAdminError(err error) error {
	switch {
	case errors.Is(err, membership.ErrNotLeader):
		return &Problem{Status: http.StatusServiceUnavailable, Title: problemTitleServiceUnavailable,
			Detail: "this node is not the leader, retry", RetryAfter: 1}
	case errors.Is(err, cluster.ErrNotApplied), errors.Is(err, cluster.ErrUnknownOutcome), errors.Is(err, membership.ErrLeaderUnavailable):
		return &Problem{Status: http.StatusServiceUnavailable, Title: problemTitleServiceUnavailable,
			Detail: "no leader available, retry", RetryAfter: 1}
	case errors.Is(err, clusterpki.ErrNoCA):
		return &Problem{Status: http.StatusServiceUnavailable, Title: problemTitleServiceUnavailable,
			Detail: "the cluster CA does not exist yet, retry", RetryAfter: 1}
	case errors.Is(err, membership.ErrUnknownNode):
		return &Problem{Status: http.StatusNotFound, Title: "Not Found", Detail: "node is not a member of the cluster"}
	case errors.Is(err, membership.ErrRemoveLeader):
		return &Problem{Status: http.StatusConflict, Title: "Conflict",
			Detail: "the leader cannot remove itself; transfer leadership to another voter first"}
	case errors.Is(err, membership.ErrLastVoter):
		return &Problem{Status: http.StatusConflict, Title: "Conflict",
			Detail: "the last voter cannot be removed"}
	}
	// Deterministic rejections of the FSM, e.g. a probe token for an unknown
	// tenant; their messages contain no secrets.
	var cmdErr *cluster.CommandError
	if errors.As(err, &cmdErr) {
		switch cmdErr.Code {
		case cluster.CodeInvalid:
			return &Problem{Status: http.StatusUnprocessableEntity, Title: problemTitleValidationFailed, Detail: cmdErr.Message}
		case cluster.CodeNotEnabled:
			return notEnabled(cmdErr)
		}
	}
	return err
}

// notEnabled maps a command that waits for a rolling update to finish (ADR
// 0018) to 503. The detail names the required version; no secrets are involved.
func notEnabled(ce *cluster.CommandError) *Problem {
	return &Problem{Status: http.StatusServiceUnavailable, Title: problemTitleServiceUnavailable, Detail: ce.Message, RetryAfter: 30}
}

// CreateJoinToken issues a one-time token. The secret is returned once and
// never logged; the response must not be cached.
func (h *Handlers) CreateJoinToken(ctx context.Context, req gen.CreateJoinTokenRequestObject) (gen.CreateJoinTokenResponseObject, error) {
	if _, err := principal(ctx, platformAccess); err != nil {
		return nil, err
	}
	admin, err := h.clusterAdmin()
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, &Problem{Status: http.StatusUnprocessableEntity, Title: problemTitleValidationFailed,
			Detail: "request body is required", Fields: []checker.FieldError{{Field: "/role", Message: "required"}}}
	}
	role := pki.Role(req.Body.Role)
	tenantID := ""
	if req.Body.TenantId != nil {
		tenantID = *req.Body.TenantId
	}
	minutes := defaultJoinTokenMinutes
	if req.Body.TtlMinutes != nil {
		minutes = *req.Body.TtlMinutes
	}
	var fields []checker.FieldError
	switch {
	case !role.Valid():
		fields = append(fields, checker.FieldError{Field: "/role", Message: "must be voter or probe"})
	case tenantID != "" && role != pki.RoleProbe:
		fields = append(fields, checker.FieldError{Field: "/tenantId", Message: "only probe tokens can be bound to a tenant"})
	}
	if minutes < 1 || minutes > maxJoinTokenMinutes {
		fields = append(fields, checker.FieldError{Field: "/ttlMinutes", Message: fmt.Sprintf("must be between 1 and %d", maxJoinTokenMinutes)})
	}
	if len(fields) > 0 {
		return nil, &Problem{Status: http.StatusUnprocessableEntity, Title: problemTitleValidationFailed, Fields: fields}
	}

	token, expires, err := admin.CreateJoinToken(ctx, role, tenantID, time.Duration(minutes)*time.Minute)
	if err != nil {
		return nil, clusterAdminError(err)
	}
	// The token itself is deliberately absent from the log.
	h.log.Info("join token created", "role", role, "tenant", tenantID, "expires_at", expires)

	body := gen.JoinToken{Token: token, ExpiresAt: expires.UTC(), Role: gen.JoinTokenRole(role)}
	if tenantID != "" {
		body.TenantId = &tenantID
	}
	noStore := "no-store"
	return gen.CreateJoinToken201JSONResponse{
		Body:    body,
		Headers: gen.CreateJoinToken201ResponseHeaders{CacheControl: &noStore},
	}, nil
}

// ListClusterNodes returns the Raft configuration with quorum warnings
// (FA-CLU-11).
func (h *Handlers) ListClusterNodes(ctx context.Context, _ gen.ListClusterNodesRequestObject) (gen.ListClusterNodesResponseObject, error) {
	if _, err := principal(ctx, platformAccess); err != nil {
		return nil, err
	}
	admin, err := h.clusterAdmin()
	if err != nil {
		return nil, err
	}
	members, _, err := admin.Members()
	if err != nil {
		h.log.Error("could not read the cluster configuration", "err", err)
		return nil, &Problem{Status: http.StatusServiceUnavailable, Title: problemTitleServiceUnavailable,
			Detail: "cluster configuration is not available, retry", RetryAfter: 1}
	}
	_, leaderID := admin.Leader()
	self := admin.ID()
	peers := h.replicationPeers()
	registry, err := h.deps.State.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	commandLevel, err := h.deps.State.CommandLevel(ctx)
	if err != nil {
		return nil, err
	}
	registered := make(map[string]store.Node, len(registry))
	for _, n := range registry {
		registered[n.ID] = n
	}

	var offsets map[string]int64
	if h.deps.ClockOffsets != nil {
		offsets = h.deps.ClockOffsets()
	}
	var ssrf map[string]bool
	if h.deps.SSRFProtectionByNode != nil {
		ssrf = h.deps.SSRFProtectionByNode()
	}
	nodes := make([]gen.ClusterNode, 0, len(members))
	voters := 0
	// Command versions of the registered voters decide whether a rolling
	// update is in progress (ADR 0018, section 7).
	var voterVersions []uint32
	for _, m := range members {
		if m.Voter {
			voters++
		}
		node := gen.ClusterNode{
			Id: m.ID, Address: m.Address, Voter: m.Voter, Leader: m.ID == leaderID, Self: m.ID == self,
		}
		if n, ok := registered[m.ID]; ok {
			name, version, healthy := n.Name, n.Version, n.Healthy
			changedAt := time.UnixMilli(n.HealthChangedMs).UTC()
			capabilities := append([]string{}, n.Capabilities...)
			maxCommand := int32(min(n.MaxCommandVersion, math.MaxInt32))
			node.Name, node.Version, node.Healthy = &name, &version, &healthy
			node.Capabilities, node.HealthChangedAt = &capabilities, &changedAt
			node.MaxCommandVersion = &maxCommand
			impaired := n.ConnectivityImpaired
			node.ConnectivityImpaired = &impaired
			if n.ConnectivityReason != "" {
				reason := gen.ClusterNodeConnectivityReason(n.ConnectivityReason)
				node.ConnectivityReason = &reason
			}
			if m.Voter {
				voterVersions = append(voterVersions, n.MaxCommandVersion)
			}
		}
		if off, ok := offsets[m.ID]; ok && m.ID != self {
			node.ClockOffsetMs = &off
		}
		// Absent for a node without a recent answer, so the SPA ignores it.
		if enabled, ok := ssrf[m.ID]; ok {
			node.SsrfProtection = &enabled
		}
		if ps, ok := peers[m.ID]; ok && m.ID != self {
			if lag, caught := ps.Lag(h.deps.Now()); caught {
				ms := max(lag.Milliseconds(), 0)
				node.ReplicationLagMs = &ms
			}
			if ps.LastError != "" {
				e := ps.LastError
				node.ReplicationError = &e
			}
		}
		nodes = append(nodes, node)
	}
	warnings := make([]string, 0, 2)
	if voters > 0 && voters%2 == 0 {
		warnings = append(warnings, fmt.Sprintf("the cluster has an even number of voters (%d); add or remove one voter (FA-CLU-11)", voters))
	}
	if voters > maxVoters {
		warnings = append(warnings, fmt.Sprintf("the cluster has more than %d voters (%d); more voters add latency without adding fault tolerance (FA-CLU-11)", maxVoters, voters))
	}
	level := int32(min(cluster.EffectiveCommandLevel(commandLevel), math.MaxInt32))
	upgradePending := upgradeInProgress(voterVersions)
	return gen.ListClusterNodes200JSONResponse{
		Nodes: nodes, LeaderId: leaderID, Warnings: warnings,
		CommandLevel: &level, UpgradePending: &upgradePending,
	}, nil
}

// upgradeInProgress reports whether the registered voters support different
// command versions, i.e. a rolling update is running (ADR 0018).
func upgradeInProgress(versions []uint32) bool {
	if len(versions) == 0 {
		return false
	}
	lo, hi := versions[0], versions[0]
	for _, v := range versions[1:] {
		lo, hi = min(lo, v), max(hi, v)
	}
	return lo < hi
}

// replicationPeers indexes the replication state by peer ID. Without a
// replicator it is empty.
func (h *Handlers) replicationPeers() map[string]replication.PeerStatus {
	if h.deps.Replication == nil {
		return nil
	}
	statuses := h.deps.Replication.Status()
	out := make(map[string]replication.PeerStatus, len(statuses))
	for _, s := range statuses {
		out[s.ID] = s
	}
	return out
}

// RemoveClusterNode takes a node out of the cluster. Any voter accepts the
// request; a follower forwards it to the leader, which does the work. The
// leader itself and the last voter cannot be removed. 503 asks the caller to
// retry when no leader can be reached.
func (h *Handlers) RemoveClusterNode(ctx context.Context, req gen.RemoveClusterNodeRequestObject) (gen.RemoveClusterNodeResponseObject, error) {
	p, err := principal(ctx, platformAccess)
	if err != nil {
		return nil, err
	}
	admin, err := h.clusterAdmin()
	if err != nil {
		return nil, err
	}
	removal, err := admin.Remove(ctx, req.NodeId, p.UserID)
	if err != nil {
		return nil, clusterAdminError(err)
	}
	h.log.Info("cluster node removed", "node_id", req.NodeId, "by", p.UserID, "voters_left", removal.VotersLeft)
	resp := gen.RemoveClusterNode200JSONResponse{VotersLeft: removal.VotersLeft}
	if removal.Warning != "" {
		w := removal.Warning
		resp.Warning = &w
	}
	return resp, nil
}
