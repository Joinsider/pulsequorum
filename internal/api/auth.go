// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package api

import (
	"context"
	"errors"
	"math"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/joinsider/uptimecluster/internal/api/gen"
	"github.com/joinsider/uptimecluster/internal/auth"
	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/config"
	"github.com/joinsider/uptimecluster/internal/store"
)

// Local authentication endpoints (ADR 0008, FA-AUTH-01..04). The refresh
// token only ever travels in an HttpOnly, Secure, SameSite=Strict cookie
// scoped to /api/v1/auth; access tokens are returned in the body and kept in
// memory by the SPA (NFA-SEC-04).

const (
	refreshCookie = "uc_refresh"
	refreshPath   = "/api/v1/auth"
	csrfHeader    = "uptimecluster"
)

func unauthorized(detail string) *Problem {
	return &Problem{Status: http.StatusUnauthorized, Title: "Unauthorized", Detail: detail}
}

// errManagedByIDP answers local login and user management while an external
// identity provider manages the users (ADR 0027).
func errManagedByIDP() *Problem {
	return &Problem{Status: http.StatusConflict, Title: "Conflict", Detail: "users are managed by the identity provider"}
}

// localAuth returns the local authentication, 409 while an identity provider
// manages the users, or 404 if local authentication is not available.
func (h *Handlers) localAuth() (*auth.Local, error) {
	if h.cfg.AuthMode == config.AuthOIDC {
		return nil, errManagedByIDP()
	}
	if h.deps.Local == nil {
		return nil, &Problem{Status: http.StatusNotFound, Title: "Not Found", Detail: "local authentication is not enabled"}
	}
	return h.deps.Local, nil
}

func (h *Handlers) setRefreshCookie(ctx context.Context, token string, expires time.Time) {
	w, _, ok := httpFrom(ctx)
	if !ok {
		return
	}
	c := &http.Cookie{
		Name: refreshCookie, Value: token, Path: refreshPath, Domain: h.cfg.AuthCookieDomain,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	}
	if token == "" {
		c.MaxAge = -1
	} else {
		c.Expires = expires.UTC()
		c.MaxAge = int(expires.Sub(h.deps.Now()).Seconds())
	}
	http.SetCookie(w, c)
}

func refreshFromCookie(ctx context.Context) string {
	_, r, ok := httpFrom(ctx)
	if !ok {
		return ""
	}
	c, err := r.Cookie(refreshCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// clientIP is the TCP peer address. Proxy headers are not trusted (a trusted
// proxy list is future work), so they cannot be used to dodge throttling.
func clientIP(ctx context.Context) string {
	_, r, ok := httpFrom(ctx)
	if !ok {
		return ""
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func tokenBody(t auth.Tokens) gen.TokenResponse {
	return gen.TokenResponse{AccessToken: t.AccessToken, ExpiresAt: t.AccessExpires.UTC()}
}

// authError maps errors of the local authentication. Messages never reveal
// whether an account exists.
func authError(err error) error {
	var throttled *auth.ThrottledError
	switch {
	case errors.As(err, &throttled):
		return &Problem{Status: http.StatusTooManyRequests, Title: "Too Many Requests",
			Detail: "too many failed attempts, try again later", RetryAfter: int(math.Ceil(throttled.RetryAfter.Seconds()))}
	case errors.Is(err, auth.ErrInvalidCredentials):
		return unauthorized("invalid username or password")
	case errors.Is(err, auth.ErrInvalidSession):
		return unauthorized("session expired, please log in again")
	case errors.Is(err, auth.ErrTenantSuspended):
		return &Problem{Status: http.StatusForbidden, Title: "Forbidden", Detail: "tenant is suspended"}
	case errors.Is(err, auth.ErrSetupUnavailable):
		return &Problem{Status: http.StatusForbidden, Title: "Forbidden", Detail: "setup token is invalid or expired"}
	case errors.Is(err, auth.ErrWeakPassword):
		return &Problem{Status: http.StatusUnprocessableEntity, Title: "Validation Failed", Detail: err.Error()}
	case errors.Is(err, store.ErrConflict):
		return &Problem{Status: http.StatusConflict, Title: "Conflict", Detail: "setup is already complete or the username is taken"}
	case errors.Is(err, cluster.ErrNotApplied), errors.Is(err, cluster.ErrUnknownOutcome):
		return &Problem{Status: http.StatusServiceUnavailable, Title: "Service Unavailable", Detail: "no leader available, retry", RetryAfter: 1}
	}
	return err
}

func validUsername(name string) bool {
	n := len(strings.TrimSpace(name))
	return n >= 3 && n <= 64 && !strings.ContainsAny(name, "\x00\r\n")
}

// Setup creates the first platform admin with the one-time token (FA-AUTH-01).
func (h *Handlers) Setup(ctx context.Context, req gen.SetupRequestObject) (gen.SetupResponseObject, error) {
	l, err := h.localAuth()
	if err != nil {
		return nil, err
	}
	if req.Body == nil || !validUsername(req.Body.Username) {
		return nil, &Problem{Status: http.StatusUnprocessableEntity, Title: "Validation Failed", Detail: "username must have 3 to 64 characters"}
	}
	display := ""
	if req.Body.DisplayName != nil {
		display = *req.Body.DisplayName
	}
	t, err := l.Setup(ctx, req.Body.SetupToken, req.Body.Username, display, req.Body.Password)
	if err != nil {
		return nil, authError(err)
	}
	h.log.Info("setup completed; first platform admin created", "username", strings.TrimSpace(req.Body.Username))
	h.setRefreshCookie(ctx, t.RefreshToken, t.RefreshExpires)
	return gen.Setup201JSONResponse{Body: tokenBody(t)}, nil
}

// Login checks the credentials (throttled per account and IP, FA-AUTH-04).
func (h *Handlers) Login(ctx context.Context, req gen.LoginRequestObject) (gen.LoginResponseObject, error) {
	l, err := h.localAuth()
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, unauthorized("invalid username or password")
	}
	t, err := l.Login(ctx, req.Body.Username, req.Body.Password, clientIP(ctx))
	if err != nil {
		return nil, authError(err)
	}
	h.setRefreshCookie(ctx, t.RefreshToken, t.RefreshExpires)
	return gen.Login200JSONResponse{Body: tokenBody(t)}, nil
}

// Refresh rotates the refresh cookie (FA-AUTH-03). The custom header guards
// against cross-site requests in addition to SameSite=Strict.
func (h *Handlers) Refresh(ctx context.Context, req gen.RefreshRequestObject) (gen.RefreshResponseObject, error) {
	l, err := h.localAuth()
	if err != nil {
		return nil, err
	}
	if string(req.Params.XRequestedWith) != csrfHeader {
		return nil, &Problem{Status: http.StatusBadRequest, Title: "Bad Request", Detail: "missing X-Requested-With header"}
	}
	t, err := l.Refresh(ctx, refreshFromCookie(ctx))
	if err != nil {
		if errors.Is(err, auth.ErrInvalidSession) {
			h.setRefreshCookie(ctx, "", time.Time{})
		}
		return nil, authError(err)
	}
	h.setRefreshCookie(ctx, t.RefreshToken, t.RefreshExpires)
	return gen.Refresh200JSONResponse{Body: tokenBody(t)}, nil
}

// Logout revokes the token family and clears the cookie.
func (h *Handlers) Logout(ctx context.Context, req gen.LogoutRequestObject) (gen.LogoutResponseObject, error) {
	l, err := h.localAuth()
	if err != nil {
		return nil, err
	}
	if string(req.Params.XRequestedWith) != csrfHeader {
		return nil, &Problem{Status: http.StatusBadRequest, Title: "Bad Request", Detail: "missing X-Requested-With header"}
	}
	if err := l.Logout(ctx, refreshFromCookie(ctx)); err != nil {
		return nil, authError(err)
	}
	h.setRefreshCookie(ctx, "", time.Time{})
	return gen.Logout204Response{}, nil
}

// GetMe returns the caller.
func (h *Handlers) GetMe(ctx context.Context, _ gen.GetMeRequestObject) (gen.GetMeResponseObject, error) {
	p, err := principal(ctx, readAccess)
	if err != nil {
		return nil, err
	}
	out := gen.Me{Username: p.Username, TenantId: p.TenantID, AuthMode: gen.MeAuthMode(h.cfg.AuthMode), Roles: make([]gen.Role, 0, len(p.Roles))}
	out.Id = p.UserID
	for _, r := range p.Roles {
		out.Roles = append(out.Roles, gen.Role(r))
	}
	if u, err := h.deps.State.GetUser(ctx, p.TenantID, p.UserID); err == nil && u.DisplayName != "" {
		out.DisplayName = &u.DisplayName
	}
	if t, err := h.deps.State.GetTenant(ctx, p.TenantID); err == nil {
		out.TenantName = &t.Name
	}
	return gen.GetMe200JSONResponse(out), nil
}

// ChangeMyPassword verifies the current password, stores the new one, ends
// all other sessions and starts a new one for this client.
func (h *Handlers) ChangeMyPassword(ctx context.Context, req gen.ChangeMyPasswordRequestObject) (gen.ChangeMyPasswordResponseObject, error) {
	p, err := principal(ctx, readAccess)
	if err != nil {
		return nil, err
	}
	l, err := h.localAuth()
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, &Problem{Status: http.StatusBadRequest, Title: "Bad Request", Detail: "request body is required"}
	}
	t, err := l.ChangePassword(ctx, p, req.Body.CurrentPassword, req.Body.NewPassword)
	if errors.Is(err, auth.ErrInvalidCredentials) {
		return nil, &Problem{Status: http.StatusUnprocessableEntity, Title: "Validation Failed", Detail: "current password is wrong"}
	}
	if err != nil {
		return nil, authError(err)
	}
	h.setRefreshCookie(ctx, t.RefreshToken, t.RefreshExpires)
	return gen.ChangeMyPassword204Response{}, nil
}
