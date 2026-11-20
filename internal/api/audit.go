// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/joinsider/pulsequorum/internal/api/gen"
	"github.com/joinsider/pulsequorum/internal/auth"
	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/tenant"
)

// The audit log (FA-DAT-01, ADR 0031). Tenant admins read their own tenant,
// platform admins all tenants. Entries are newest first and paged by a cursor
// over (index, sub).

const (
	auditDefaultLimit = 50
	auditMaxLimit     = 200
	// auditCursorKind tags the cursors of the audit log (see encodeCursor).
	auditCursorKind = "a"
)

// auditFilter holds the query parameters shared by both audit operations.
type auditFilter struct {
	cursor     *string
	limit      *int
	from, to   *time.Time
	action     *string
	objectKind *string
	objectID   *string
	actor      *string
}

// ListAudit returns the audit log of the caller's tenant. API tokens with the
// role tenant-admin may read it (ADR 0031).
func (h *Handlers) ListAudit(ctx context.Context, req gen.ListAuditRequestObject) (gen.ListAuditResponseObject, error) {
	p, err := principal(ctx, adminAccess)
	if err != nil {
		return nil, err
	}
	out, err := h.auditPage(ctx, p.TenantID, auditFilter{
		cursor:     req.Params.Cursor,
		limit:      req.Params.Limit,
		from:       req.Params.From,
		to:         req.Params.To,
		action:     req.Params.Action,
		objectKind: req.Params.ObjectKind,
		objectID:   req.Params.ObjectId,
		actor:      req.Params.Actor,
	})
	if err != nil {
		return nil, err
	}
	return gen.ListAudit200JSONResponse(out), nil
}

// ListAdminAudit returns the audit log of all tenants, including the platform
// actions of the system tenant, or of the tenant named by the filter.
func (h *Handlers) ListAdminAudit(ctx context.Context, req gen.ListAdminAuditRequestObject) (gen.ListAdminAuditResponseObject, error) {
	p, err := principal(ctx, platformAccess)
	if err != nil {
		return nil, err
	}
	if err := auth.RequireInteractive(p); err != nil {
		return nil, &Problem{Status: http.StatusForbidden, Title: "Forbidden", Detail: err.Error()}
	}
	tenantID := ""
	if req.Params.Tenant != nil {
		tenantID = *req.Params.Tenant
		if !checkAuditTenant(tenantID) {
			return nil, validationProblem([]checker.FieldError{fieldError("/tenant", "must be a tenant ID or _system")}, "the audit filter is invalid")
		}
	}
	out, err := h.auditPage(ctx, tenantID, auditFilter{
		cursor:     req.Params.Cursor,
		limit:      req.Params.Limit,
		from:       req.Params.From,
		to:         req.Params.To,
		action:     req.Params.Action,
		objectKind: req.Params.ObjectKind,
		objectID:   req.Params.ObjectId,
		actor:      req.Params.Actor,
	})
	if err != nil {
		return nil, err
	}
	return gen.ListAdminAudit200JSONResponse(out), nil
}

// auditPage loads one page of the audit log. An empty tenantID lists all
// tenants; the callers decide who may ask for that.
func (h *Handlers) auditPage(ctx context.Context, tenantID string, f auditFilter) (gen.AuditPage, error) {
	q := store.AuditQuery{TenantID: tenantID, Limit: auditDefaultLimit}
	if f.limit != nil {
		if *f.limit < 1 || *f.limit > auditMaxLimit {
			return gen.AuditPage{}, validationProblem([]checker.FieldError{fieldError("/limit", "must be between 1 and 200")}, "the audit query is invalid")
		}
		q.Limit = *f.limit
	}
	if f.cursor != nil {
		c, err := decodeAuditCursor(*f.cursor)
		if err != nil {
			return gen.AuditPage{}, err
		}
		q.After = &c
	}
	if f.from != nil {
		q.FromMs = f.from.UnixMilli()
	}
	if f.to != nil {
		q.ToMs = f.to.UnixMilli()
	}
	if f.action != nil {
		q.Action = *f.action
	}
	if f.objectKind != nil {
		q.ObjectKind = *f.objectKind
	}
	if f.objectID != nil {
		q.ObjectID = *f.objectID
	}
	if f.actor != nil {
		q.ActorID = *f.actor
	}
	page, err := h.deps.State.ListAudit(ctx, q)
	if err != nil {
		return gen.AuditPage{}, err
	}
	out := gen.AuditPage{Items: make([]gen.AuditEntry, 0, len(page.Entries))}
	for _, e := range page.Entries {
		out.Items = append(out.Items, apiAuditEntry(e))
	}
	if page.Next != nil {
		c := encodeAuditCursor(*page.Next)
		out.NextCursor = &c
	}
	return out, nil
}

func apiAuditEntry(e store.AuditEntry) gen.AuditEntry {
	return gen.AuditEntry{
		At:         time.UnixMilli(e.AtMs).UTC(),
		TenantId:   e.TenantID,
		Actor:      gen.AuditActor{Kind: gen.AuditActorKind(e.ActorKind), Id: e.ActorID, Name: e.ActorName},
		Action:     e.Action,
		ObjectKind: e.ObjectKind,
		ObjectId:   e.ObjectID,
		RequestId:  e.RequestID,
	}
}

// checkAuditTenant reports whether a tenant ID may filter the admin audit log:
// a valid tenant ID or the system tenant.
func checkAuditTenant(id string) bool {
	return id == store.SystemTenantID || tenant.ValidID(id)
}

// encodeAuditCursor encodes the position of an entry as an opaque cursor.
func encodeAuditCursor(c store.AuditCursor) string {
	return encodeCursor(auditCursorKind, fmt.Sprintf("%d.%d", c.Index, c.Sub))
}

// decodeAuditCursor reads a cursor of the audit log. Malformed cursors are a
// 400, like those of the other listings.
func decodeAuditCursor(cursor string) (store.AuditCursor, error) {
	raw, err := decodeCursor(cursor, auditCursorKind)
	if err != nil {
		return store.AuditCursor{}, err
	}
	idx, sub, ok := strings.Cut(raw, ".")
	index, errIdx := strconv.ParseUint(idx, 10, 64)
	position, errSub := strconv.Atoi(sub)
	if !ok || errIdx != nil || errSub != nil || position < 0 {
		return store.AuditCursor{}, &Problem{Status: http.StatusBadRequest, Title: "Bad Request", Detail: "invalid cursor"}
	}
	return store.AuditCursor{Index: index, Sub: position}, nil
}
