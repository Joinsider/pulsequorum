// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package authswitch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/config"
	"github.com/joinsider/pulsequorum/internal/store"
)

// actorKindUser is the audit value of a user actor. It equals the value that
// internal/cluster writes for pb.Actor_KIND_USER (audit.go).
const actorKindUser = "user"

// StartupHintsInterval is how often RunStartupHints checks whether it can log.
const StartupHintsInterval = 5 * time.Second

// HintReader is the read access of the startup hints.
type HintReader interface {
	GetTenant(ctx context.Context, id string) (store.Tenant, error)
	ListTenants(ctx context.Context) ([]store.Tenant, error)
	GetUser(ctx context.Context, tenantID, id string) (store.User, error)
	ListAPITokens(ctx context.Context, tenantID string) ([]store.APIToken, error)
}

// LogStartupHints logs what an operator must check after the switch from
// AUTH_MODE=local to oidc (ADR 0032, options 3A, 4A, 5A): every tenant from
// local mode that has no organization, and the API tokens that local users
// created and that stay valid. It runs once, on the leader.
func LogStartupHints(ctx context.Context, cfg config.Config, log *slog.Logger, st HintReader, now func() time.Time) error {
	tenants, err := st.ListTenants(ctx)
	if err != nil {
		return fmt.Errorf("list tenants: %w", err)
	}
	nowMs := now().UnixMilli()
	for _, t := range tenants {
		if t.ID != cluster.DefaultTenantID && t.ID != store.SystemTenantID && t.ZitadelOrgID == "" {
			if cfg.OIDC.Profile == config.ProfileZitadel && cfg.OIDC.Tenancy == config.TenancyMulti {
				log.Warn("tenant from local mode is not bound to an organization; bind it with PATCH /api/v1/admin/tenants/{tenantId} and orgId",
					"tenant_id", t.ID, "name", t.Name)
			} else {
				log.Warn("tenant from local mode is not reachable in this OIDC setup", "tenant_id", t.ID, "name", t.Name)
			}
		}
		count, err := countLocalAPITokens(ctx, st, t.ID, nowMs)
		if err != nil {
			return fmt.Errorf("count API tokens of tenant %s: %w", t.ID, err)
		}
		if count > 0 {
			log.Info("API tokens created by local users stay valid after the switch to oidc; "+
				"revoke them with DELETE /api/v1/api-tokens/{id} if needed",
				"tenant_id", t.ID, "count", count)
		}
	}
	return nil
}

// countLocalAPITokens counts the unexpired API tokens of the tenant whose
// creator is a local user of that tenant.
func countLocalAPITokens(ctx context.Context, st HintReader, tenantID string, nowMs int64) (int, error) {
	tokens, err := st.ListAPITokens(ctx, tenantID)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, tok := range tokens {
		if tok.CreatedByKind != actorKindUser || tok.ExpiresMs <= nowMs {
			continue
		}
		_, err := st.GetUser(ctx, tenantID, tok.CreatedByID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			continue // created by someone who is not a local user of the tenant
		case err != nil:
			return 0, fmt.Errorf("read creator of token %s: %w", tok.ID, err)
		}
		count++
	}
	return count, nil
}

// RunStartupHints waits until this node leads and the default tenant exists,
// then calls LogStartupHints once. It stops early when ctx ends or the hints
// are logged. The caller starts it only in AUTH_MODE=oidc.
func RunStartupHints(ctx context.Context, cfg config.Config, log *slog.Logger, n Applier, st HintReader, now func() time.Time, interval time.Duration) {
	if cfg.AuthMode != config.AuthOIDC {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if n.IsLeader() {
			if _, err := st.GetTenant(ctx, cluster.DefaultTenantID); err == nil {
				err := LogStartupHints(ctx, cfg, log, st, now)
				if err == nil {
					return
				}
				if ctx.Err() == nil {
					log.Warn("could not log the startup hints of the switch to oidc; retrying", "err", err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
