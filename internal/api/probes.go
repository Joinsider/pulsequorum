// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/pulsequorum/internal/api/gen"
	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/tenant"
)

// CreatePrivateProbeJoinToken issues a one-time join token bound to the caller's
// tenant (FA-PRB-05, ADR 0035). Restricted to interactive tenant admins; API
// tokens are rejected by middleware and by the FSM actor check.
func (h *Handlers) CreatePrivateProbeJoinToken(ctx context.Context, req gen.CreatePrivateProbeJoinTokenRequestObject) (gen.CreatePrivateProbeJoinTokenResponseObject, error) {
	p, err := principal(ctx, adminAccess)
	if err != nil {
		return nil, err
	}
	admin, err := h.clusterAdmin()
	if err != nil {
		return nil, err
	}
	minutes := defaultJoinTokenMinutes
	if req.Body != nil && req.Body.TtlMinutes != nil {
		minutes = *req.Body.TtlMinutes
	}
	if minutes < 1 || minutes > maxJoinTokenMinutes {
		return nil, &Problem{
			Status: http.StatusUnprocessableEntity,
			Title:  problemTitleValidationFailed,
			Fields: []checker.FieldError{{
				Field:   "/ttlMinutes",
				Message: fmt.Sprintf("must be between 1 and %d", maxJoinTokenMinutes),
			}},
		}
	}
	if err := h.checkPrivateProbeLimit(ctx, p.TenantID); err != nil {
		return nil, err
	}

	token, expires, err := admin.CreateJoinToken(ctx, pki.RoleProbe, p.TenantID, time.Duration(minutes)*time.Minute, actorOf(p))
	if err != nil {
		return nil, clusterAdminError(err)
	}
	// The token itself is deliberately absent from logs.
	h.log.Info("private probe join token created", "tenant", p.TenantID, "expires_at", expires)

	tenantID := p.TenantID
	noStore := "no-store"
	return gen.CreatePrivateProbeJoinToken201JSONResponse{
		Body: gen.JoinToken{
			Token:     token,
			ExpiresAt: expires.UTC(),
			Role:      gen.JoinTokenRoleProbe,
			TenantId:  &tenantID,
		},
		Headers: gen.CreatePrivateProbeJoinToken201ResponseHeaders{CacheControl: &noStore},
	}, nil
}

// ListPrivateProbes returns the private probes registered for the caller's
// tenant (FA-PRB-05, ADR 0035). Foreign probes and cluster addresses are never exposed.
func (h *Handlers) ListPrivateProbes(ctx context.Context, _ gen.ListPrivateProbesRequestObject) (gen.ListPrivateProbesResponseObject, error) {
	p, err := principal(ctx, adminAccess)
	if err != nil {
		return nil, err
	}
	nodes, err := h.deps.State.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	alerts, err := h.deps.State.ListProbeAlerts(ctx, p.TenantID, false)
	if err != nil {
		return nil, err
	}
	alertActive := make(map[string]bool, len(alerts))
	for _, a := range alerts {
		alertActive[a.ProbeID] = a.Active
	}
	var sessions map[string]ProbeSessionView
	if h.deps.ProbeSessions != nil {
		sessions = h.deps.ProbeSessions()
	}

	out := make([]gen.PrivateProbe, 0)
	for _, n := range nodes {
		if n.Role != store.NodeRoleProbe || n.OwnerTenantID != p.TenantID {
			continue
		}
		caps := slices.Clone(n.Capabilities)
		if caps == nil {
			caps = []string{}
		}
		offline := alertActive[n.ID]
		item := gen.PrivateProbe{
			Id:              n.ID,
			Name:            n.Name,
			Capabilities:    caps,
			Healthy:         n.Healthy,
			HealthChangedAt: time.UnixMilli(n.HealthChangedMs).UTC(),
			OfflineAlert:    &offline,
			Version:         n.Version,
		}
		if n.Region != "" {
			region := n.Region
			item.Region = &region
		}
		if sess, ok := sessions[n.ID]; ok {
			if sess.LastMessageMs > 0 {
				t := time.UnixMilli(sess.LastMessageMs).UTC()
				item.LastContactAt = &t
			}
			item.ProbeBuffer = probeBufferStatus(sess.Buffer)
		}
		out = append(out, item)
	}
	slices.SortFunc(out, func(a, b gen.PrivateProbe) int {
		if c := cmp.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return cmp.Compare(a.Id, b.Id)
	})
	return gen.ListPrivateProbes200JSONResponse(out), nil
}

// RemovePrivateProbe revokes and removes a private probe owned by the caller's
// tenant (FA-PRB-05, ADR 0035). Foreign or unknown probes answer 404.
func (h *Handlers) RemovePrivateProbe(ctx context.Context, req gen.RemovePrivateProbeRequestObject) (gen.RemovePrivateProbeResponseObject, error) {
	p, err := principal(ctx, adminAccess)
	if err != nil {
		return nil, err
	}
	nodes, err := h.deps.State.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	owned := false
	for _, n := range nodes {
		if n.ID == req.ProbeId && n.Role == store.NodeRoleProbe && n.OwnerTenantID == p.TenantID {
			owned = true
			break
		}
	}
	if !owned {
		return nil, notFound("probe")
	}

	cmd := cluster.NewRevokeTenantProbe(uuid.NewString(), p.TenantID, req.ProbeId)
	if _, err := h.apply(ctx, cmd); err != nil {
		return nil, err
	}
	h.log.Info("private probe removed", "tenant", p.TenantID, "probe_id", req.ProbeId)
	return gen.RemovePrivateProbe204Response{}, nil
}

// ListAssignableNodes returns the voters, public probes, and own private probes
// selectable in monitor assignment (ADR 0035). Foreign private probes and cluster
// addresses are never exposed.
func (h *Handlers) ListAssignableNodes(ctx context.Context, _ gen.ListAssignableNodesRequestObject) (gen.ListAssignableNodesResponseObject, error) {
	p, err := principal(ctx, readAccess)
	if err != nil {
		return nil, err
	}
	nodes, err := h.deps.State.ListNodes(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]gen.AssignableNode, 0, len(nodes))
	for _, n := range nodes {
		var role gen.AssignableNodeRole
		private := false
		switch n.Role {
		case store.NodeRoleVoter:
			role = gen.AssignableNodeRoleVoter
		case store.NodeRoleProbe:
			if n.OwnerTenantID != "" && n.OwnerTenantID != p.TenantID {
				continue
			}
			role = gen.AssignableNodeRoleProbe
			private = n.OwnerTenantID == p.TenantID
		default:
			continue
		}
		caps := slices.Clone(n.Capabilities)
		if caps == nil {
			caps = []string{}
		}
		item := gen.AssignableNode{
			Id:           n.ID,
			Name:         n.Name,
			Role:         role,
			Private:      private,
			Healthy:      n.Healthy,
			Capabilities: caps,
		}
		if n.Region != "" {
			region := n.Region
			item.Region = &region
		}
		out = append(out, item)
	}
	slices.SortFunc(out, func(a, b gen.AssignableNode) int {
		if ra, rb := assignableSortRank(a), assignableSortRank(b); ra != rb {
			return cmp.Compare(ra, rb)
		}
		if c := cmp.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return cmp.Compare(a.Id, b.Id)
	})
	return gen.ListAssignableNodes200JSONResponse(out), nil
}

func assignableSortRank(n gen.AssignableNode) int {
	if n.Private {
		return 2
	}
	if n.Role == gen.AssignableNodeRoleProbe {
		return 1
	}
	return 0
}

// privateProbeCount returns the tenant's registered private probes plus open
// (unused, unexpired) private probe join tokens (ADR 0035, FA-PRB-05).
func (h *Handlers) privateProbeCount(ctx context.Context, tenantID string) (int, error) {
	probes, err := h.deps.State.CountTenantPrivateProbes(ctx, tenantID)
	if err != nil {
		return 0, err
	}
	openTokens, err := h.deps.State.CountOpenTenantJoinTokens(ctx, tenantID, h.deps.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	return probes + openTokens, nil
}

// checkPrivateProbeLimit pre-checks maxPrivateProbes before submitting a join
// token request so exceeded limits fail fast with 403; the FSM re-checks under Raft.
func (h *Handlers) checkPrivateProbeLimit(ctx context.Context, tenantID string) error {
	tn, err := h.deps.State.GetTenant(ctx, tenantID)
	if err != nil {
		h.logLimitLookup(tenantID, err)
		return nil
	}
	plan, err := h.deps.State.GetPlan(ctx, tn.Plan)
	if err != nil {
		h.logLimitLookup(tenantID, err)
		return nil
	}
	planLimits, err := tenant.ParseLimits(plan.Limits)
	if err != nil {
		h.logLimitLookup(tenantID, err)
		return nil
	}
	overrides, err := tenant.ParseOverrides(tn.Limits)
	if err != nil {
		h.logLimitLookup(tenantID, err)
		return nil
	}
	eff := tenant.Effective(planLimits, overrides)
	if eff.MaxPrivateProbes == nil {
		return nil
	}
	used, err := h.privateProbeCount(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("count private probes of tenant %s: %w", tenantID, err)
	}
	if used >= *eff.MaxPrivateProbes {
		return limitProblem(&cluster.CommandError{
			Code:    cluster.CodeLimitExceeded,
			Message: fmt.Sprintf("private probe limit reached: plan %q allows %d private probes", tn.Plan, *eff.MaxPrivateProbes),
		})
	}
	return nil
}
