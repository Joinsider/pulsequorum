// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package auth

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/store"
)

// API tokens (ADR 0031, FA-AUTH-10) are long-lived service tokens of a
// tenant. The secret is "uct_" followed by 256 random bits; only its SHA-256
// is stored and replicated.
const (
	// APITokenPrefix starts every API token secret. It lets secret scanners
	// find leaked tokens and separates tokens from JWTs.
	APITokenPrefix = "uct_"
	// APITokenIDPrefix starts the public ID of an API token.
	APITokenIDPrefix = "tok_"
	// APITokenScopeTenant is the only scope until a later ADR adds more.
	APITokenScopeTenant = cluster.APITokenScopeTenant
	// APITokenCommandLevel is the command level from which the cluster
	// stores API tokens (ADR 0031). Tokens are not accepted below it.
	APITokenCommandLevel = cluster.AuditCommandLevel
)

// apiTokenSecretLen is the length of "uct_" + base64url(32 bytes).
const apiTokenSecretLen = len(APITokenPrefix) + 43

// ErrInvalidAPIToken is the single answer for unknown, malformed and expired
// tokens, so a caller cannot tell which case applies.
var ErrInvalidAPIToken = fmt.Errorf("%w: invalid or expired API token", ErrUnauthenticated)

// ErrTokenNotAllowed rejects API tokens on operations reserved for
// interactive logins (ADR 0031): token and user management, admin and auth
// endpoints.
var ErrTokenNotAllowed = fmt.Errorf("%w: this operation is not available to API tokens", ErrForbidden)

// NewAPIToken returns a new public ID, the secret that is shown once and the
// hash that is stored.
func NewAPIToken() (id, secret, secretHash string, err error) {
	raw, err := NewOpaqueToken()
	if err != nil {
		return "", "", "", err
	}
	b := make([]byte, 10) // 80 bits, 16 base32 characters
	if _, err := rand.Read(b); err != nil {
		return "", "", "", fmt.Errorf("random token ID: %w", err)
	}
	id = APITokenIDPrefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
	secret = APITokenPrefix + raw
	return id, secret, HashToken(secret), nil
}

// IsAPITokenSecret reports whether a bearer value is meant as an API token.
// It only looks at the prefix; Authenticate checks the rest.
func IsAPITokenSecret(bearer string) bool {
	return strings.HasPrefix(bearer, APITokenPrefix)
}

// ValidAPITokenRole reports whether role may be given to an API token: tenant
// roles only, never platform-admin (ADR 0031, option 2A). The FSM applies the
// same rule.
func ValidAPITokenRole(role string) bool { return cluster.ValidAPITokenRole(role) }

// APITokenState is what the token authenticator reads from state.db.
type APITokenState interface {
	GetAPITokenBySecretHash(ctx context.Context, secretHash string) (store.APIToken, error)
	GetTenant(ctx context.Context, id string) (store.Tenant, error)
	CommandLevel(ctx context.Context) (uint32, error)
}

// WithAPITokens accepts API tokens in front of another authenticator. Bearer
// values with the token prefix are checked against state.db; all other
// requests go to next unchanged.
func WithAPITokens(next Authenticator, state APITokenState, now func() time.Time) Authenticator {
	if now == nil {
		now = time.Now
	}
	return &apiTokenAuthenticator{next: next, state: state, now: now}
}

type apiTokenAuthenticator struct {
	next  Authenticator
	state APITokenState
	now   func() time.Time
}

// Authenticate implements Authenticator.
func (a *apiTokenAuthenticator) Authenticate(r *http.Request) (Principal, error) {
	secret, ok := bearerValue(r)
	if !ok || !IsAPITokenSecret(secret) {
		return a.next.Authenticate(r)
	}
	return a.authenticateToken(r.Context(), secret)
}

// authenticateToken checks an API token secret.
func (a *apiTokenAuthenticator) authenticateToken(ctx context.Context, secret string) (Principal, error) {
	if len(secret) != apiTokenSecretLen {
		return Principal{}, ErrInvalidAPIToken
	}
	level, err := a.state.CommandLevel(ctx)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: command level: %w", ErrAuthUnavailable, err)
	}
	if level < APITokenCommandLevel {
		return Principal{}, ErrInvalidAPIToken
	}
	// The lookup is by the SHA-256 of a 256-bit secret; there is nothing to
	// learn from timing.
	t, err := a.state.GetAPITokenBySecretHash(ctx, HashToken(secret))
	switch {
	case errors.Is(err, store.ErrNotFound):
		return Principal{}, ErrInvalidAPIToken
	case err != nil:
		return Principal{}, fmt.Errorf("%w: token lookup: %w", ErrAuthUnavailable, err)
	}
	now := a.now()
	expires := time.UnixMilli(t.ExpiresMs)
	if !now.Before(expires) || t.Scope != APITokenScopeTenant || !ValidAPITokenRole(t.Role) ||
		t.TenantID == "" || t.TenantID == store.SystemTenantID {
		return Principal{}, ErrInvalidAPIToken
	}
	tn, err := a.state.GetTenant(ctx, t.TenantID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return Principal{}, ErrInvalidAPIToken
	case err != nil:
		return Principal{}, fmt.Errorf("%w: tenant lookup: %w", ErrAuthUnavailable, err)
	case tn.Suspended:
		return Principal{}, ErrTenantSuspended
	}
	return Principal{
		UserID:    t.ID,
		Username:  t.Name,
		TenantID:  t.TenantID,
		Roles:     []Role{Role(t.Role)},
		ExpiresAt: expires,
		ViaToken:  true,
	}, nil
}

// bearerValue returns the value of an "Authorization: Bearer" header, matched
// like the local and OIDC authenticators do.
func bearerValue(r *http.Request) (string, bool) {
	return strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// RequireInteractive rejects API tokens (ADR 0031). Token and user
// management, admin endpoints and tenant administration call it so that a
// leaked token can neither extend itself nor create accounts.
func RequireInteractive(p Principal) error {
	if p.ViaToken {
		return ErrTokenNotAllowed
	}
	return nil
}
