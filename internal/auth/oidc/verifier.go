// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package oidc authenticates requests with JWT access tokens of an external
// OpenID Connect provider (ADR 0027). Signature and standard claims are
// checked by go-oidc's verifier with our own key set; the extra claims, the
// tenant and the roles are checked here. The verifier never calls the
// provider per request.
package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"

	"github.com/joinsider/pulsequorum/internal/auth"
	"github.com/joinsider/pulsequorum/internal/store"
)

const (
	// maxTokenSize bounds the bearer token before any parsing.
	maxTokenSize = 16 << 10
	clockLeeway  = 30 * time.Second
)

// TenantStore is the read access the verifier needs.
type TenantStore interface {
	GetTenant(ctx context.Context, id string) (store.Tenant, error)
	GetTenantByOrg(ctx context.Context, orgID string) (store.Tenant, error)
}

// Options configures the Authenticator.
type Options struct {
	Issuer      string
	Audience    string
	ClientIDs   []string
	RequireAZP  bool
	Algorithms  []string
	MaxTokenTTL time.Duration
	Profile     Profile
	Tenants     TenantStore
	Keys        *KeySet
	// UserInfo, if set, supplies the claims the profile maps instead of the
	// access token (OIDC_ROLES_SOURCE=userinfo, ADR 0028).
	UserInfo *UserInfo
	Now      func() time.Time
	Log      *slog.Logger
}

// Authenticator implements auth.Authenticator for OIDC access tokens.
type Authenticator struct {
	opts     Options
	verifier *gooidc.IDTokenVerifier
}

// verifierConfig is the go-oidc configuration. None of the Skip* switches
// and not InsecureSkipSignatureCheck may ever be set; a test enforces it.
func verifierConfig(opts Options) *gooidc.Config {
	return &gooidc.Config{
		ClientID:             opts.Audience,
		SupportedSigningAlgs: slices.Clone(opts.Algorithms),
		Now:                  opts.Now,
	}
}

// NewAuthenticator checks the options and returns the authenticator.
func NewAuthenticator(opts Options) (*Authenticator, error) {
	switch {
	case opts.Issuer == "", opts.Audience == "", len(opts.ClientIDs) == 0:
		return nil, errors.New("oidc: issuer, audience and client IDs are required")
	case opts.MaxTokenTTL <= 0:
		return nil, errors.New("oidc: maximum token lifetime is required")
	case opts.Profile == nil, opts.Tenants == nil, opts.Keys == nil:
		return nil, errors.New("oidc: profile, tenant store and key set are required")
	}
	if _, err := signatureAlgorithms(opts.Algorithms); err != nil {
		return nil, err
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &Authenticator{opts: opts, verifier: gooidc.NewVerifier(opts.Issuer, opts.Keys, verifierConfig(opts))}, nil
}

// tokenClaims are the claims checked in addition to go-oidc's checks.
type tokenClaims struct {
	Subject   string          `json:"sub"`
	AZP       string          `json:"azp"`
	ClientID  string          `json:"client_id"` // RFC 9068; used like azp (ADR 0028)
	IssuedAt  *int64          `json:"iat"`
	NotBefore *int64          `json:"nbf"`
	ExpiresAt int64           `json:"exp"`
	Nonce     json.RawMessage `json:"nonce"`
	AtHash    json.RawMessage `json:"at_hash"`
}

// Authenticate implements auth.Authenticator. Errors wrap
// auth.ErrUnauthenticated (401), auth.ErrForbidden (403) or
// auth.ErrAuthUnavailable (503). Reasons for 401 are logged, not returned.
func (a *Authenticator) Authenticate(r *http.Request) (auth.Principal, error) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	if len(token) > maxTokenSize {
		return a.reject("token too large")
	}
	if strings.Count(token, ".") != 2 {
		return auth.Principal{}, auth.ErrOpaqueToken
	}
	if !a.opts.Keys.Ready() {
		return auth.Principal{}, auth.ErrAuthUnavailable
	}
	ctx := r.Context()
	idt, err := a.verifier.Verify(ctx, token)
	if err != nil {
		return a.reject(err.Error())
	}
	var c tokenClaims
	if err := idt.Claims(&c); err != nil {
		return a.reject("claims: " + err.Error())
	}
	if reason := a.checkClaims(c); reason != "" {
		return a.reject(reason)
	}
	var raw map[string]json.RawMessage
	if err := idt.Claims(&raw); err != nil {
		return a.reject("claims: " + err.Error())
	}
	if a.opts.UserInfo != nil {
		// Only after the token passed every check: invalid tokens never
		// reach the provider (ADR 0028).
		info, err := a.opts.UserInfo.Claims(ctx, token, c.Subject, time.Unix(c.ExpiresAt, 0))
		switch {
		case errors.Is(err, auth.ErrAuthUnavailable):
			a.opts.Log.Warn("oidc: userinfo unavailable", "err", err)
			return auth.Principal{}, auth.ErrAuthUnavailable
		case err != nil:
			return a.reject("userinfo: " + err.Error())
		}
		raw = info
	}
	id, err := a.opts.Profile.Map(ctx, raw)
	if err != nil {
		return a.deny(c.Subject, err)
	}
	if err := a.checkTenant(ctx, id.TenantID); err != nil {
		return a.deny(c.Subject, err)
	}
	return auth.Principal{
		UserID: c.Subject, Username: c.Subject, TenantID: id.TenantID, Roles: id.Roles,
		ExpiresAt: time.Unix(c.ExpiresAt, 0),
	}, nil
}

// checkClaims returns why the claims are not acceptable, or "".
func (a *Authenticator) checkClaims(c tokenClaims) string {
	now := a.opts.Now().Unix()
	leeway := int64(clockLeeway / time.Second)
	switch {
	case c.Subject == "":
		return "no subject"
	// ID tokens carry nonce or at_hash; they must not be used as access tokens.
	case len(c.Nonce) > 0, len(c.AtHash) > 0:
		return "ID token presented as access token"
	case c.IssuedAt == nil:
		return "no iat"
	case *c.IssuedAt > now+leeway:
		return "issued in the future"
	case c.NotBefore != nil && *c.NotBefore > now+leeway:
		return "not valid yet"
	// The same leeway applies: PocketID issues "1h" tokens with exp-iat of
	// 3601s because iat and exp are rounded separately.
	case c.ExpiresAt-*c.IssuedAt > int64(a.opts.MaxTokenTTL/time.Second)+leeway:
		return fmt.Sprintf("lifetime %ds exceeds the maximum %s", c.ExpiresAt-*c.IssuedAt, a.opts.MaxTokenTTL)
	}
	// The client is azp, or client_id from RFC 9068 tokens (ADR 0028).
	client := c.AZP
	switch {
	case c.AZP != "" && c.ClientID != "" && c.AZP != c.ClientID:
		return "azp and client_id differ"
	case client == "":
		client = c.ClientID
	}
	switch {
	case client == "" && a.opts.RequireAZP:
		return "no azp or client_id"
	case client != "" && !slices.Contains(a.opts.ClientIDs, client):
		return "client is not allowed"
	}
	return ""
}

// checkTenant rejects unknown, suspended and reserved tenants.
func (a *Authenticator) checkTenant(ctx context.Context, tenantID string) error {
	if tenantID == "" || tenantID == store.SystemTenantID {
		return fmt.Errorf("%w: no tenant", auth.ErrForbidden)
	}
	t, err := a.opts.Tenants.GetTenant(ctx, tenantID)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: tenant does not exist", auth.ErrForbidden)
	}
	if err != nil {
		a.opts.Log.Error("oidc: tenant lookup failed", "tenant", tenantID, "err", err)
		return auth.ErrAuthUnavailable
	}
	if t.Suspended {
		return fmt.Errorf("%w: tenant is suspended", auth.ErrForbidden)
	}
	return nil
}

func (a *Authenticator) reject(reason string) (auth.Principal, error) {
	a.opts.Log.Info("oidc: token rejected", "reason", reason)
	return auth.Principal{}, auth.ErrUnauthenticated
}

func (a *Authenticator) deny(subject string, err error) (auth.Principal, error) {
	a.opts.Log.Info("oidc: access denied", "sub", subject, "err", err)
	if errors.Is(err, auth.ErrForbidden) || errors.Is(err, auth.ErrAuthUnavailable) {
		return auth.Principal{}, err
	}
	return auth.Principal{}, auth.ErrForbidden
}
