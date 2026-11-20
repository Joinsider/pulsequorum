// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/joinsider/pulsequorum/internal/api/gen"
	"github.com/joinsider/pulsequorum/internal/auth"
	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/maintenance"
	"github.com/joinsider/pulsequorum/internal/store"
)

// Maintenance windows (FA-MNT-01). The schedule is compiled by
// internal/maintenance; the cluster FSM checks only the structure.

const (
	// maxMaintenanceMinutes is the longest allowed window per occurrence (one week).
	maxMaintenanceMinutes = 7 * 24 * 60
	// defaultTimezone applies to recurring schedules without a time zone.
	defaultTimezone = "UTC"
)

// ListMaintenanceWindows returns the maintenance windows of the caller's tenant.
func (h *Handlers) ListMaintenanceWindows(ctx context.Context, _ gen.ListMaintenanceWindowsRequestObject) (gen.ListMaintenanceWindowsResponseObject, error) {
	p, err := principal(ctx, readAccess)
	if err != nil {
		return nil, err
	}
	ws, err := h.deps.State.ListMaintenanceWindows(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	now := h.deps.Now()
	out := make(gen.ListMaintenanceWindows200JSONResponse, 0, len(ws))
	for _, w := range ws {
		api, err := apiMaintenance(w, now)
		if err != nil {
			return nil, err
		}
		out = append(out, api)
	}
	return out, nil
}

// CreateMaintenanceWindow validates the input and creates the window through Raft.
func (h *Handlers) CreateMaintenanceWindow(ctx context.Context, req gen.CreateMaintenanceWindowRequestObject) (gen.CreateMaintenanceWindowResponseObject, error) {
	p, err := principal(ctx, editAccess)
	if err != nil {
		return nil, err
	}
	doc, err := h.maintenanceDocument(ctx, p.TenantID, req.Body)
	if err != nil {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	cmd, err := cluster.NewPutMaintenanceWindow(requestID(p.TenantID, req.Params.IdempotencyKey), p.TenantID, id.String(), doc, 0)
	if err != nil {
		return nil, err
	}
	res, err := h.apply(ctx, cmd)
	if err != nil {
		return nil, err
	}
	// A deduplicated retry returns the ID of the first attempt.
	var put cluster.PutResult
	if err := json.Unmarshal(res.Result, &put); err != nil {
		return nil, fmt.Errorf("decode put result: %w", err)
	}
	w, err := h.deps.State.GetMaintenanceWindow(ctx, p.TenantID, put.ID)
	if err != nil {
		return nil, err
	}
	out, err := apiMaintenance(w, h.deps.Now())
	if err != nil {
		return nil, err
	}
	return gen.CreateMaintenanceWindow201JSONResponse(out), nil
}

// GetMaintenanceWindow returns one window of the caller's tenant.
func (h *Handlers) GetMaintenanceWindow(ctx context.Context, req gen.GetMaintenanceWindowRequestObject) (gen.GetMaintenanceWindowResponseObject, error) {
	p, err := principal(ctx, readAccess)
	if err != nil {
		return nil, err
	}
	w, err := h.maintenanceWindow(ctx, p, req.MaintenanceId)
	if err != nil {
		return nil, err
	}
	out, err := apiMaintenance(w, h.deps.Now())
	if err != nil {
		return nil, err
	}
	return gen.GetMaintenanceWindow200JSONResponse(out), nil
}

// UpdateMaintenanceWindow replaces the window's settings. The write is
// conditional on the revision that was read just before (ErrConflict otherwise).
func (h *Handlers) UpdateMaintenanceWindow(ctx context.Context, req gen.UpdateMaintenanceWindowRequestObject) (gen.UpdateMaintenanceWindowResponseObject, error) {
	p, err := principal(ctx, editAccess)
	if err != nil {
		return nil, err
	}
	current, err := h.maintenanceWindow(ctx, p, req.MaintenanceId)
	if err != nil {
		return nil, err
	}
	doc, err := h.maintenanceDocument(ctx, p.TenantID, req.Body)
	if err != nil {
		return nil, err
	}
	cmd, err := cluster.NewPutMaintenanceWindow(uuid.NewString(), p.TenantID, current.ID, doc, current.Revision)
	if err != nil {
		return nil, err
	}
	if _, err := h.apply(ctx, cmd); err != nil {
		return nil, err
	}
	w, err := h.deps.State.GetMaintenanceWindow(ctx, p.TenantID, current.ID)
	if err != nil {
		return nil, err
	}
	out, err := apiMaintenance(w, h.deps.Now())
	if err != nil {
		return nil, err
	}
	return gen.UpdateMaintenanceWindow200JSONResponse(out), nil
}

// DeleteMaintenanceWindow removes the window. Alerts and uptime accounting
// return to normal from the next evaluation on.
func (h *Handlers) DeleteMaintenanceWindow(ctx context.Context, req gen.DeleteMaintenanceWindowRequestObject) (gen.DeleteMaintenanceWindowResponseObject, error) {
	p, err := principal(ctx, editAccess)
	if err != nil {
		return nil, err
	}
	current, err := h.maintenanceWindow(ctx, p, req.MaintenanceId)
	if err != nil {
		return nil, err
	}
	if _, err := h.apply(ctx, cluster.NewDeleteMaintenanceWindow(uuid.NewString(), p.TenantID, current.ID)); err != nil {
		return nil, err
	}
	return gen.DeleteMaintenanceWindow204Response{}, nil
}

// maintenanceWindow loads a window of the caller's tenant; other tenants'
// windows are reported as not found (FA-TEN-02).
func (h *Handlers) maintenanceWindow(ctx context.Context, p auth.Principal, id uuid.UUID) (store.MaintenanceWindow, error) {
	w, err := h.deps.State.GetMaintenanceWindow(ctx, p.TenantID, id.String())
	if errors.Is(err, store.ErrNotFound) {
		return store.MaintenanceWindow{}, notFound("maintenance window")
	}
	return w, err
}

// maintenanceDocument validates the input and fills defaults (FA-MNT-01).
// Field errors are reported together as 422.
func (h *Handlers) maintenanceDocument(ctx context.Context, tenantID string, in *gen.MaintenanceWindowInput) (cluster.MaintenanceDocument, error) {
	if in == nil {
		return cluster.MaintenanceDocument{}, &Problem{Status: http.StatusBadRequest, Title: "Bad Request", Detail: "request body is required"}
	}
	var fields []checker.FieldError
	add := func(field, msg string) { fields = append(fields, checker.FieldError{Field: field, Message: msg}) }

	doc := cluster.MaintenanceDocument{Name: strings.TrimSpace(in.Name), Active: true}
	if doc.Name == "" || len([]rune(doc.Name)) > maxNameLength {
		add("/name", fmt.Sprintf("must be 1 to %d characters", maxNameLength))
	}
	if in.Description != nil {
		doc.Description = *in.Description
		if len([]rune(doc.Description)) > maxDescription {
			add("/description", fmt.Sprintf("must be at most %d characters", maxDescription))
		}
	}
	if in.Active != nil {
		doc.Active = *in.Active
	}

	ids := make([]string, 0, len(in.MonitorIds))
	for _, id := range in.MonitorIds {
		ids = append(ids, id.String())
	}
	monitorIDs, idFields, err := h.maintenanceMonitorIDs(ctx, tenantID, ids)
	if err != nil {
		return cluster.MaintenanceDocument{}, err
	}
	fields = append(fields, idFields...)
	doc.MonitorIDs = monitorIDs

	sched, schedFields := maintenanceSchedule(in.Schedule)
	fields = append(fields, schedFields...)
	if len(schedFields) == 0 {
		if _, err := maintenance.Compile(sched); err != nil {
			add("/schedule", err.Error())
		}
	}
	doc.Schedule = sched

	if len(fields) > 0 {
		return cluster.MaintenanceDocument{}, &Problem{Status: http.StatusUnprocessableEntity, Title: "Validation Failed",
			Detail: "the maintenance window is invalid", Fields: fields}
	}
	return doc, nil
}

// maintenanceSchedule converts the schedule input to the stored form and
// checks the fields that depend on the kind. The cron expression and the
// bounds are checked later by maintenance.Compile.
func maintenanceSchedule(in gen.MaintenanceSchedule) (store.MaintenanceSchedule, []checker.FieldError) {
	var fields []checker.FieldError
	add := func(field, msg string) { fields = append(fields, checker.FieldError{Field: field, Message: msg}) }

	s := store.MaintenanceSchedule{Kind: string(in.Kind)}
	if in.StartsAt != nil {
		s.StartsMs = in.StartsAt.UnixMilli()
	}
	if in.EndsAt != nil {
		s.EndsMs = in.EndsAt.UnixMilli()
	}
	reversed := in.StartsAt != nil && in.EndsAt != nil && !in.EndsAt.After(*in.StartsAt)

	switch in.Kind {
	case gen.Once:
		if in.StartsAt == nil {
			add("/schedule/startsAt", "required for kind once")
		}
		switch {
		case in.EndsAt == nil:
			add("/schedule/endsAt", "required for kind once")
		case reversed:
			add("/schedule/endsAt", "must be after startsAt")
		}
		if in.Cron != nil {
			add("/schedule/cron", "not allowed for kind once")
		}
		if in.DurationMinutes != nil {
			add("/schedule/durationMinutes", "not allowed for kind once")
		}
	case gen.Recurring:
		switch {
		case in.Cron == nil || strings.TrimSpace(*in.Cron) == "":
			add("/schedule/cron", "required for kind recurring")
		default:
			s.Cron = strings.TrimSpace(*in.Cron)
		}
		switch {
		case in.DurationMinutes == nil:
			add("/schedule/durationMinutes", "required for kind recurring")
		case *in.DurationMinutes < 1 || *in.DurationMinutes > maxMaintenanceMinutes:
			add("/schedule/durationMinutes", fmt.Sprintf("must be between 1 and %d", maxMaintenanceMinutes))
		default:
			s.DurationMinutes = *in.DurationMinutes
		}
		if reversed {
			add("/schedule/endsAt", "must be after startsAt")
		}
	default:
		add("/schedule/kind", "must be once or recurring")
	}

	if in.Timezone != nil && strings.TrimSpace(*in.Timezone) != "" {
		s.Timezone = strings.TrimSpace(*in.Timezone)
		if _, err := time.LoadLocation(s.Timezone); err != nil {
			add("/schedule/timezone", "unknown time zone "+s.Timezone)
		}
	} else if in.Kind == gen.Recurring {
		s.Timezone = defaultTimezone
	}
	return s, fields
}

// apiMaintenance converts a window and computes whether it is active now and
// when it starts next. Inactive windows report no next start.
func apiMaintenance(w store.MaintenanceWindow, now time.Time) (gen.MaintenanceWindow, error) {
	sched, err := maintenance.Compile(w.Schedule)
	if err != nil {
		return gen.MaintenanceWindow{}, fmt.Errorf("stored maintenance window %s: %w", w.ID, err)
	}
	id, err := uuid.Parse(w.ID)
	if err != nil {
		return gen.MaintenanceWindow{}, fmt.Errorf("stored maintenance window: %w", err)
	}
	monitorIDs := make([]openapi_types.UUID, 0, len(w.MonitorIDs))
	for _, m := range w.MonitorIDs {
		mid, err := uuid.Parse(m)
		if err != nil {
			return gen.MaintenanceWindow{}, fmt.Errorf("stored maintenance window %s: monitor ID: %w", w.ID, err)
		}
		monitorIDs = append(monitorIDs, mid)
	}

	inProgress := false
	var nextStart *time.Time
	if w.Active {
		_, _, inProgress = sched.Occurrence(now)
		if next, ok := sched.NextStart(now); ok {
			nextStart = &next
		}
	}
	active := w.Active
	description := w.Description
	return gen.MaintenanceWindow{
		Id:          id,
		Name:        w.Name,
		Description: &description,
		Active:      &active,
		MonitorIds:  monitorIDs,
		Schedule:    apiMaintenanceSchedule(w.Schedule),
		InProgress:  &inProgress,
		NextStart:   nextStart,
		CreatedAt:   time.UnixMilli(w.CreatedMs).UTC(),
	}, nil
}

func apiMaintenanceSchedule(s store.MaintenanceSchedule) gen.MaintenanceSchedule {
	out := gen.MaintenanceSchedule{Kind: gen.MaintenanceScheduleKind(s.Kind)}
	if s.StartsMs != 0 {
		at := time.UnixMilli(s.StartsMs).UTC()
		out.StartsAt = &at
	}
	if s.EndsMs != 0 {
		at := time.UnixMilli(s.EndsMs).UTC()
		out.EndsAt = &at
	}
	if s.Kind == store.ScheduleRecurring {
		cron := s.Cron
		out.Cron = &cron
		duration := s.DurationMinutes
		out.DurationMinutes = &duration
	}
	if s.Timezone != "" {
		tz := s.Timezone
		out.Timezone = &tz
	}
	return out
}
