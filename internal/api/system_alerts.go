// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/joinsider/uptimecluster/internal/api/gen"
)

// ListSystemAlerts returns the replicated state of the system alerts (FA-OBS-01,
// ADR 0025). Platform admins only. The query active=true (default) returns only
// the alerts that are active now.
func (h *Handlers) ListSystemAlerts(ctx context.Context, req gen.ListSystemAlertsRequestObject) (gen.ListSystemAlertsResponseObject, error) {
	if _, err := principal(ctx, platformAccess); err != nil {
		return nil, err
	}
	activeOnly := true
	if req.Params.Active != nil {
		activeOnly = *req.Params.Active
	}
	alerts, err := h.deps.State.ListSystemAlerts(ctx, activeOnly)
	if err != nil {
		return nil, err
	}
	out := make(gen.ListSystemAlerts200JSONResponse, 0, len(alerts))
	for _, a := range alerts {
		var details map[string]any
		if len(a.DetailsJSON) > 0 {
			if err := json.Unmarshal(a.DetailsJSON, &details); err != nil {
				return nil, fmt.Errorf("decode details of system alert %s/%s: %w", a.Kind, a.Subject, err)
			}
		}
		if details == nil {
			details = map[string]any{}
		}
		out = append(out, gen.SystemAlert{
			Kind:    a.Kind,
			Subject: a.Subject,
			Active:  a.Active,
			//nolint:gosec // G115: levels are small counters written by the leader (ADR 0025)
			Level:   int32(a.Level),
			Since:   time.UnixMilli(a.SinceMs).UTC(),
			Updated: time.UnixMilli(a.UpdatedMs).UTC(),
			Details: details,
		})
	}
	return out, nil
}
