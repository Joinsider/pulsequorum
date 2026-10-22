// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/joinsider/uptimecluster/internal/api/gen"
	"github.com/joinsider/uptimecluster/internal/checker"
	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/clusterpki"
	"github.com/joinsider/uptimecluster/internal/membership"
	"github.com/joinsider/uptimecluster/internal/pki"
)

// Cluster administration (FA-CLU-08, FA-PKI-02). Platform admins only.

const (
	defaultJoinTokenMinutes = 60
	maxJoinTokenMinutes     = 1440
	// maxVoters is the voter count above which FA-CLU-11 warns.
	maxVoters = 5
)

// ClusterAdmin is what the cluster administration endpoints need from the
// cluster host. cmd/uptimecluster fills it from the host's authority, membership
// manager and Raft node.
type ClusterAdmin interface {
	CreateJoinToken(ctx context.Context, role pki.Role, tenantID string, ttl time.Duration) (string, time.Time, error)
	Remove(ctx context.Context, nodeID string) (membership.Removal, error)
	Members() ([]cluster.Member, uint64, error)
	Leader() (address, id string)
	ID() string
}

// clusterAdmin returns the cluster administration or 503 when this node has none.
func (h *Handlers) clusterAdmin() (ClusterAdmin, error) {
	if h.deps.ClusterAdmin == nil {
		return nil, &Problem{Status: http.StatusServiceUnavailable, Title: "Service Unavailable",
			Detail: "cluster administration is not available"}
	}
	return h.deps.ClusterAdmin, nil
}

// clusterAdminError maps the errors of the cluster administration to problems.
func clusterAdminError(err error) error {
	switch {
	case errors.Is(err, membership.ErrNotLeader):
		return &Problem{Status: http.StatusServiceUnavailable, Title: "Service Unavailable",
			Detail: "this node is not the leader, retry", RetryAfter: 1}
	case errors.Is(err, cluster.ErrNotApplied), errors.Is(err, cluster.ErrUnknownOutcome):
		return &Problem{Status: http.StatusServiceUnavailable, Title: "Service Unavailable",
			Detail: "no leader available, retry", RetryAfter: 1}
	case errors.Is(err, clusterpki.ErrNoCA):
		return &Problem{Status: http.StatusServiceUnavailable, Title: "Service Unavailable",
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
	if errors.As(err, &cmdErr) && cmdErr.Code == cluster.CodeInvalid {
		return &Problem{Status: http.StatusUnprocessableEntity, Title: "Validation Failed", Detail: cmdErr.Message}
	}
	return err
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
		return nil, &Problem{Status: http.StatusUnprocessableEntity, Title: "Validation Failed",
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
		return nil, &Problem{Status: http.StatusUnprocessableEntity, Title: "Validation Failed", Fields: fields}
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
		return nil, &Problem{Status: http.StatusServiceUnavailable, Title: "Service Unavailable",
			Detail: "cluster configuration is not available, retry", RetryAfter: 1}
	}
	_, leaderID := admin.Leader()
	self := admin.ID()

	nodes := make([]gen.ClusterNode, 0, len(members))
	voters := 0
	for _, m := range members {
		if m.Voter {
			voters++
		}
		nodes = append(nodes, gen.ClusterNode{
			Id: m.ID, Address: m.Address, Voter: m.Voter, Leader: m.ID == leaderID, Self: m.ID == self,
		})
	}
	warnings := make([]string, 0, 2)
	if voters > 0 && voters%2 == 0 {
		warnings = append(warnings, fmt.Sprintf("the cluster has an even number of voters (%d); add or remove one voter (FA-CLU-11)", voters))
	}
	if voters > maxVoters {
		warnings = append(warnings, fmt.Sprintf("the cluster has more than %d voters (%d); more voters add latency without adding fault tolerance (FA-CLU-11)", maxVoters, voters))
	}
	return gen.ListClusterNodes200JSONResponse{Nodes: nodes, LeaderId: leaderID, Warnings: warnings}, nil
}

// RemoveClusterNode takes a node out of the cluster. The leader does the work;
// a follower answers 503 so that the caller retries against the leader.
func (h *Handlers) RemoveClusterNode(ctx context.Context, req gen.RemoveClusterNodeRequestObject) (gen.RemoveClusterNodeResponseObject, error) {
	p, err := principal(ctx, platformAccess)
	if err != nil {
		return nil, err
	}
	admin, err := h.clusterAdmin()
	if err != nil {
		return nil, err
	}
	removal, err := admin.Remove(ctx, req.NodeId)
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
