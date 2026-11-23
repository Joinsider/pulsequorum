// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/pulsequorum/internal/alert"
	"github.com/joinsider/pulsequorum/internal/api/gen"
	"github.com/joinsider/pulsequorum/internal/auth"
	"github.com/joinsider/pulsequorum/internal/auth/oidc"
	"github.com/joinsider/pulsequorum/internal/auth/oidctest"
	"github.com/joinsider/pulsequorum/internal/authswitch"
	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/config"
)

// newIDPAuthenticator builds the OIDC authenticator against the fake identity
// provider, the way cmd/pulsequorum does (generic profile, RS256 only).
func newIDPAuthenticator(t *testing.T, idp *oidctest.Server, tenants oidc.TenantStore) *oidc.Authenticator {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	keys, err := oidc.NewKeySet(oidc.KeySetOptions{
		Issuer: idp.Issuer(), Algorithms: []string{oidc.AlgRS256}, CacheFile: filepath.Join(t.TempDir(), "jwks.json"),
		Client: idp.Client(), Now: time.Now, Log: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := keys.Load(context.Background()); err != nil {
		t.Fatalf("load keys: %v", err)
	}
	authn, err := oidc.NewAuthenticator(oidc.Options{
		Issuer: idp.Issuer(), Audience: "project-1", ClientIDs: []string{"spa-client"}, RequireAZP: true,
		Algorithms: []string{oidc.AlgRS256}, MaxTokenTTL: time.Hour,
		Profile: oidc.Generic{TenantID: cluster.DefaultTenantID, RolesClaim: "roles"},
		Tenants: tenants, Keys: keys, Now: time.Now, Log: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	return authn
}

// idpToken signs an access token of the identity provider for subject sub
// with the single role role.
func idpToken(idp *oidctest.Server, sub, role string) string {
	claims := idp.Claims(sub, 15*time.Minute, time.Now())
	claims["roles"] = []string{role}
	return idp.Sign(claims)
}

// remount serves the state and the node of base in another authentication
// mode, as a restart with a changed AUTH_MODE does (ADR 0032).
func remount(t *testing.T, base *env, cfg config.Config, local *auth.Local, authn auth.Authenticator) *env {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := &env{t: t, metrics: base.metrics, node: base.node, state: base.state, local: local, keyring: base.keyring}
	deps := Deps{
		State: base.state, Metrics: base.metrics, Cluster: base.node, Registry: checker.Default,
		CheckEnv: &checker.Env{Dialer: &checker.Dialer{}, UserAgent: "test"}, NodeID: "n1",
		Authenticator: authn, Keyring: base.keyring, Alerts: alert.NewSender(&checker.Dialer{}, logger),
		Local: local,
	}
	mux := http.NewServeMux()
	Register(mux, cfg, logger, deps)
	e.handler = mux
	return e
}

// TestSwitchLocalToOIDC covers the switch from AUTH_MODE=local to oidc and
// back on one state (FA-AUTH-08, ADR 0032): the data of default stays, local
// refresh sessions are revoked, API tokens keep working, and a switch back to
// local accepts only passwords again.
func TestSwitchLocalToOIDC(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	const adminName = "root-admin"
	password := "pw-" + uuid.NewString()

	// Phase 1: local mode. The admin, a monitor, a channel, an API token and
	// a refresh session exist.
	e := newEnvWith(t, true)
	setup := e.setup(adminName, password)
	if setup.code != http.StatusCreated {
		t.Fatalf("setup = %d %s", setup.code, setup.body)
	}
	admin := decode[gen.TokenResponse](t, setup).AccessToken
	localJWT, oldRefresh := e.mustLogin(adminName, password)
	if oldRefresh == nil {
		t.Fatal("login set no refresh cookie")
	}

	monRes := e.do("POST", "/api/v1/monitors", "", httpMonitor("site", "https://example.com"), bearer(admin)...)
	if monRes.code != http.StatusCreated {
		t.Fatalf("create monitor = %d %s", monRes.code, monRes.body)
	}
	mon := decode[gen.Monitor](t, monRes)
	chRes := e.do("POST", channelsPath, "", map[string]any{"name": "ops", "kind": "shoutrrr", "url": shoutrrrURL}, bearer(admin)...)
	if chRes.code != http.StatusCreated {
		t.Fatalf("create channel = %d %s", chRes.code, chRes.body)
	}
	channel := decode[gen.NotificationChannel](t, chRes)

	raiseCommandLevel(t, e, 9)
	tokRes := e.do("POST", apiTokensPath, "", tokenRequest("ci", "tenant-editor", 30*day), bearer(admin)...)
	if tokRes.code != http.StatusCreated {
		t.Fatalf("create token = %d %s", tokRes.code, tokRes.body)
	}
	apiToken := decode[gen.ApiTokenCreated](t, tokRes).Token
	if !auth.IsAPITokenSecret(apiToken) {
		t.Fatalf("token %q does not have the uct_ prefix", apiToken)
	}

	users, err := e.state.ListUnrevokedSessionUsers(ctx)
	if err != nil || len(users) != 1 {
		t.Fatalf("before the switch: session users = %v, %v; want 1", users, err)
	}

	// Phase 2: oidc mode on the same state.
	idp := oidctest.New(t)
	authn := newIDPAuthenticator(t, idp, e.state)
	oidcCfg := config.Config{NodeName: "n1", Role: config.RoleVoter, AuthMode: config.AuthOIDC}
	o := remount(t, e, oidcCfg, nil, auth.WithAPITokens(authn, e.state, time.Now))
	viewer := idpToken(idp, "idp-viewer-1", string(auth.RoleTenantViewer))

	n, err := authswitch.RevokeOnce(ctx, logger, e.node, e.state)
	if err != nil || n != 1 {
		t.Fatalf("RevokeOnce = %d, %v; want 1, nil", n, err)
	}
	if left, err := e.state.ListUnrevokedSessionUsers(ctx); err != nil || len(left) != 0 {
		t.Fatalf("after RevokeOnce: session users = %v, %v; want none", left, err)
	}

	t.Run("data unchanged for a default principal from the IdP", func(t *testing.T) {
		res := o.do("GET", "/api/v1/monitors", "", nil, bearer(viewer)...)
		if res.code != http.StatusOK {
			t.Fatalf("list monitors = %d %s", res.code, res.body)
		}
		if !bytes.Contains(res.body, []byte(mon.Id.String())) {
			t.Errorf("monitor %s missing in %s", mon.Id, res.body)
		}
		res = o.do("GET", "/api/v1/notifications", "", nil, bearer(viewer)...)
		if res.code != http.StatusOK {
			t.Fatalf("list channels = %d %s", res.code, res.body)
		}
		if !bytes.Contains(res.body, []byte(channel.Id.String())) {
			t.Errorf("channel %s missing in %s", channel.Id, res.body)
		}
	})

	t.Run("local access token is rejected by the oidc authenticator", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/monitors", nil)
		req.Header.Set("Authorization", "Bearer "+localJWT)
		if _, err := authn.Authenticate(req); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("authenticate local JWT: err = %v, want ErrUnauthenticated", err)
		}
		if res := o.do("GET", "/api/v1/monitors", "", nil, bearer(localJWT)...); res.code != http.StatusUnauthorized {
			t.Errorf("local JWT over HTTP = %d, want 401", res.code)
		}
	})

	t.Run("API token keeps working", func(t *testing.T) {
		res := o.do("GET", "/api/v1/monitors", "", nil, bearer(apiToken)...)
		if res.code != http.StatusOK {
			t.Fatalf("list monitors with API token = %d %s", res.code, res.body)
		}
		if !bytes.Contains(res.body, []byte(mon.Id.String())) {
			t.Errorf("monitor missing for API token")
		}
	})

	t.Run("refresh is refused in oidc mode", func(t *testing.T) {
		res := o.do("POST", "/api/v1/auth/refresh", "", nil, append(requestedWith, cookieHeader(oldRefresh)...)...)
		if res.code != http.StatusConflict {
			t.Errorf("refresh = %d, want 409", res.code)
		}
	})

	t.Run("second RevokeOnce revokes nothing", func(t *testing.T) {
		n, err := authswitch.RevokeOnce(ctx, logger, e.node, e.state)
		if err != nil || n != 0 {
			t.Errorf("second RevokeOnce = %d, %v; want 0, nil", n, err)
		}
	})

	// Phase 3: back to local mode on the same state (emergency access, ADR 0019).
	l := remount(t, e, config.Config{NodeName: "n1", Role: config.RoleVoter, AuthMode: config.AuthLocal}, e.local, e.local)

	t.Run("old refresh cookie is refused after switching back", func(t *testing.T) {
		res := l.do("POST", "/api/v1/auth/refresh", "", nil, append(requestedWith, cookieHeader(oldRefresh)...)...)
		if res.code != http.StatusUnauthorized {
			t.Errorf("refresh with revoked cookie = %d %s, want 401", res.code, res.body)
		}
	})

	t.Run("password login works again and data is still there", func(t *testing.T) {
		token, _ := l.mustLogin(adminName, password)
		res := l.do("GET", "/api/v1/monitors", "", nil, bearer(token)...)
		if res.code != http.StatusOK {
			t.Fatalf("list monitors after login = %d %s", res.code, res.body)
		}
		if !bytes.Contains(res.body, []byte(mon.Id.String())) {
			t.Errorf("monitor %s missing after switching back", mon.Id)
		}
	})
}
