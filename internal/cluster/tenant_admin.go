// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/tenant"
)

// TenantAdminCommandLevel is the command level of the tenant and plan commands
// and of limit enforcement (ADR 0029). Below it the commands are rejected and
// PutEntity does not check limits, so that voters of different versions apply
// the same log the same way (ADR 0018).
const TenantAdminCommandLevel = 8

// CodeLimitExceeded rejects a command that would exceed a tenant limit
// (FA-TEN-04). The API answers 403.
const CodeLimitExceeded = "limit_exceeded"

// TenantResult is the result of the tenant commands.
type TenantResult struct {
	TenantID string `json:"tenant_id"`
	// Existing is set when CreateTenant with source auto found the
	// organization already bound and returned that tenant.
	Existing bool `json:"existing,omitempty"`
}

// PlanResult is the result of the plan commands.
type PlanResult struct {
	PlanID string `json:"plan_id"`
}

// requireTenantAdmin checks the command level and the actor of a tenant or
// plan command. The API checks the platform-admin role; the FSM only admits
// users and the system, never API tokens (ADR 0029).
func requireTenantAdmin(ctx context.Context, tx store.StateTx, cmd *pb.Command, what string) error {
	if err := requireCommandLevel(ctx, tx, what, TenantAdminCommandLevel); err != nil {
		return err
	}
	switch cmd.GetActor().GetKind() {
	case pb.Actor_KIND_USER, pb.Actor_KIND_SYSTEM:
		return nil
	}
	return invalid("%s is only allowed for users and the system", what)
}

// checkOrgFree validates orgID and checks that no tenant other than tenantID
// holds it. The system tenant can never be bound (ADR 0027).
func checkOrgFree(ctx context.Context, tx store.StateTx, tenantID, orgID string) error {
	if tenantID == store.SystemTenantID {
		return invalid("the system tenant cannot be bound to an organization")
	}
	if !tenant.ValidOrgID(orgID) {
		return invalid("%s", tenant.OrgIDRule)
	}
	holder, err := tx.GetTenantByOrg(ctx, orgID)
	switch {
	case err == nil && holder.ID != tenantID:
		return &CommandError{Code: CodeConflict, Message: fmt.Sprintf("organization %s is already bound to another tenant", orgID)}
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return storeError(err)
	}
	return nil
}

// checkAssignablePlan checks that the plan exists and may be given to a tenant.
func checkAssignablePlan(ctx context.Context, tx store.StateTx, planID string) error {
	if planID == tenant.SystemPlanID {
		return invalid("the system plan is reserved for the system tenant")
	}
	if _, err := tx.GetPlan(ctx, planID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return invalid("plan %s does not exist", planID)
		}
		return storeError(err)
	}
	return nil
}

// canonicalOverrides validates per-tenant overrides and returns their canonical JSON.
func canonicalOverrides(raw string) (json.RawMessage, error) {
	o, err := tenant.ParseOverrides([]byte(raw))
	if err != nil {
		return nil, invalid("%v", err)
	}
	return o.Canonical(), nil
}

// applyCreateTenant creates a tenant (ADR 0029). Rules, in order:
//   - level, actor, source admin or auto;
//   - with source auto: an organization already bound returns its tenant
//     (parallel first logins), and the number of auto tenants is capped;
//   - valid, new tenant ID that never existed (tombstones);
//   - valid name, assignable plan, valid overrides, free organization.
func applyCreateTenant(ctx context.Context, tx store.StateTx, cmd *pb.Command, c *pb.CreateTenant) (json.RawMessage, error) {
	if err := requireTenantAdmin(ctx, tx, cmd, "creating tenants"); err != nil {
		return nil, err
	}
	id, orgID := c.GetTenantId(), c.GetOrgId()
	source := c.GetSource()
	switch source {
	case tenant.SourceAdmin:
	case tenant.SourceAuto:
		if cmd.GetActor().GetKind() != pb.Actor_KIND_SYSTEM {
			return nil, invalid("only the system provisions tenants automatically")
		}
		if orgID == "" {
			return nil, invalid("automatic provisioning needs an organization")
		}
		if tenant.ValidOrgID(orgID) {
			existing, err := tx.GetTenantByOrg(ctx, orgID)
			switch {
			case err == nil:
				return json.Marshal(TenantResult{TenantID: existing.ID, Existing: true})
			case !errors.Is(err, store.ErrNotFound):
				return nil, storeError(err)
			}
		}
		n, err := tx.CountTenants(ctx, tenant.SourceAuto)
		if err != nil {
			return nil, storeError(err)
		}
		if n >= int(c.GetMaxAutoTenants()) {
			return nil, &CommandError{Code: CodeLimitExceeded, Message: fmt.Sprintf("automatic provisioning is limited to %d tenants", c.GetMaxAutoTenants())}
		}
	default:
		return nil, invalid("tenant source must be %s or %s", tenant.SourceAdmin, tenant.SourceAuto)
	}

	switch {
	case !tenant.ValidID(id):
		return nil, invalid("tenant ID must be 1 to 64 characters of a-z, 0-9 and '-'")
	case !tenant.ValidName(c.GetName()):
		return nil, invalid("tenant name must be 1 to 100 characters without control characters or surrounding spaces")
	}
	deleted, err := tx.IsTenantDeleted(ctx, id)
	if err != nil {
		return nil, storeError(err)
	}
	if deleted {
		return nil, &CommandError{Code: CodeConflict, Message: fmt.Sprintf("tenant ID %s belonged to a deleted tenant and is not reused", id)}
	}
	if _, err := tx.GetTenant(ctx, id); err == nil {
		return nil, &CommandError{Code: CodeConflict, Message: fmt.Sprintf("tenant %s already exists", id)}
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, storeError(err)
	}
	if err := checkAssignablePlan(ctx, tx, c.GetPlanId()); err != nil {
		return nil, err
	}
	limits, err := canonicalOverrides(c.GetLimitsJson())
	if err != nil {
		return nil, err
	}
	if orgID != "" {
		if err := checkOrgFree(ctx, tx, id, orgID); err != nil {
			return nil, err
		}
	}

	issuedAt := cmd.GetIssuedAtMs()
	if err := tx.PutTenant(ctx, store.Tenant{
		ID: id, Name: c.GetName(), ZitadelOrgID: orgID, Plan: c.GetPlanId(), Limits: limits,
		Source: source, CreatedMs: issuedAt, UpdatedMs: issuedAt,
	}); err != nil {
		return nil, storeError(err)
	}
	return json.Marshal(TenantResult{TenantID: id})
}

// applyUpdateTenant changes the set fields of a tenant (ADR 0029). The system
// tenant is never changed; the default tenant keeps its organization binding,
// which the leader manages in the zitadel profile (ADR 0027).
func applyUpdateTenant(ctx context.Context, tx store.StateTx, cmd *pb.Command, u *pb.UpdateTenant) (json.RawMessage, error) {
	if err := requireTenantAdmin(ctx, tx, cmd, "updating tenants"); err != nil {
		return nil, err
	}
	id := u.GetTenantId()
	if id == store.SystemTenantID {
		return nil, invalid("the system tenant is reserved")
	}
	tn, err := tx.GetTenant(ctx, id)
	if err != nil {
		return nil, storeError(err)
	}
	if u.Name != nil {
		if !tenant.ValidName(u.GetName()) {
			return nil, invalid("tenant name must be 1 to 100 characters without control characters or surrounding spaces")
		}
		tn.Name = u.GetName()
	}
	if u.PlanId != nil {
		if err := checkAssignablePlan(ctx, tx, u.GetPlanId()); err != nil {
			return nil, err
		}
		tn.Plan = u.GetPlanId()
	}
	if u.LimitsJson != nil {
		limits, err := canonicalOverrides(u.GetLimitsJson())
		if err != nil {
			return nil, err
		}
		tn.Limits = limits
	}
	if u.OrgId != nil && u.GetOrgId() != tn.ZitadelOrgID {
		if id == tenant.DefaultTenantID {
			return nil, invalid("the organization of the default tenant is set by the server configuration")
		}
		if u.GetOrgId() != "" {
			if err := checkOrgFree(ctx, tx, id, u.GetOrgId()); err != nil {
				return nil, err
			}
		}
		tn.ZitadelOrgID = u.GetOrgId()
	}
	tn.UpdatedMs = cmd.GetIssuedAtMs()
	if err := tx.PutTenant(ctx, tn); err != nil {
		return nil, storeError(err)
	}
	return json.Marshal(TenantResult{TenantID: id})
}

// applySetTenantSuspended suspends or resumes a tenant (ADR 0029). The system
// and default tenants are never suspended. Suspending revokes the tenant's
// refresh sessions, so local users have to sign in again after a resume.
func applySetTenantSuspended(ctx context.Context, tx store.StateTx, cmd *pb.Command, s *pb.SetTenantSuspended) (json.RawMessage, error) {
	if err := requireTenantAdmin(ctx, tx, cmd, "suspending tenants"); err != nil {
		return nil, err
	}
	id := s.GetTenantId()
	if tenant.Protected(id) {
		return nil, invalid("tenant %s cannot be suspended", id)
	}
	tn, err := tx.GetTenant(ctx, id)
	if err != nil {
		return nil, storeError(err)
	}
	if tn.Suspended == s.GetSuspended() {
		return json.Marshal(TenantResult{TenantID: id})
	}
	tn.Suspended = s.GetSuspended()
	tn.UpdatedMs = cmd.GetIssuedAtMs()
	if err := tx.PutTenant(ctx, tn); err != nil {
		return nil, storeError(err)
	}
	if tn.Suspended {
		if err := revokeTenantSessions(ctx, tx, id, cmd.GetIssuedAtMs()); err != nil {
			return nil, err
		}
	}
	return json.Marshal(TenantResult{TenantID: id})
}

// revokeTenantSessions revokes the refresh sessions of the tenant's users.
func revokeTenantSessions(ctx context.Context, tx store.StateTx, tenantID string, ms int64) error {
	if err := tx.RevokeTenantSessions(ctx, tenantID, ms); err != nil {
		return storeError(err)
	}
	return nil
}

// applyDeleteTenant deletes a suspended tenant with all its replicated data in
// this transaction and records a tombstone (ADR 0029). The system and default
// tenants are never deleted.
func applyDeleteTenant(ctx context.Context, tx store.StateTx, cmd *pb.Command, d *pb.DeleteTenant) (json.RawMessage, error) {
	if err := requireTenantAdmin(ctx, tx, cmd, "deleting tenants"); err != nil {
		return nil, err
	}
	id := d.GetTenantId()
	if tenant.Protected(id) {
		return nil, invalid("tenant %s cannot be deleted", id)
	}
	tn, err := tx.GetTenant(ctx, id)
	if err != nil {
		return nil, storeError(err)
	}
	if !tn.Suspended {
		return nil, &CommandError{Code: CodeConflict, Message: "suspend the tenant before deleting it"}
	}
	if err := tx.DeleteTenant(ctx, id, cmd.GetIssuedAtMs()); err != nil {
		return nil, storeError(err)
	}
	return json.Marshal(TenantResult{TenantID: id})
}

// applyPutPlan creates or replaces a plan (ADR 0029). The system plan is fixed.
func applyPutPlan(ctx context.Context, tx store.StateTx, cmd *pb.Command, p *pb.PutPlan) (json.RawMessage, error) {
	if err := requireTenantAdmin(ctx, tx, cmd, "changing plans"); err != nil {
		return nil, err
	}
	id := p.GetPlanId()
	switch {
	case id == tenant.SystemPlanID:
		return nil, invalid("the system plan cannot be changed")
	case !tenant.ValidID(id):
		return nil, invalid("plan ID must be 1 to 64 characters of a-z, 0-9 and '-'")
	case !tenant.ValidName(p.GetName()):
		return nil, invalid("plan name must be 1 to 100 characters without control characters or surrounding spaces")
	}
	limits, err := tenant.ParseLimits([]byte(p.GetLimitsJson()))
	if err != nil {
		return nil, invalid("%v", err)
	}
	issuedAt := cmd.GetIssuedAtMs()
	if err := tx.PutPlan(ctx, store.Plan{ID: id, Name: p.GetName(), Limits: limits.Canonical(), CreatedMs: issuedAt, UpdatedMs: issuedAt}); err != nil {
		return nil, storeError(err)
	}
	return json.Marshal(PlanResult{PlanID: id})
}

// applyDeletePlan deletes a plan that no tenant uses (ADR 0029). The default
// and system plans are never deleted.
func applyDeletePlan(ctx context.Context, tx store.StateTx, cmd *pb.Command, d *pb.DeletePlan) (json.RawMessage, error) {
	if err := requireTenantAdmin(ctx, tx, cmd, "deleting plans"); err != nil {
		return nil, err
	}
	id := d.GetPlanId()
	if id == tenant.DefaultPlanID || id == tenant.SystemPlanID {
		return nil, invalid("plan %s cannot be deleted", id)
	}
	n, err := tx.CountTenantsWithPlan(ctx, id)
	if err != nil {
		return nil, storeError(err)
	}
	if n > 0 {
		return nil, &CommandError{Code: CodeConflict, Message: fmt.Sprintf("plan %s is used by %d tenants", id, n)}
	}
	if err := tx.DeletePlan(ctx, id); err != nil {
		return nil, storeError(err)
	}
	return json.Marshal(PlanResult{PlanID: id})
}

// tenantLimits returns the effective limits of a tenant from the replicated
// state. A plan that does not exist is an error, so a broken reference never
// means "unlimited".
func tenantLimits(ctx context.Context, tx store.StateTx, tn store.Tenant) (tenant.Limits, error) {
	p, err := tx.GetPlan(ctx, tn.Plan)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return tenant.Limits{}, invalid("plan %s of tenant %s does not exist", tn.Plan, tn.ID)
		}
		return tenant.Limits{}, storeError(err)
	}
	planLimits, err := tenant.ParseLimits(p.Limits)
	if err != nil {
		return tenant.Limits{}, fmt.Errorf("stored limits of plan %s: %w", p.ID, err)
	}
	o, err := tenant.ParseOverrides(tn.Limits)
	if err != nil {
		return tenant.Limits{}, fmt.Errorf("stored limits of tenant %s: %w", tn.ID, err)
	}
	return tenant.Effective(planLimits, o), nil
}

// limitTarget is what a PutEntity creates or changes, for checkTenantLimits.
type limitTarget struct {
	kind   pb.EntityKind
	create bool
	// interval and oldInterval are only used for monitors; oldInterval is
	// the stored interval of a changed monitor.
	interval, oldInterval int
}

// checkTenantLimits enforces the plan limits of FA-TEN-04 inside the FSM, so
// that parallel requests through several nodes cannot exceed them (ADR 0029).
// From command level 8 on it also rejects writes to suspended tenants. The
// system tenant has no limits. A stock above a lowered limit stays: only new
// objects and shorter intervals are rejected (ADR 0029, 3A).
func checkTenantLimits(ctx context.Context, tx store.StateTx, tenantID string, t limitTarget) error {
	level, err := currentCommandLevel(ctx, tx)
	if err != nil {
		return err
	}
	if level < TenantAdminCommandLevel || tenantID == store.SystemTenantID {
		return nil
	}
	tn, err := tx.GetTenant(ctx, tenantID)
	if err != nil {
		return storeError(err)
	}
	if tn.Suspended {
		return &CommandError{Code: CodeConflict, Message: "tenant is suspended"}
	}
	lim, err := tenantLimits(ctx, tx, tn)
	if err != nil {
		return err
	}
	usage, err := tx.TenantUsage(ctx, tenantID)
	if err != nil {
		return storeError(err)
	}
	return LimitViolation(lim, tn.Plan, usage, t.kind, t.create, t.interval, t.oldInterval)
}

// LimitViolation returns a CodeLimitExceeded error if creating or changing an
// object of the kind would break the limits; nil otherwise. The API calls it
// before proposing, the FSM again when applying (ADR 0029).
func LimitViolation(lim tenant.Limits, planID string, usage store.TenantUsage, kind pb.EntityKind, create bool, interval, oldInterval int) error {
	exceeded := func(format string, args ...any) error {
		return &CommandError{Code: CodeLimitExceeded, Message: fmt.Sprintf(format, args...)}
	}
	switch kind {
	case pb.EntityKind_ENTITY_KIND_MONITOR:
		if create && lim.MaxMonitors != nil && usage.Monitors >= *lim.MaxMonitors {
			return exceeded("monitor limit reached: plan %q allows %d monitors", planID, *lim.MaxMonitors)
		}
		if lim.MinIntervalSeconds != nil && interval < *lim.MinIntervalSeconds && (create || interval != oldInterval) {
			return exceeded("interval too short: plan %q allows intervals of at least %d seconds", planID, *lim.MinIntervalSeconds)
		}
	case pb.EntityKind_ENTITY_KIND_NOTIFICATION_CHANNEL:
		if create && lim.MaxChannels != nil && usage.Channels >= *lim.MaxChannels {
			return exceeded("notification channel limit reached: plan %q allows %d channels", planID, *lim.MaxChannels)
		}
	}
	return nil
}

// checkPutTenant applies the rules of ADR 0029 to PutEntity for tenants from
// command level 8 on: only the system creates tenants that way (bootstrap),
// and an organization ID is checked like in BindTenantOrg. Below level 8 the
// old behavior stays, so that voters of different versions do not diverge.
func checkPutTenant(ctx context.Context, tx store.StateTx, cmd *pb.Command, tenantID string, doc TenantDocument) error {
	level, err := currentCommandLevel(ctx, tx)
	if err != nil {
		return err
	}
	if level < TenantAdminCommandLevel {
		return nil
	}
	if cmd.GetActor().GetKind() != pb.Actor_KIND_SYSTEM {
		return invalid("tenants are created through the tenant admin commands")
	}
	// The system plan belongs to the system tenant only; every other tenant
	// needs an existing, assignable plan.
	if tenantID == store.SystemTenantID {
		if doc.Plan != tenant.SystemPlanID {
			return invalid("the system tenant uses the system plan")
		}
	} else if err := checkAssignablePlan(ctx, tx, doc.Plan); err != nil {
		return err
	}
	if doc.ZitadelOrgID != "" {
		return checkOrgFree(ctx, tx, tenantID, doc.ZitadelOrgID)
	}
	return nil
}

// NewProvisionTenant builds the command that creates the tenant of an
// organization on its first login (ADR 0029). The request ID is unique per
// attempt: the FSM, not the dedup table, makes parallel first logins end at
// one tenant, and a rejection is not cached for later attempts.
func NewProvisionTenant(tenantID, name, planID, orgID string, maxTenants int) *pb.Command {
	if maxTenants < 0 {
		maxTenants = 0
	}
	return stamp(&pb.Command{
		RequestId: "provision-org:" + orgID + ":" + tenantID,
		Actor:     &pb.Actor{Kind: pb.Actor_KIND_SYSTEM},
		Payload: &pb.Command_CreateTenant{CreateTenant: &pb.CreateTenant{
			TenantId:       tenantID,
			Name:           name,
			PlanId:         planID,
			OrgId:          orgID,
			Source:         tenant.SourceAuto,
			MaxAutoTenants: uint32(min(maxTenants, math.MaxUint32)),
		}},
	})
}
