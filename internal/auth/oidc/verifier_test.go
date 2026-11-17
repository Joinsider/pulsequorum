// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package oidc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joinsider/uptimecluster/internal/auth"
	"github.com/joinsider/uptimecluster/internal/auth/oidctest"
	"github.com/joinsider/uptimecluster/internal/store"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type harness struct {
	t       testing.TB
	idp     *oidctest.Server
	clock   *clock
	keys    *KeySet
	authn   *Authenticator
	cache   string
	tenants fakeTenants
}

type harnessOpt func(*Options, *KeySetOptions)

func newHarness(t testing.TB, opts ...harnessOpt) *harness {
	t.Helper()
	return newHarnessWith(t, nil, opts...)
}

// newHarnessWith lets before prepare the provider before the keys are loaded.
func newHarnessWith(t testing.TB, before func(*oidctest.Server), opts ...harnessOpt) *harness {
	t.Helper()
	h := &harness{t: t, idp: oidctest.New(t), clock: &clock{now: time.Unix(1_800_000_000, 0)},
		cache:   filepath.Join(t.TempDir(), "oidc-jwks.json"),
		tenants: fakeTenants{"default": {ID: "default"}}}
	if before != nil {
		before(h.idp)
	}
	h.keys, h.authn = h.build(opts...)
	if err := h.keys.Load(context.Background()); err != nil {
		t.Fatalf("load keys: %v", err)
	}
	return h
}

// build creates a key set and authenticator against the fake provider without
// loading keys.
func (h *harness) build(opts ...harnessOpt) (*KeySet, *Authenticator) {
	h.t.Helper()
	o := Options{
		Issuer: h.idp.Issuer(), Audience: "project-1", ClientIDs: []string{"spa-client"}, RequireAZP: true,
		Algorithms: []string{AlgRS256}, MaxTokenTTL: time.Hour, Now: h.clock.Now, Log: quiet,
		Profile: Generic{TenantID: "default", RolesClaim: "roles"}, Tenants: h.tenants,
	}
	ko := KeySetOptions{Issuer: h.idp.Issuer(), Client: h.idp.Client(), CacheFile: h.cache, Now: h.clock.Now, Log: quiet}
	for _, fn := range opts {
		fn(&o, &ko)
	}
	ko.Algorithms = o.Algorithms
	ks, err := NewKeySet(ko)
	if err != nil {
		h.t.Fatal(err)
	}
	o.Keys = ks
	a, err := NewAuthenticator(o)
	if err != nil {
		h.t.Fatal(err)
	}
	return ks, a
}

func (h *harness) claims() map[string]any {
	c := h.idp.Claims("user-1", 15*time.Minute, h.clock.Now())
	c["roles"] = []string{"tenant-editor"}
	return c
}

func (h *harness) authenticate(token string) (auth.Principal, error) {
	return authenticateWith(h.authn, token)
}

func authenticateWith(a *Authenticator, token string) (auth.Principal, error) {
	r, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/monitors", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return a.Authenticate(r)
}

func TestAuthenticateValidToken(t *testing.T) {
	h := newHarness(t)
	p, err := h.authenticate(h.idp.Sign(h.claims()))
	if err != nil {
		t.Fatal(err)
	}
	want := auth.Principal{UserID: "user-1", Username: "user-1", TenantID: "default",
		Roles: []auth.Role{auth.RoleTenantEditor}, ExpiresAt: h.clock.Now().Add(15 * time.Minute)}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("principal = %+v, want %+v", p, want)
	}
}

// TestAuthenticateRejects covers every verification rule of ADR 0027
// (FA-AUTH-05/06, PF-31).
func TestAuthenticateRejects(t *testing.T) {
	h := newHarness(t)
	esKid := h.idp.AddKey(AlgES256) // published, but ES256 is not allowed
	weakKid := h.idp.AddRSAKey(1024)
	rsaKid := h.idp.AddKey(AlgRS256)
	if err := h.keys.Load(context.Background()); err != nil { // pick up the new keys
		t.Fatal(err)
	}
	h.clock.Advance(time.Minute)
	now := h.clock.Now().Unix()

	with := func(mut func(map[string]any)) string {
		c := h.claims()
		mut(c)
		return h.idp.SignWith(rsaKid, nil, c)
	}
	tests := []struct {
		name  string
		token func() string
		want  error
	}{
		{"no token", func() string { return "" }, auth.ErrUnauthenticated},
		{"opaque token", func() string { return "dGhpcyBpcyBvcGFxdWU" }, auth.ErrOpaqueToken},
		{"too large", func() string { return strings.Repeat("a", 8<<10) + "." + strings.Repeat("b", 9<<10) + ".c" }, auth.ErrUnauthenticated},
		{"alg none", func() string { return h.idp.SignNone(h.claims()) }, auth.ErrUnauthenticated},
		{"HS256 keyed with the public key", func() string { return h.idp.SignHS256WithPublicKey(rsaKid, h.claims()) }, auth.ErrUnauthenticated},
		{"header alg differs from key", func() string {
			return h.idp.SignWith(rsaKid, map[string]any{"alg": "ES256"}, h.claims())
		}, auth.ErrUnauthenticated},
		{"algorithm not allowed", func() string { return h.idp.SignWith(esKid, nil, h.claims()) }, auth.ErrUnauthenticated},
		{"RSA key below 2048 bits", func() string { return h.idp.SignWith(weakKid, nil, h.claims()) }, auth.ErrUnauthenticated},
		{"no kid", func() string { return h.idp.SignWith(rsaKid, map[string]any{"kid": nil}, h.claims()) }, auth.ErrUnauthenticated},
		{"tampered payload", func() string {
			good := strings.Split(h.idp.SignWith(rsaKid, nil, h.claims()), ".")
			other := strings.Split(with(func(c map[string]any) { c["roles"] = []string{"platform-admin"} }), ".")
			return good[0] + "." + other[1] + "." + good[2]
		}, auth.ErrUnauthenticated},
		{"wrong issuer", func() string { return with(func(c map[string]any) { c["iss"] = "https://evil.example.test" }) }, auth.ErrUnauthenticated},
		{"issuer with trailing slash", func() string { return with(func(c map[string]any) { c["iss"] = h.idp.Issuer() + "/" }) }, auth.ErrUnauthenticated},
		{"foreign audience", func() string {
			return with(func(c map[string]any) { c["aud"] = []string{"other-project", "spa-client"} })
		}, auth.ErrUnauthenticated},
		{"no audience", func() string { return with(func(c map[string]any) { delete(c, "aud") }) }, auth.ErrUnauthenticated},
		{"foreign azp", func() string { return with(func(c map[string]any) { c["azp"] = "other-app" }) }, auth.ErrUnauthenticated},
		{"no azp", func() string { return with(func(c map[string]any) { delete(c, "azp") }) }, auth.ErrUnauthenticated},
		{"expired", func() string { return with(func(c map[string]any) { c["iat"] = now - 3600; c["exp"] = now - 1 }) }, auth.ErrUnauthenticated},
		{"no exp", func() string { return with(func(c map[string]any) { delete(c, "exp") }) }, auth.ErrUnauthenticated},
		{"not valid yet", func() string { return with(func(c map[string]any) { c["nbf"] = now + 120 }) }, auth.ErrUnauthenticated},
		{"issued in the future", func() string { return with(func(c map[string]any) { c["iat"] = now + 120 }) }, auth.ErrUnauthenticated},
		{"no iat", func() string { return with(func(c map[string]any) { delete(c, "iat") }) }, auth.ErrUnauthenticated},
		{"lifetime above maximum", func() string { return with(func(c map[string]any) { c["iat"] = now; c["exp"] = now + 12*3600 }) }, auth.ErrUnauthenticated},
		{"no subject", func() string { return with(func(c map[string]any) { delete(c, "sub") }) }, auth.ErrUnauthenticated},
		{"ID token with nonce", func() string { return with(func(c map[string]any) { c["nonce"] = "n-1" }) }, auth.ErrUnauthenticated},
		{"ID token with at_hash", func() string { return with(func(c map[string]any) { c["at_hash"] = "x" }) }, auth.ErrUnauthenticated},
		{"no role", func() string { return with(func(c map[string]any) { c["roles"] = []string{"nobody"} }) }, auth.ErrForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := h.authenticate(tc.token())
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v (principal %+v)", err, tc.want, p)
			}
			if errors.Is(tc.want, auth.ErrUnauthenticated) && errors.Is(err, auth.ErrForbidden) {
				t.Fatalf("err = %v, want plain 401", err)
			}
			if p.TenantID != "" || p.UserID != "" {
				t.Fatalf("principal %+v returned with error", p)
			}
		})
	}
}

func TestAuthenticateTenantChecks(t *testing.T) {
	tests := []struct {
		name    string
		tenants fakeTenants
		want    error
	}{
		{"suspended tenant", fakeTenants{"default": {ID: "default", Suspended: true}}, auth.ErrForbidden},
		{"missing tenant", fakeTenants{}, auth.ErrForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(o *Options, _ *KeySetOptions) { o.Tenants = tc.tenants })
			if _, err := h.authenticate(h.idp.Sign(h.claims())); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	t.Run("system tenant", func(t *testing.T) {
		h := newHarness(t, func(o *Options, _ *KeySetOptions) {
			o.Profile = Generic{TenantID: store.SystemTenantID, RolesClaim: "roles"}
			o.Tenants = fakeTenants{store.SystemTenantID: {ID: store.SystemTenantID}}
		})
		if _, err := h.authenticate(h.idp.Sign(h.claims())); !errors.Is(err, auth.ErrForbidden) {
			t.Fatalf("err = %v, want ErrForbidden", err)
		}
	})
}

func TestAuthenticateWithoutAZPWhenAllowed(t *testing.T) {
	h := newHarness(t, func(o *Options, _ *KeySetOptions) { o.RequireAZP = false })
	c := h.claims()
	delete(c, "azp")
	if _, err := h.authenticate(h.idp.Sign(c)); err != nil {
		t.Fatalf("err = %v, want accepted", err)
	}
	c["azp"] = "other-app"
	if _, err := h.authenticate(h.idp.Sign(c)); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("foreign azp: err = %v, want rejected", err)
	}
}

func TestAuthenticateOtherAlgorithms(t *testing.T) {
	for _, alg := range []string{AlgES256, AlgEdDSA} {
		t.Run(alg, func(t *testing.T) {
			h := newHarnessWith(t, func(s *oidctest.Server) { s.AddKey(alg) },
				func(o *Options, _ *KeySetOptions) { o.Algorithms = []string{alg} })
			if _, err := h.authenticate(h.idp.Sign(h.claims())); err != nil {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestKeyRotationAndRefetchLimit(t *testing.T) {
	h := newHarness(t)
	base := h.idp.JWKSRequests()

	// A rotated key is fetched on first use.
	h.clock.Advance(time.Minute)
	kid := h.idp.AddKey(AlgRS256)
	if _, err := h.authenticate(h.idp.SignWith(kid, nil, h.claims())); err != nil {
		t.Fatalf("rotated key: %v", err)
	}
	if got := h.idp.JWKSRequests() - base; got != 1 {
		t.Fatalf("JWKS requests after rotation = %d, want 1", got)
	}

	// Unknown key IDs within the limit do not reach the provider.
	for range 20 {
		_, err := h.authenticate(h.idp.SignWith(kid, map[string]any{"kid": "unknown"}, h.claims()))
		if !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatalf("unknown kid: err = %v", err)
		}
	}
	if got := h.idp.JWKSRequests() - base; got != 1 {
		t.Fatalf("JWKS requests after unknown kids = %d, want 1", got)
	}

	// After the limit, one more fetch is allowed.
	h.clock.Advance(defaultMinRefetch)
	_, _ = h.authenticate(h.idp.SignWith(kid, map[string]any{"kid": "unknown"}, h.claims()))
	if got := h.idp.JWKSRequests() - base; got != 2 {
		t.Fatalf("JWKS requests after the limit = %d, want 2", got)
	}
}

func TestUnknownKeyConcurrentSingleFetch(t *testing.T) {
	h := newHarness(t)
	h.clock.Advance(time.Minute)
	kid := h.idp.AddKey(AlgRS256)
	token := h.idp.SignWith(kid, nil, h.claims())
	base := h.idp.JWKSRequests()
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for range 32 {
		wg.Go(func() {
			_, err := h.authenticate(token)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("err = %v", err)
		}
	}
	if got := h.idp.JWKSRequests() - base; got != 1 {
		t.Fatalf("JWKS requests = %d, want 1", got)
	}
}

func TestRemovedKeyIsRejectedAfterRefresh(t *testing.T) {
	h := newHarness(t)
	old := h.idp.Sign(h.claims())
	h.idp.AddKey(AlgRS256) // new active key
	h.idp.Unpublish(kidOf(t, old))
	h.clock.Advance(time.Minute)
	if err := h.keys.fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.authenticate(old); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("token of a removed key: err = %v, want rejected", err)
	}
}

// TestProviderOutage covers FA-AUTH-07: known keys keep working, failed
// fetches keep the last keys, and a restart during the outage uses the cache.
func TestProviderOutage(t *testing.T) {
	var failures int
	h := newHarness(t, func(_ *Options, ko *KeySetOptions) { ko.OnFetchFailure = func() { failures++ } })
	token := h.idp.Sign(h.claims())
	h.idp.SetDown(true)

	h.clock.Advance(time.Minute)
	if err := h.keys.fetch(context.Background()); err == nil {
		t.Fatal("fetch while down succeeded")
	}
	if failures != 1 {
		t.Fatalf("failures = %d, want 1", failures)
	}
	if _, err := h.authenticate(token); err != nil {
		t.Fatalf("token during outage: %v", err)
	}

	// Restart: a new key set loads the cache file while the provider is down.
	ks, a := h.build()
	if err := ks.Load(context.Background()); err != nil {
		t.Fatalf("load from cache: %v", err)
	}
	if _, err := authenticateWith(a, token); err != nil {
		t.Fatalf("token after restart during outage: %v", err)
	}
	info, err := os.Stat(h.cache)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != cacheFileMode {
		t.Fatalf("cache file mode = %v, want %v", mode, os.FileMode(cacheFileMode))
	}

	// Status errors keep the keys too.
	h.idp.SetDown(false)
	h.idp.SetStatus(http.StatusInternalServerError)
	h.clock.Advance(time.Minute)
	if err := h.keys.fetch(context.Background()); err == nil {
		t.Fatal("fetch with status 500 succeeded")
	}
	if _, err := h.authenticate(token); err != nil {
		t.Fatalf("token after failed refresh: %v", err)
	}
}

func TestNoKeysIsUnavailable(t *testing.T) {
	h := newHarness(t)
	h.idp.SetDown(true)
	h.cache = filepath.Join(t.TempDir(), "none.json")
	ks, a := h.build()
	if err := ks.Load(context.Background()); !errors.Is(err, ErrNoKeys) {
		t.Fatalf("load = %v, want ErrNoKeys", err)
	}
	if _, err := authenticateWith(a, h.idp.Sign(h.claims())); !errors.Is(err, auth.ErrAuthUnavailable) {
		t.Fatalf("err = %v, want ErrAuthUnavailable", err)
	}
}

func TestDiscoveryIssuerMismatch(t *testing.T) {
	h := newHarness(t)
	h.idp.SetIssuerOverride(h.idp.Issuer() + "/")
	h.cache = ""
	ks, _ := h.build(func(_ *Options, ko *KeySetOptions) { ko.CacheFile = "" })
	if err := ks.Load(context.Background()); !errors.Is(err, ErrNoKeys) || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("load = %v, want issuer mismatch", err)
	}
}

func TestCacheOfOtherIssuerIgnored(t *testing.T) {
	h := newHarness(t)
	other := oidctest.New(t)
	other.SetDown(true)
	ks, err := NewKeySet(KeySetOptions{Issuer: other.Issuer(), Algorithms: []string{AlgRS256}, Client: other.Client(),
		CacheFile: h.cache, Now: h.clock.Now, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Load(context.Background()); !errors.Is(err, ErrNoKeys) {
		t.Fatalf("load = %v, want ErrNoKeys (cache belongs to another issuer)", err)
	}
}

func TestPublishedAlgMismatchSkipsKey(t *testing.T) {
	h := newHarness(t)
	h.idp.SetPublishAlg(true)
	// A JWK that names RS256 must not verify a token claiming another alg,
	// even if the allow-list contains it.
	ks, a := h.build(func(o *Options, _ *KeySetOptions) { o.Algorithms = []string{AlgRS256, AlgES256} })
	if err := ks.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	tok := h.idp.SignWith("", map[string]any{"alg": "ES256"}, h.claims())
	if _, err := authenticateWith(a, tok); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("err = %v, want rejected", err)
	}
}

func TestRefreshIntervalHonorsMaxAge(t *testing.T) {
	h := newHarness(t)
	if got := h.keys.refreshInterval(); got != defaultRefresh {
		t.Fatalf("interval = %v, want %v", got, defaultRefresh)
	}
	h.idp.SetJWKSMaxAge(time.Minute)
	h.clock.Advance(time.Minute)
	if err := h.keys.fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.keys.refreshInterval(); got != time.Minute {
		t.Fatalf("interval = %v, want 1m", got)
	}
	h.idp.SetJWKSMaxAge(time.Second)
	h.clock.Advance(time.Minute)
	if err := h.keys.fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.keys.refreshInterval(); got != defaultMinRefetch {
		t.Fatalf("interval = %v, want the refetch minimum", got)
	}
}

// TestVerifierConfigHasNoSkips guards the go-oidc configuration: none of its
// switches that disable checks may be set (ADR 0027).
func TestVerifierConfigHasNoSkips(t *testing.T) {
	cfg := verifierConfig(Options{Audience: "project-1", Algorithms: []string{AlgRS256}})
	v := reflect.ValueOf(*cfg)
	for i := range v.NumField() {
		name := v.Type().Field(i).Name
		if (strings.HasPrefix(name, "Skip") || strings.HasPrefix(name, "Insecure")) && !v.Field(i).IsZero() {
			t.Errorf("go-oidc config field %s is set", name)
		}
	}
	if cfg.ClientID != "project-1" || !slices.Equal(cfg.SupportedSigningAlgs, []string{AlgRS256}) {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestNewAuthenticatorValidates(t *testing.T) {
	if _, err := NewAuthenticator(Options{}); err == nil {
		t.Fatal("empty options accepted")
	}
	if _, err := NewKeySet(KeySetOptions{Issuer: "https://x", Algorithms: []string{"HS256"}}); err == nil {
		t.Fatal("HS256 accepted")
	}
	if _, err := NewKeySet(KeySetOptions{Issuer: "https://x", Algorithms: []string{"none"}}); err == nil {
		t.Fatal("none accepted")
	}
}

func kidOf(t *testing.T, token string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[0])
	if err != nil {
		t.Fatal(err)
	}
	var hdr struct {
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(raw, &hdr); err != nil || hdr.Kid == "" {
		t.Fatalf("no kid in %s: %v", raw, err)
	}
	return hdr.Kid
}
