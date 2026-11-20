// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/joinsider/pulsequorum/internal/api/gen"
	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/cluster"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
)

// Bulk monitor actions (FA-MON-06, ADR 0031). The API builds one document per
// monitor, as the single operations do, and proposes them as one Batch: all of
// them apply or none does. A monitor that is already in the requested state
// is not part of the batch and reports its current revision.

// bulkMaxIDs is the largest number of monitors of one bulk action.
const bulkMaxIDs = 500

// BulkMonitors applies one action to several monitors of the caller's tenant.
func (h *Handlers) BulkMonitors(ctx context.Context, req gen.BulkMonitorsRequestObject) (gen.BulkMonitorsResponseObject, error) {
	p, err := principal(ctx, editAccess)
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, requestBodyRequired()
	}
	in := req.Body
	action := in.Action
	ids := in.Ids
	tags := []string{}
	if in.Tags != nil {
		tags = *in.Tags
	}

	var fields []checker.FieldError
	if !action.Valid() {
		fields = append(fields, fieldError("/action", "must be pause, resume, delete, addTags or removeTags"))
	}
	if n := len(ids); n < 1 || n > bulkMaxIDs {
		fields = append(fields, fieldError("/ids", fmt.Sprintf("must contain 1 to %d monitors", bulkMaxIDs)))
	}
	if action == gen.AddTags || action == gen.RemoveTags {
		fields = append(fields, bulkTagFields(tags)...)
	}
	if err := validationProblem(fields, "the bulk action is invalid"); err != nil {
		return nil, err
	}

	seen := make(map[string]bool, len(ids))
	monitors := make([]store.Monitor, len(ids))
	for i, id := range ids {
		if seen[id.String()] {
			fields = append(fields, fieldError(fmt.Sprintf("/ids/%d", i), "duplicate monitor"))
			continue
		}
		seen[id.String()] = true
		m, err := h.deps.State.GetMonitor(ctx, p.TenantID, id.String())
		if errors.Is(err, store.ErrNotFound) {
			fields = append(fields, fieldError(fmt.Sprintf("/ids/%d", i), "unknown monitor"))
			continue
		}
		if err != nil {
			return nil, err
		}
		monitors[i] = m
	}
	if err := validationProblem(fields, "the bulk action cannot be applied"); err != nil {
		return nil, err
	}

	// opOf maps each monitor to its operation in the batch; -1 if it needs no change.
	ops := make([]*pb.BatchOp, 0, len(ids))
	opOf := make([]int, len(ids))
	for i, m := range monitors {
		if action == gen.Delete {
			opOf[i] = len(ops)
			ops = append(ops, bulkDeleteOp(m.ID))
			continue
		}
		doc, hint, changed := bulkChange(m, action, tags)
		if !changed {
			opOf[i] = -1
			continue
		}
		if action == gen.AddTags && len(doc.Tags) > maxTags {
			fields = append(fields, fieldError(fmt.Sprintf("/ids/%d", i), fmt.Sprintf("the monitor would have more than %d tags", maxTags)))
			continue
		}
		cmd, err := cluster.NewPutMonitor("", p.TenantID, m.ID, doc, m.Revision)
		if err != nil {
			return nil, err
		}
		opOf[i] = len(ops)
		ops = append(ops, &pb.BatchOp{Op: &pb.BatchOp_PutEntity{PutEntity: cmd.GetPutEntity()}, AuditAction: hint})
	}
	if err := validationProblem(fields, "the bulk action cannot be applied"); err != nil {
		return nil, err
	}

	var applied cluster.BatchResult
	if len(ops) > 0 {
		res, err := h.apply(ctx, cluster.NewBatch(requestID(p.TenantID, req.Params.IdempotencyKey), p.TenantID, nil, ops))
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(res.Result, &applied); err != nil {
			return nil, fmt.Errorf("decode batch result: %w", err)
		}
		if len(applied.Results) != len(ops) {
			return nil, fmt.Errorf("batch returned %d results for %d operations", len(applied.Results), len(ops))
		}
	}

	out := make([]gen.BulkMonitorResult, 0, len(ids))
	for i, id := range ids {
		r := gen.BulkMonitorResult{Id: id}
		switch {
		case action == gen.Delete:
			// Deleted monitors have no revision.
		case opOf[i] < 0:
			rev := int(monitors[i].Revision) //nolint:gosec // G115: revisions count changes, far below the int range
			r.Revision = &rev
		default:
			rev := int(applied.Results[opOf[i]].Revision) //nolint:gosec // G115: revisions count changes, far below the int range
			r.Revision = &rev
		}
		out = append(out, r)
	}
	return gen.BulkMonitors200JSONResponse(gen.BulkMonitorsResult{Results: out}), nil
}

// bulkTagFields checks the tags of addTags and removeTags with the rules of a
// single monitor update.
func bulkTagFields(tags []string) []checker.FieldError {
	if len(tags) == 0 {
		return []checker.FieldError{fieldError("/tags", "required for addTags and removeTags")}
	}
	var fields []checker.FieldError
	if len(tags) > maxTags {
		fields = append(fields, fieldError("/tags", fmt.Sprintf("at most %d tags", maxTags)))
	}
	for i, t := range tags {
		if t == "" || len([]rune(t)) > maxTagLength {
			fields = append(fields, fieldError(fmt.Sprintf("/tags/%d", i), fmt.Sprintf("must be 1 to %d characters", maxTagLength)))
		}
	}
	return fields
}

// bulkChange returns the document of m after action and the audit hint of the
// change. changed is false if m is already in the requested state.
func bulkChange(m store.Monitor, action gen.BulkMonitorsInputAction, tags []string) (doc cluster.MonitorDocument, hint string, changed bool) {
	switch action {
	case gen.Pause, gen.Resume:
		paused := action == gen.Pause
		if m.Paused == paused {
			return doc, "", false
		}
		doc = cluster.MonitorDocumentFrom(m)
		doc.Paused = paused
		if paused {
			return doc, "pause", true
		}
		return doc, "resume", true
	case gen.AddTags:
		merged := slices.Clone(m.Tags)
		for _, t := range tags {
			if !slices.Contains(merged, t) {
				merged = append(merged, t)
			}
		}
		if len(merged) == len(m.Tags) {
			return doc, "", false
		}
		doc = cluster.MonitorDocumentFrom(m)
		doc.Tags = merged
		return doc, "tags", true
	case gen.RemoveTags:
		kept := slices.DeleteFunc(slices.Clone(m.Tags), func(t string) bool { return slices.Contains(tags, t) })
		if len(kept) == len(m.Tags) {
			return doc, "", false
		}
		doc = cluster.MonitorDocumentFrom(m)
		doc.Tags = kept
		return doc, "tags", true
	}
	return doc, "", false
}

// bulkDeleteOp deletes one monitor within a batch.
func bulkDeleteOp(monitorID string) *pb.BatchOp {
	return &pb.BatchOp{Op: &pb.BatchOp_DeleteEntity{DeleteEntity: &pb.DeleteEntity{
		Kind: pb.EntityKind_ENTITY_KIND_MONITOR, Id: monitorID,
	}}}
}
