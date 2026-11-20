// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package authswitch

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/config"
	"github.com/joinsider/pulsequorum/internal/store"
)

// BindInterval is how often the leader retries binding the default tenant.
const BindInterval = 30 * time.Second

// TenantReader reads the default tenant.
type TenantReader interface {
	GetTenant(ctx context.Context, id string) (store.Tenant, error)
}

// BindPlatformOrg binds the default tenant to the platform organization
// (zitadel profile, ADR 0027). Only the leader proposes; the loop ends once
// the binding exists or cannot succeed.
func BindPlatformOrg(ctx context.Context, cfg config.Config, log *slog.Logger, n Applier, st TenantReader) {
	bindPlatformOrgEvery(ctx, cfg, log, n, st, BindInterval)
}

// bindPlatformOrgEvery is BindPlatformOrg with the retry interval as a parameter.
func bindPlatformOrgEvery(ctx context.Context, cfg config.Config, log *slog.Logger, n Applier, st TenantReader, interval time.Duration) {
	if cfg.AuthMode != config.AuthOIDC || cfg.OIDC.Profile != config.ProfileZitadel {
		return
	}
	org := cfg.OIDC.PlatformOrgID
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if TryBindPlatformOrg(ctx, log, n, st, org) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// TryBindPlatformOrg makes one attempt to bind the default tenant to org. It
// returns true when the attempt is final: the binding exists, the default
// tenant is bound elsewhere, or the command was rejected for good. It returns
// false when the caller should try again.
func TryBindPlatformOrg(ctx context.Context, log *slog.Logger, n Applier, st TenantReader, org string) bool {
	t, err := st.GetTenant(ctx, cluster.DefaultTenantID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return false // not bootstrapped yet
	case err != nil:
		log.Warn("could not read the default tenant", "err", err)
		return false
	case t.ZitadelOrgID == org:
		return true
	case t.ZitadelOrgID != "":
		log.Error("the default tenant is bound to another organization than ZITADEL_PLATFORM_ORG_ID; "+
			"set ZITADEL_PLATFORM_ORG_ID back to the bound organization (ADR 0032)",
			"bound_org", t.ZitadelOrgID, "platform_org", org)
		return true
	case !n.IsLeader():
		return false
	}
	applyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := n.Apply(applyCtx, cluster.NewBindTenantOrg(cluster.DefaultTenantID, org)); err != nil {
		var cerr *cluster.CommandError
		if errors.As(err, &cerr) && cerr.Code == cluster.CodeNotEnabled {
			log.Info("binding the default tenant waits until all voters run this version", "org", org)
			return false
		}
		if ctx.Err() == nil {
			log.Error("could not bind the default tenant to the platform organization", "org", org, "err", err)
		}
		return errors.As(err, &cerr) && cerr.Code != cluster.CodeNotEnabled
	}
	log.Info("default tenant bound to the platform organization", "org", org)
	return true
}
