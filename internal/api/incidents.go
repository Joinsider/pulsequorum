// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/uptimecluster/internal/api/gen"
	"github.com/joinsider/uptimecluster/internal/store"
)

// ListIncidents returns the caller's incidents, newest first (FA-INC-02).
func (h *Handlers) ListIncidents(ctx context.Context, req gen.ListIncidentsRequestObject) (gen.ListIncidentsResponseObject, error) {
	p, err := principal(ctx, readAccess)
	if err != nil {
		return nil, err
	}
	q := store.IncidentQuery{}
	if req.Params.MonitorId != nil {
		q.MonitorID = req.Params.MonitorId.String()
	}
	if req.Params.From != nil {
		q.FromMs = req.Params.From.UnixMilli()
	}
	if req.Params.To != nil {
		q.ToMs = req.Params.To.UnixMilli()
	}
	if req.Params.Open != nil {
		q.OpenOnly = *req.Params.Open
	}
	if req.Params.Limit != nil {
		q.Limit = *req.Params.Limit
	}
	if req.Params.Cursor != nil {
		raw, err := decodeCursor(*req.Params.Cursor, "i")
		if err != nil {
			return nil, err
		}
		var c store.IncidentCursor
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			return nil, &Problem{Status: http.StatusBadRequest, Title: "Bad Request", Detail: "invalid cursor"}
		}
		q.After = &c
	}
	page, err := h.deps.State.ListIncidents(ctx, p.TenantID, q)
	if err != nil {
		return nil, err
	}
	out := gen.IncidentPage{Items: make([]gen.Incident, 0, len(page.Items))}
	for _, inc := range page.Items {
		out.Items = append(out.Items, toAPIIncident(inc))
	}
	if page.Next != nil {
		raw, _ := json.Marshal(page.Next)
		c := encodeCursor("i", string(raw))
		out.NextCursor = &c
	}
	return gen.ListIncidents200JSONResponse(out), nil
}

// GetIncident returns one incident of the caller's tenant.
func (h *Handlers) GetIncident(ctx context.Context, req gen.GetIncidentRequestObject) (gen.GetIncidentResponseObject, error) {
	p, err := principal(ctx, readAccess)
	if err != nil {
		return nil, err
	}
	inc, err := h.deps.State.GetIncident(ctx, p.TenantID, req.IncidentId.String())
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound("incident")
	}
	if err != nil {
		return nil, err
	}
	return gen.GetIncident200JSONResponse(toAPIIncident(inc)), nil
}

func toAPIIncident(inc store.Incident) gen.Incident {
	id, _ := uuid.Parse(inc.ID)
	monitorID, _ := uuid.Parse(inc.MonitorID)
	nodes := inc.AffectedNodes
	if nodes == nil {
		nodes = []string{}
	}
	out := gen.Incident{
		Id:            id,
		MonitorId:     monitorID,
		MonitorName:   inc.MonitorName,
		StartedAt:     time.UnixMilli(inc.StartedMs).UTC(),
		Cause:         inc.Cause,
		AffectedNodes: &nodes,
		Confirmed:     &inc.Confirmed,
		Reevaluated:   &inc.Reevaluated,
	}
	if inc.ResolvedMs != 0 {
		at := time.UnixMilli(inc.ResolvedMs).UTC()
		out.ResolvedAt = &at
	}
	if inc.ErrorClass != "" {
		ec := gen.ErrorClass(inc.ErrorClass)
		out.ErrorClass = &ec
	}
	return out
}
