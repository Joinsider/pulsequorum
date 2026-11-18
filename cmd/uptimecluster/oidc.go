// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package main

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/joinsider/uptimecluster/internal/auth"
	"github.com/joinsider/uptimecluster/internal/auth/oidc"
	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/config"
	"github.com/joinsider/uptimecluster/internal/provision"
	"github.com/joinsider/uptimecluster/internal/store"
)

// oidcKeyCacheFile is the key cache in the data directory (ADR 0027).
const oidcKeyCacheFile = "oidc-jwks.json"

// recommendedTokenTTL is the access token lifetime the setup docs recommend.
const recommendedTokenTTL = 15 * time.Minute

// provisionPlanInterval is how often the plan check of the automatic
// provisioning retries while the plan does not exist.
const provisionPlanInterval = 10 * time.Second

// oidcTenants is the tenant read access of the verifier.
type oidcTenants interface {
	oidc.TenantStore
}

// newOIDC builds the key set and the authenticator for AUTH_MODE=oidc. Keys
// are loaded in the background by runOIDCKeys; until then requests get 503.
// oidcMetrics are the counters the OIDC authenticator reports to.
type oidcMetrics interface {
	JWKSFetchFailed()
	UserInfoCalled()
	UserInfoFailed()
}

// applier proposes the tenant commands of the automatic provisioning.
func newOIDC(cfg config.Config, log *slog.Logger, tenants oidcTenants, now func() time.Time, m oidcMetrics, applier provision.Applier) (*oidc.KeySet, *oidc.Authenticator, error) {
	c := cfg.OIDC
	keys, err := oidc.NewKeySet(oidc.KeySetOptions{
		Issuer: c.Issuer, Algorithms: c.Algorithms, AllowHTTP: c.InsecureHTTP,
		CacheFile: filepath.Join(cfg.DataDir, oidcKeyCacheFile), OnFetchFailure: m.JWKSFetchFailed,
		Now: now, Log: log,
	})
	if err != nil {
		return nil, nil, err
	}
	var profile oidc.Profile
	switch c.Profile {
	case config.ProfileZitadel:
		zitadel := oidc.Zitadel{
			ProjectID: c.Audience, PlatformOrgID: c.PlatformOrgID, Multi: c.Tenancy == config.TenancyMulti,
			DefaultTenantID: cluster.DefaultTenantID, Tenants: tenants,
		}
		if c.AutoProvision {
			p, err := provision.NewProvisioner(provision.ProvisionerOptions{
				Cluster: applier, PlanID: c.AutoProvisionPlan, Max: c.AutoProvisionMax, Log: log,
			})
			if err != nil {
				return nil, nil, err
			}
			zitadel.Provisioner = p
			log.Info("automatic provisioning of tenants is on (ADR 0029)", "plan", c.AutoProvisionPlan, "max", c.AutoProvisionMax)
		}
		profile = zitadel
	default:
		roleMap := make(map[string]auth.Role, len(c.RoleMap))
		for k, v := range c.RoleMap {
			roleMap[k] = auth.Role(v)
		}
		profile = oidc.Generic{TenantID: cluster.DefaultTenantID, RolesClaim: c.RolesClaim, RoleMap: roleMap,
			DefaultRole: auth.Role(c.DefaultRole)}
	}
	var userInfo *oidc.UserInfo
	if c.RolesSource == config.RolesSourceUserInfo {
		userInfo = oidc.NewUserInfo(oidc.UserInfoOptions{Keys: keys, OnCall: m.UserInfoCalled, OnFailure: m.UserInfoFailed, Now: now})
		log.Info("OIDC roles are read from the userinfo endpoint, once per access token (ADR 0028)")
	}
	if c.DefaultRole != "" {
		log.Warn("every token without a role gets OIDC_DEFAULT_ROLE; restrict access to the client at the identity provider",
			"default_role", c.DefaultRole)
	}
	authn, err := oidc.NewAuthenticator(oidc.Options{
		Issuer: c.Issuer, Audience: c.Audience, ClientIDs: c.ClientIDs, RequireAZP: c.RequireAZP,
		Algorithms: c.Algorithms, MaxTokenTTL: c.MaxTokenTTL, Profile: profile, Tenants: tenants,
		Keys: keys, UserInfo: userInfo, Now: now, Log: log,
	})
	if err != nil {
		return nil, nil, err
	}
	if cfg.AuthModeAlias {
		log.Info("AUTH_MODE=zitadel is an alias for AUTH_MODE=oidc with OIDC_PROFILE=zitadel")
	}
	if c.InsecureHTTP {
		log.Warn("OIDC_INSECURE_HTTP is set: the identity provider is reached without TLS; use this for development only")
	}
	log.Info("OIDC authentication enabled", "issuer", c.Issuer, "profile", c.Profile, "tenancy", c.Tenancy,
		"max_token_ttl", c.MaxTokenTTL)
	if c.MaxTokenTTL > recommendedTokenTTL {
		log.Info("access tokens may live up to OIDC_MAX_TOKEN_TTL; revoked roles stay valid until the token expires. "+
			"Set the access token lifetime of the identity provider to 15 minutes", "max_token_ttl", c.MaxTokenTTL)
	}
	return keys, authn, nil
}

// watchProvisionPlan checks that the plan of the automatic provisioning exists.
// The state may still be replaying the log at start, so a missing plan is
// logged as an error and checked again, instead of stopping the node. It
// returns once the plan exists or ctx ends.
func watchProvisionPlan(ctx context.Context, log *slog.Logger, state store.State, planID string) {
	ticker := time.NewTicker(provisionPlanInterval)
	defer ticker.Stop()
	reported := false
	for {
		_, err := state.GetPlan(ctx, planID)
		switch {
		case err == nil:
			log.Info("plan of automatic provisioning exists", "plan", planID)
			return
		case errors.Is(err, store.ErrNotFound):
			if !reported {
				log.Error("plan of automatic provisioning does not exist; first logins of new organizations are refused until it does",
					"plan", planID, "setting", "OIDC_AUTO_PROVISION_PLAN")
				reported = true
			}
		case ctx.Err() == nil:
			log.Warn("could not read the plan of automatic provisioning", "plan", planID, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runOIDCKeys loads the keys and refreshes them until ctx ends.
func runOIDCKeys(ctx context.Context, keys *oidc.KeySet, log *slog.Logger) {
	if err := keys.Load(ctx); err != nil && ctx.Err() == nil {
		log.Error("OIDC signing keys are not available; API requests are answered with 503 until they are", "err", err)
	}
	keys.Run(ctx)
}

// oidcReadiness extends a readiness check: without keys the node cannot
// authenticate anyone and is not ready (ADR 0027).
func oidcReadiness(next func() error, keys *oidc.KeySet) func() error {
	return func() error {
		if !keys.Ready() {
			return oidc.ErrNoKeys
		}
		return next()
	}
}

// bindPlatformOrg binds the default tenant to the platform organization
// (zitadel profile, ADR 0027). Only the leader proposes; the loop ends once
// the binding exists or cannot succeed.
func bindPlatformOrg(ctx context.Context, cfg config.Config, log *slog.Logger, n *cluster.Node, state store.State) {
	if cfg.AuthMode != config.AuthOIDC || cfg.OIDC.Profile != config.ProfileZitadel {
		return
	}
	org := cfg.OIDC.PlatformOrgID
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		if done := tryBindPlatformOrg(ctx, log, n, state, org); done {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func tryBindPlatformOrg(ctx context.Context, log *slog.Logger, n *cluster.Node, state store.State, org string) bool {
	t, err := state.GetTenant(ctx, cluster.DefaultTenantID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return false // not bootstrapped yet
	case err != nil:
		log.Warn("could not read the default tenant", "err", err)
		return false
	case t.ZitadelOrgID == org:
		return true
	case t.ZitadelOrgID != "":
		log.Warn("the default tenant is bound to another organization than ZITADEL_PLATFORM_ORG_ID; platform users are not admitted in multi tenancy",
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
