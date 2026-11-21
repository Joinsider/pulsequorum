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

	"github.com/joinsider/pulsequorum/internal/api/gen"
	"github.com/joinsider/pulsequorum/internal/auth"
	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/cluster"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/tenant"
)

// Tenants and plans (FA-TEN-03, FA-TEN-04, ADR 0029). The platform admin manages
// them under /admin; GET /tenant shows the caller's own tenant. The reserved
// tenant _system and the reserved plan system are never listed or served, and
// the API checks the rules before proposing so that forms get field errors.
// The FSM checks them again and decides.
//
// The commands are built without a version: Node.Apply sets it from the payload.

const (
	tenantIDRule   = "must be 1 to 64 characters of a-z, 0-9 and '-'"
	tenantNameRule = "must be 1 to 100 characters without control characters or surrounding spaces"
	fieldLimits    = "/limits"
)

// ListPlans returns the plans with the number of tenants on each.
func (h *Handlers) ListPlans(ctx context.Context, _ gen.ListPlansRequestObject) (gen.ListPlansResponseObject, error) {
	if _, err := principal(ctx, platformAccess); err != nil {
		return nil, err
	}
	plans, err := h.deps.State.ListPlans(ctx)
	if err != nil {
		return nil, err
	}
	counts, err := h.planCounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]gen.Plan, 0, len(plans))
	for _, p := range plans {
		if p.ID == tenant.SystemPlanID {
			continue
		}
		api, err := apiPlan(p, counts[p.ID])
		if err != nil {
			return nil, err
		}
		out = append(out, api)
	}
	return gen.ListPlans200JSONResponse(out), nil
}

// GetPlan returns one plan. The system plan is not found.
func (h *Handlers) GetPlan(ctx context.Context, req gen.GetPlanRequestObject) (gen.GetPlanResponseObject, error) {
	if _, err := principal(ctx, platformAccess); err != nil {
		return nil, err
	}
	p, err := h.adminPlan(ctx, req.PlanId)
	if err != nil {
		return nil, err
	}
	counts, err := h.planCounts(ctx)
	if err != nil {
		return nil, err
	}
	out, err := apiPlan(p, counts[p.ID])
	if err != nil {
		return nil, err
	}
	return gen.GetPlan200JSONResponse(out), nil
}

// PutPlan creates or replaces a plan. A lowered limit keeps the stock of the
// tenants on the plan; only new objects and shorter intervals are rejected.
func (h *Handlers) PutPlan(ctx context.Context, req gen.PutPlanRequestObject) (gen.PutPlanResponseObject, error) {
	p, err := principal(ctx, platformAccess)
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, requestBodyRequired()
	}
	if req.PlanId == tenant.SystemPlanID {
		return nil, &Problem{Status: http.StatusUnprocessableEntity, Title: problemTitleUnprocessableEntity, Detail: "the system plan cannot be changed"}
	}
	var fields []checker.FieldError
	if !tenant.ValidID(req.PlanId) {
		fields = append(fields, fieldError("/planId", tenantIDRule))
	}
	if !tenant.ValidName(req.Body.Name) {
		fields = append(fields, fieldError("/name", tenantNameRule))
	}
	limits, err := planLimits(req.Body.Limits)
	if err != nil {
		fields = append(fields, fieldError(fieldLimits, limitMessage(err)))
	}
	if err := validationProblem(fields, "the plan is invalid"); err != nil {
		return nil, err
	}

	cmd := adminCommand(p, uuid.NewString())
	cmd.Payload = &pb.Command_PutPlan{PutPlan: &pb.PutPlan{
		PlanId: req.PlanId, Name: req.Body.Name, LimitsJson: string(limits.Canonical()),
	}}
	if _, err := h.apply(ctx, cmd); err != nil {
		return nil, err
	}
	saved, err := h.deps.State.GetPlan(ctx, req.PlanId)
	if err != nil {
		return nil, fmt.Errorf("read saved plan %s: %w", req.PlanId, err)
	}
	counts, err := h.planCounts(ctx)
	if err != nil {
		return nil, err
	}
	out, err := apiPlan(saved, counts[saved.ID])
	if err != nil {
		return nil, err
	}
	return gen.PutPlan200JSONResponse(out), nil
}

// DeletePlan deletes a plan that no tenant uses. The plans default and system
// cannot be deleted.
func (h *Handlers) DeletePlan(ctx context.Context, req gen.DeletePlanRequestObject) (gen.DeletePlanResponseObject, error) {
	p, err := principal(ctx, platformAccess)
	if err != nil {
		return nil, err
	}
	if _, err := h.adminPlan(ctx, req.PlanId); err != nil {
		return nil, err
	}
	if req.PlanId == tenant.DefaultPlanID {
		return nil, &Problem{Status: http.StatusUnprocessableEntity, Title: problemTitleUnprocessableEntity, Detail: "the default plan cannot be deleted"}
	}
	counts, err := h.planCounts(ctx)
	if err != nil {
		return nil, err
	}
	if n := counts[req.PlanId]; n > 0 {
		return nil, &Problem{Status: http.StatusConflict, Title: "Conflict",
			Detail: fmt.Sprintf("plan %s is used by %d tenants", req.PlanId, n)}
	}
	cmd := adminCommand(p, uuid.NewString())
	cmd.Payload = &pb.Command_DeletePlan{DeletePlan: &pb.DeletePlan{PlanId: req.PlanId}}
	if _, err := h.apply(ctx, cmd); err != nil {
		return nil, err
	}
	return gen.DeletePlan204Response{}, nil
}

// ListTenants returns the tenants with plan, limits and usage, the system
// tenant hidden.
func (h *Handlers) ListTenants(ctx context.Context, _ gen.ListTenantsRequestObject) (gen.ListTenantsResponseObject, error) {
	if _, err := principal(ctx, platformAccess); err != nil {
		return nil, err
	}
	tns, err := h.deps.State.ListTenants(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]gen.Tenant, 0, len(tns))
	for _, tn := range tns {
		if tn.ID == store.SystemTenantID {
			continue
		}
		v, err := h.viewTenant(ctx, tn)
		if err != nil {
			return nil, err
		}
		out = append(out, apiTenant(v))
	}
	return gen.ListTenants200JSONResponse(out), nil
}

// CreateTenant creates a tenant. Without an ID the server generates a UUID.
func (h *Handlers) CreateTenant(ctx context.Context, req gen.CreateTenantRequestObject) (gen.CreateTenantResponseObject, error) {
	p, err := principal(ctx, platformAccess)
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, requestBodyRequired()
	}
	in := req.Body
	var fields []checker.FieldError

	id := uuid.NewString()
	if in.Id != nil {
		id = *in.Id
		if !tenant.ValidID(id) {
			fields = append(fields, fieldError("/id", tenantIDRule))
		}
	}
	if !tenant.ValidName(in.Name) {
		fields = append(fields, fieldError("/name", tenantNameRule))
	}
	planID := tenant.DefaultPlanID
	if in.Plan != nil && *in.Plan != "" {
		planID = *in.Plan
	}
	msg, err := h.planFieldError(ctx, planID)
	if err != nil {
		return nil, err
	}
	if msg != "" {
		fields = append(fields, fieldError("/plan", msg))
	}
	orgID := ""
	if in.OrgId != nil {
		orgID = *in.OrgId
		if !tenant.ValidOrgID(orgID) {
			fields = append(fields, fieldError("/orgId", tenant.OrgIDRule))
		}
	}
	limits := "{}"
	if in.Limits != nil {
		s, err := overridesJSON(*in.Limits)
		if err != nil {
			fields = append(fields, fieldError(fieldLimits, limitMessage(err)))
		} else {
			limits = s
		}
	}
	if err := validationProblem(fields, "the tenant is invalid"); err != nil {
		return nil, err
	}

	cmd := adminCommand(p, requestID(p.TenantID, req.Params.IdempotencyKey))
	cmd.Payload = &pb.Command_CreateTenant{CreateTenant: &pb.CreateTenant{
		TenantId: id, Name: in.Name, PlanId: planID, OrgId: orgID, LimitsJson: limits,
		Source: tenant.SourceAdmin,
	}}
	tn, err := h.applyTenant(ctx, cmd)
	if err != nil {
		return nil, err
	}
	v, err := h.viewTenant(ctx, tn)
	if err != nil {
		return nil, err
	}
	return gen.CreateTenant201JSONResponse(apiTenant(v)), nil
}

// GetTenant returns one tenant. The system tenant is not found.
func (h *Handlers) GetTenant(ctx context.Context, req gen.GetTenantRequestObject) (gen.GetTenantResponseObject, error) {
	if _, err := principal(ctx, platformAccess); err != nil {
		return nil, err
	}
	tn, err := h.adminTenant(ctx, req.TenantId)
	if err != nil {
		return nil, err
	}
	v, err := h.viewTenant(ctx, tn)
	if err != nil {
		return nil, err
	}
	return gen.GetTenant200JSONResponse(apiTenant(v)), nil
}

// UpdateTenant changes the set fields of a tenant. Omitted fields stay as they
// are; limits replace all overrides.
func (h *Handlers) UpdateTenant(ctx context.Context, req gen.UpdateTenantRequestObject) (gen.UpdateTenantResponseObject, error) {
	p, err := principal(ctx, platformAccess)
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, requestBodyRequired()
	}
	current, err := h.adminTenant(ctx, req.TenantId)
	if err != nil {
		return nil, err
	}
	in := req.Body
	var fields []checker.FieldError
	upd := &pb.UpdateTenant{TenantId: current.ID}
	if in.Name != nil {
		if tenant.ValidName(*in.Name) {
			upd.Name = in.Name
		} else {
			fields = append(fields, fieldError("/name", tenantNameRule))
		}
	}
	if in.Plan != nil {
		msg, err := h.planFieldError(ctx, *in.Plan)
		if err != nil {
			return nil, err
		}
		if msg != "" {
			fields = append(fields, fieldError("/plan", msg))
		} else {
			upd.PlanId = in.Plan
		}
	}
	if in.Limits != nil {
		s, err := overridesJSON(*in.Limits)
		if err != nil {
			fields = append(fields, fieldError(fieldLimits, limitMessage(err)))
		} else {
			upd.LimitsJson = &s
		}
	}
	if in.OrgId != nil {
		// An empty organization ID unbinds the tenant.
		if *in.OrgId == "" || tenant.ValidOrgID(*in.OrgId) {
			upd.OrgId = in.OrgId
		} else {
			fields = append(fields, fieldError("/orgId", tenant.OrgIDRule))
		}
	}
	if err := validationProblem(fields, "the tenant change is invalid"); err != nil {
		return nil, err
	}

	cmd := adminCommand(p, uuid.NewString())
	cmd.Payload = &pb.Command_UpdateTenant{UpdateTenant: upd}
	tn, err := h.applyTenant(ctx, cmd)
	if err != nil {
		return nil, err
	}
	v, err := h.viewTenant(ctx, tn)
	if err != nil {
		return nil, err
	}
	return gen.UpdateTenant200JSONResponse(apiTenant(v)), nil
}

// SuspendTenant suspends a tenant (ADR 0029). Its users are rejected, its
// sessions revoked, its checks and alerts stop; the data is kept.
func (h *Handlers) SuspendTenant(ctx context.Context, req gen.SuspendTenantRequestObject) (gen.SuspendTenantResponseObject, error) {
	p, err := principal(ctx, platformAccess)
	if err != nil {
		return nil, err
	}
	out, err := h.setSuspended(ctx, p, req.TenantId, true)
	if err != nil {
		return nil, err
	}
	return gen.SuspendTenant200JSONResponse(out), nil
}

// ResumeTenant lifts the suspension of a tenant.
func (h *Handlers) ResumeTenant(ctx context.Context, req gen.ResumeTenantRequestObject) (gen.ResumeTenantResponseObject, error) {
	p, err := principal(ctx, platformAccess)
	if err != nil {
		return nil, err
	}
	out, err := h.setSuspended(ctx, p, req.TenantId, false)
	if err != nil {
		return nil, err
	}
	return gen.ResumeTenant200JSONResponse(out), nil
}

// setSuspended proposes SetTenantSuspended. The default and system tenants
// are never suspended.
func (h *Handlers) setSuspended(ctx context.Context, p auth.Principal, id string, suspended bool) (gen.Tenant, error) {
	if tenant.Protected(id) {
		return gen.Tenant{}, &Problem{Status: http.StatusUnprocessableEntity, Title: problemTitleUnprocessableEntity,
			Detail: fmt.Sprintf("tenant %s cannot be suspended or resumed", id)}
	}
	current, err := h.adminTenant(ctx, id)
	if err != nil {
		return gen.Tenant{}, err
	}
	cmd := adminCommand(p, uuid.NewString())
	cmd.Payload = &pb.Command_SetTenantSuspended{SetTenantSuspended: &pb.SetTenantSuspended{
		TenantId: current.ID, Suspended: suspended,
	}}
	tn, err := h.applyTenant(ctx, cmd)
	if err != nil {
		return gen.Tenant{}, err
	}
	v, err := h.viewTenant(ctx, tn)
	if err != nil {
		return gen.Tenant{}, err
	}
	return apiTenant(v), nil
}

// DeleteTenant deletes a suspended tenant with all its data (ADR 0029). The
// caller confirms the tenant name. The default and system tenants cannot be
// deleted.
func (h *Handlers) DeleteTenant(ctx context.Context, req gen.DeleteTenantRequestObject) (gen.DeleteTenantResponseObject, error) {
	p, err := principal(ctx, platformAccess)
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, requestBodyRequired()
	}
	current, err := h.adminTenant(ctx, req.TenantId)
	if err != nil {
		return nil, err
	}
	switch {
	case tenant.Protected(current.ID):
		return nil, &Problem{Status: http.StatusUnprocessableEntity, Title: problemTitleUnprocessableEntity,
			Detail: fmt.Sprintf("tenant %s cannot be deleted", current.ID)}
	case req.Body.ConfirmName != current.Name:
		return nil, &Problem{Status: http.StatusUnprocessableEntity, Title: problemTitleUnprocessableEntity,
			Detail: "confirmName does not match the tenant name"}
	case !current.Suspended:
		return nil, &Problem{Status: http.StatusConflict, Title: "Conflict", Detail: "suspend the tenant before deleting it"}
	}
	cmd := adminCommand(p, uuid.NewString())
	cmd.Payload = &pb.Command_DeleteTenant{DeleteTenant: &pb.DeleteTenant{TenantId: current.ID}}
	if _, err := h.apply(ctx, cmd); err != nil {
		return nil, err
	}
	return gen.DeleteTenant204Response{}, nil
}

// GetOwnTenant returns the caller's tenant with its effective limits and usage.
// The tenant comes from the token only (FA-TEN-02); the organization binding,
// the overrides and the source are not shown.
func (h *Handlers) GetOwnTenant(ctx context.Context, _ gen.GetOwnTenantRequestObject) (gen.GetOwnTenantResponseObject, error) {
	p, err := principal(ctx, readAccess)
	if err != nil {
		return nil, err
	}
	tn, err := h.deps.State.GetTenant(ctx, p.TenantID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound("tenant")
	}
	if err != nil {
		return nil, err
	}
	v, err := h.viewTenant(ctx, tn)
	if err != nil {
		return nil, err
	}
	return gen.GetOwnTenant200JSONResponse(ownTenant(v)), nil
}

// checkLimits pre-checks the plan limits of a tenant before a monitor or a
// channel is proposed (ADR 0029). The FSM checks them again and is authoritative.
// The system tenant has no limits. If the tenant or its plan cannot be read, the
// request is not blocked: the FSM decides.
func (h *Handlers) checkLimits(ctx context.Context, tenantID string, kind pb.EntityKind, create bool, interval, oldInterval int) error {
	if tenantID == store.SystemTenantID {
		return nil
	}
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
	usage, err := h.deps.State.TenantUsage(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("usage of tenant %s: %w", tenantID, err)
	}
	err = cluster.LimitViolation(tenant.Effective(planLimits, overrides), tn.Plan, usage, kind, create, interval, oldInterval)
	var ce *cluster.CommandError
	if errors.As(err, &ce) {
		return limitProblem(ce)
	}
	return err
}

func (h *Handlers) logLimitLookup(tenantID string, err error) {
	if h.log != nil && !errors.Is(err, store.ErrNotFound) {
		h.log.Warn("limit pre-check skipped; the FSM decides", "tenant", tenantID, "err", err)
	}
}

// limitProblem answers a limit violation with 403. The message is written to
// be shown to the user (FA-TEN-04).
func limitProblem(ce *cluster.CommandError) *Problem {
	return &Problem{Status: http.StatusForbidden, Title: "Forbidden", Detail: ce.Message}
}

// adminCommand starts a tenant or plan command of the platform admin. The actor
// is the user: the FSM admits users and the system, never API tokens (ADR 0029).
func adminCommand(p auth.Principal, reqID string) *pb.Command {
	return &pb.Command{RequestId: reqID, Actor: actorOf(p)}
}

// actorOf returns the actor of commands the principal causes (ADR 0031). The
// name is kept in the audit log as it was at the time of the action.
func actorOf(p auth.Principal) *pb.Actor {
	kind := pb.Actor_KIND_USER
	if p.ViaToken {
		kind = pb.Actor_KIND_API_TOKEN
	}
	return &pb.Actor{Kind: kind, Id: p.UserID, Name: p.Username}
}

// applyTenant proposes a tenant command and returns the tenant as the FSM left it.
func (h *Handlers) applyTenant(ctx context.Context, cmd *pb.Command) (store.Tenant, error) {
	res, err := h.apply(ctx, cmd)
	if err != nil {
		return store.Tenant{}, err
	}
	var r cluster.TenantResult
	if err := json.Unmarshal(res.Result, &r); err != nil {
		return store.Tenant{}, fmt.Errorf("decode tenant result: %w", err)
	}
	tn, err := h.deps.State.GetTenant(ctx, r.TenantID)
	if err != nil {
		return store.Tenant{}, fmt.Errorf("read tenant %s: %w", r.TenantID, err)
	}
	return tn, nil
}

// adminTenant loads a tenant for the admin API. The system tenant is reported
// as not found (ADR 0025).
func (h *Handlers) adminTenant(ctx context.Context, id string) (store.Tenant, error) {
	if id == store.SystemTenantID {
		return store.Tenant{}, notFound("tenant")
	}
	tn, err := h.deps.State.GetTenant(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Tenant{}, notFound("tenant")
	}
	return tn, err
}

// adminPlan loads a plan for the admin API. The system plan is reported as not found.
func (h *Handlers) adminPlan(ctx context.Context, id string) (store.Plan, error) {
	if id == tenant.SystemPlanID {
		return store.Plan{}, notFound("plan")
	}
	p, err := h.deps.State.GetPlan(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Plan{}, notFound("plan")
	}
	return p, err
}

// planFieldError returns why a plan cannot be given to a tenant, or "".
func (h *Handlers) planFieldError(ctx context.Context, planID string) (string, error) {
	if planID == tenant.SystemPlanID {
		return "the system plan is reserved for the system tenant", nil
	}
	_, err := h.deps.State.GetPlan(ctx, planID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "plan does not exist", nil
	case err != nil:
		return "", err
	}
	return "", nil
}

// planCounts returns the number of tenants per plan ID.
func (h *Handlers) planCounts(ctx context.Context) (map[string]int, error) {
	tns, err := h.deps.State.ListTenants(ctx)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int, len(tns))
	for _, tn := range tns {
		counts[tn.Plan]++
	}
	return counts, nil
}

// tenantView is a tenant with its plan, overrides, effective limits and usage.
type tenantView struct {
	tn        store.Tenant
	overrides tenant.Overrides
	effective tenant.Limits
	usage     store.TenantUsage
	exceeded  []gen.LimitExcess
}

// viewTenant loads the plan and usage of a tenant.
func (h *Handlers) viewTenant(ctx context.Context, tn store.Tenant) (tenantView, error) {
	plan, err := h.deps.State.GetPlan(ctx, tn.Plan)
	if err != nil {
		return tenantView{}, fmt.Errorf("plan %s of tenant %s: %w", tn.Plan, tn.ID, err)
	}
	planLimits, err := tenant.ParseLimits(plan.Limits)
	if err != nil {
		return tenantView{}, fmt.Errorf("stored limits of plan %s: %w", plan.ID, err)
	}
	overrides, err := tenant.ParseOverrides(tn.Limits)
	if err != nil {
		return tenantView{}, fmt.Errorf("stored limits of tenant %s: %w", tn.ID, err)
	}
	usage, err := h.deps.State.TenantUsage(ctx, tn.ID)
	if err != nil {
		return tenantView{}, fmt.Errorf("usage of tenant %s: %w", tn.ID, err)
	}
	eff := tenant.Effective(planLimits, overrides)
	return tenantView{tn: tn, overrides: overrides, effective: eff, usage: usage, exceeded: exceededLimits(eff, usage)}, nil
}

// exceededLimits lists the limits the current stock exceeds, e.g. after the
// tenant was moved to a smaller plan (ADR 0029, 3A).
func exceededLimits(eff tenant.Limits, u store.TenantUsage) []gen.LimitExcess {
	out := []gen.LimitExcess{}
	if eff.MaxMonitors != nil && u.Monitors > *eff.MaxMonitors {
		out = append(out, gen.LimitExcess{Limit: gen.MaxMonitors, Allowed: *eff.MaxMonitors, Actual: u.Monitors})
	}
	if eff.MinIntervalSeconds != nil && u.Monitors > 0 && u.MinIntervalSeconds < *eff.MinIntervalSeconds {
		out = append(out, gen.LimitExcess{Limit: gen.MinIntervalSeconds, Allowed: *eff.MinIntervalSeconds, Actual: u.MinIntervalSeconds})
	}
	if eff.MaxChannels != nil && u.Channels > *eff.MaxChannels {
		out = append(out, gen.LimitExcess{Limit: gen.MaxChannels, Allowed: *eff.MaxChannels, Actual: u.Channels})
	}
	return out
}

func apiTenant(v tenantView) gen.Tenant {
	out := gen.Tenant{
		Id:              v.tn.ID,
		Name:            v.tn.Name,
		Plan:            v.tn.Plan,
		Limits:          map[string]*int(v.overrides),
		EffectiveLimits: apiLimits(v.effective),
		Suspended:       v.tn.Suspended,
		Source:          gen.TenantSource(v.tn.Source),
		Usage:           apiUsage(v.usage),
		Exceeded:        v.exceeded,
		CreatedAt:       time.UnixMilli(v.tn.CreatedMs).UTC(),
		UpdatedAt:       time.UnixMilli(v.tn.UpdatedMs).UTC(),
	}
	if v.tn.ZitadelOrgID != "" {
		org := v.tn.ZitadelOrgID
		out.OrgId = &org
	}
	return out
}

func ownTenant(v tenantView) gen.OwnTenant {
	return gen.OwnTenant{
		Id:              v.tn.ID,
		Name:            v.tn.Name,
		Plan:            v.tn.Plan,
		EffectiveLimits: apiLimits(v.effective),
		Usage:           apiUsage(v.usage),
		Exceeded:        v.exceeded,
	}
}

func apiPlan(p store.Plan, tenantCount int) (gen.Plan, error) {
	lim, err := tenant.ParseLimits(p.Limits)
	if err != nil {
		return gen.Plan{}, fmt.Errorf("stored limits of plan %s: %w", p.ID, err)
	}
	return gen.Plan{
		Id:          p.ID,
		Name:        p.Name,
		Limits:      apiLimits(lim),
		TenantCount: tenantCount,
		CreatedAt:   time.UnixMilli(p.CreatedMs).UTC(),
		UpdatedAt:   time.UnixMilli(p.UpdatedMs).UTC(),
	}, nil
}

func apiLimits(l tenant.Limits) gen.Limits {
	return gen.Limits{
		MaxMonitors:        l.MaxMonitors,
		MinIntervalSeconds: l.MinIntervalSeconds,
		MaxChannels:        l.MaxChannels,
		MaxStatusPages:     l.MaxStatusPages,
		MaxPrivateProbes:   l.MaxPrivateProbes,
	}
}

func apiUsage(u store.TenantUsage) gen.TenantUsage {
	out := gen.TenantUsage{Monitors: u.Monitors, Channels: u.Channels}
	if u.MinIntervalSeconds > 0 {
		m := u.MinIntervalSeconds
		out.MinIntervalSeconds = &m
	}
	return out
}

// tenantAdminCommand reports whether cmd is a tenant or plan command (ADR 0029).
func tenantAdminCommand(cmd *pb.Command) bool {
	switch cmd.GetPayload().(type) {
	case *pb.Command_CreateTenant, *pb.Command_UpdateTenant, *pb.Command_SetTenantSuspended,
		*pb.Command_DeleteTenant, *pb.Command_PutPlan, *pb.Command_DeletePlan:
		return true
	}
	return false
}

// planLimits validates the limits of a plan and returns them. Unknown fields
// are rejected.
func planLimits(l map[string]*int) (tenant.Limits, error) {
	raw, err := json.Marshal(l)
	if err != nil {
		return tenant.Limits{}, err
	}
	return tenant.ParseLimits(raw)
}

// overridesJSON validates per-tenant overrides and returns their canonical JSON.
// A null value lifts the plan limit for the tenant.
func overridesJSON(in map[string]*int) (string, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	o, err := tenant.ParseOverrides(raw)
	if err != nil {
		return "", err
	}
	return string(o.Canonical()), nil
}

// limitMessage strips the common prefix of limit validation errors for the field message.
func limitMessage(err error) string {
	return strings.TrimPrefix(err.Error(), tenant.ErrInvalidLimits.Error()+": ")
}

func fieldError(field, msg string) checker.FieldError {
	return checker.FieldError{Field: field, Message: msg}
}

// validationProblem returns a 422 with the field errors, or nil without errors.
func validationProblem(fields []checker.FieldError, detail string) error {
	if len(fields) == 0 {
		return nil
	}
	return &Problem{Status: http.StatusUnprocessableEntity, Title: "Validation Failed", Detail: detail, Fields: fields}
}

func requestBodyRequired() *Problem {
	return &Problem{Status: http.StatusBadRequest, Title: "Bad Request", Detail: "request body is required"}
}
