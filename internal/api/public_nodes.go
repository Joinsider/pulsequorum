// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"context"

	"github.com/joinsider/pulsequorum/internal/api/gen"
)

// ListPublicNodes lists the voters with a public origin for client failover
// (ADR 0036). The operation is public and rate limited like /health; the
// response carries only ID, origin and health, never names, versions or
// addresses. The list is never null.
func (h *Handlers) ListPublicNodes(ctx context.Context, _ gen.ListPublicNodesRequestObject) (gen.ListPublicNodesResponseObject, error) {
	nodes := h.deps.PublicNodes.Nodes(ctx)
	out := make([]gen.PublicNode, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, gen.PublicNode{Id: n.ID, Url: n.URL, Voter: true, Healthy: n.Healthy})
	}
	return gen.ListPublicNodes200JSONResponse{Nodes: out}, nil
}
