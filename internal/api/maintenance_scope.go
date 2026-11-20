// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"context"
	"errors"
	"slices"

	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/store"
)

// maintenanceMonitorIDs enforces the tenant boundary of a maintenance window
// (FA-TEN-01): every monitor ID must belong to the caller's tenant. Unknown or
// foreign IDs are reported as field errors without revealing which of them
// exist in another tenant. The result is sorted and de-duplicated.
func (h *Handlers) maintenanceMonitorIDs(ctx context.Context, tenantID string, ids []string) ([]string, []checker.FieldError, error) {
	out := make([]string, 0, len(ids))
	var fields []checker.FieldError
	for _, id := range ids {
		if slices.Contains(out, id) {
			continue
		}
		_, err := h.deps.State.GetMonitor(ctx, tenantID, id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			fields = append(fields, checker.FieldError{Field: "/monitorIds", Message: "unknown monitor " + id})
		case err != nil:
			return nil, nil, err
		default:
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out, fields, nil
}
