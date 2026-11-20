// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package provision creates tenants for Zitadel organizations on their first
// login (FA-TEN-03, ADR 0029).
package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/pulsequorum/internal/auth"
	"github.com/joinsider/pulsequorum/internal/cluster"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/ratelimit"
	"github.com/joinsider/pulsequorum/internal/tenant"
)

// Defaults of the automatic provisioning (ADR 0029).
const (
	DefaultMax = 100
	// autoProvisionRate is the number of provisionings per node and minute.
	autoProvisionRate = 10
	// autoProvisionBlock is how long an organization is not retried after a
	// rejection.
	autoProvisionBlock = 60 * time.Second
	// autoProvisionBlockLimit bounds the remembered rejections.
	autoProvisionBlockLimit = 10_000
)

// errNotProvisioned is the answer for every failed provisioning: the caller
// learns nothing about limits or cluster state; the log has the reason.
var errNotProvisioned = fmt.Errorf("%w: organization is not provisioned", auth.ErrForbidden)

// Applier proposes commands; *Node implements it.
type Applier interface {
	Apply(ctx context.Context, cmd *pb.Command) (cluster.ApplyResult, error)
}

// ProvisionerOptions configure a Provisioner.
type ProvisionerOptions struct {
	Cluster Applier
	// PlanID is the plan of new tenants (OIDC_AUTO_PROVISION_PLAN).
	PlanID string
	// Max is the highest number of automatically created tenants
	// (OIDC_AUTO_PROVISION_MAX); the FSM enforces it cluster-wide.
	Max int
	Log *slog.Logger
	// Now is replaced in tests.
	Now func() time.Time
}

// Provisioner creates the tenant of a Zitadel organization on its first login
// (FA-TEN-03, ADR 0029). The caller has verified the token and checked that
// the user holds a role in the organization. It implements oidc.Provisioner.
type Provisioner struct {
	opts    ProvisionerOptions
	limiter *ratelimit.Limiter

	mu      sync.Mutex
	blocked map[string]time.Time // organization → rejected until
}

// NewProvisioner returns a provisioner. Max must be at least 1.
func NewProvisioner(opts ProvisionerOptions) (*Provisioner, error) {
	if opts.Cluster == nil {
		return nil, errors.New("provisioner: no cluster")
	}
	if !tenant.ValidID(opts.PlanID) || opts.PlanID == tenant.SystemPlanID {
		return nil, fmt.Errorf("provisioner: invalid plan %q", opts.PlanID)
	}
	if opts.Max < 1 {
		return nil, fmt.Errorf("provisioner: the maximum number of tenants must be at least 1, got %d", opts.Max)
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	l := ratelimit.New(autoProvisionRate/60.0, autoProvisionRate)
	l.Now = opts.Now
	return &Provisioner{opts: opts, limiter: l, blocked: map[string]time.Time{}}, nil
}

// Provision implements oidc.Provisioner.
func (p *Provisioner) Provision(ctx context.Context, orgID, orgName string) (string, error) {
	log := p.opts.Log.With("org_id", orgID)
	if !tenant.ValidOrgID(orgID) {
		log.Warn("automatic provisioning rejected: invalid organization ID")
		return "", errNotProvisioned
	}
	if p.isBlocked(orgID) {
		return "", errNotProvisioned
	}
	// One node-wide bucket: the limit is on new tenants, not per caller.
	if ok, _ := p.limiter.Allow("provision"); !ok {
		log.Warn("automatic provisioning rejected: rate limit reached", "per_minute", autoProvisionRate)
		return "", errNotProvisioned
	}

	id := uuid.NewString()
	cmd := cluster.NewProvisionTenant(id, tenant.SanitizeName(orgName, orgID), p.opts.PlanID, orgID, p.opts.Max)
	res, err := p.opts.Cluster.Apply(ctx, cmd)
	if err == nil {
		err = res.Err
	}
	if err != nil {
		var ce *cluster.CommandError
		if errors.As(err, &ce) {
			p.block(orgID)
			log.Warn("automatic provisioning rejected", "code", ce.Code, "reason", ce.Message)
			return "", errNotProvisioned
		}
		// Not applied or unknown outcome: the next login retries.
		return "", fmt.Errorf("%w: provisioning: %w", auth.ErrAuthUnavailable, err)
	}
	var r cluster.TenantResult
	if err := json.Unmarshal(res.Result, &r); err != nil || r.TenantID == "" {
		return "", fmt.Errorf("%w: provisioning returned no tenant", auth.ErrAuthUnavailable)
	}
	if r.Existing {
		log.Info("organization was provisioned concurrently", "tenant_id", r.TenantID)
	} else {
		log.Info("tenant provisioned automatically", "tenant_id", r.TenantID, "plan", p.opts.PlanID)
	}
	return r.TenantID, nil
}

func (p *Provisioner) isBlocked(orgID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	until, ok := p.blocked[orgID]
	if !ok {
		return false
	}
	if p.opts.Now().Before(until) {
		return true
	}
	delete(p.blocked, orgID)
	return false
}

func (p *Provisioner) block(orgID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.opts.Now()
	if len(p.blocked) >= autoProvisionBlockLimit {
		for k, until := range p.blocked {
			if !now.Before(until) {
				delete(p.blocked, k)
			}
		}
		if len(p.blocked) >= autoProvisionBlockLimit {
			return // keep memory bounded; the rate limit still applies
		}
	}
	p.blocked[orgID] = now.Add(autoProvisionBlock)
}
