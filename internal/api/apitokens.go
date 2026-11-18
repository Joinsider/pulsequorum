// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/uptimecluster/internal/api/gen"
	"github.com/joinsider/uptimecluster/internal/auth"
	"github.com/joinsider/uptimecluster/internal/checker"
	"github.com/joinsider/uptimecluster/internal/cluster"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
)

// API tokens (FA-AUTH-10, ADR 0031). A tenant admin manages the tokens of the
// own tenant with an interactive login. The secret is returned exactly once,
// by the create operation, and is never stored or logged; only its hash goes
// to Raft.

const apiTokenNameRule = "must be 1 to 64 characters without control characters" //nolint:gosec // G101: validation message, not a credential

// tokenManager returns the caller of the API token operations: a tenant admin
// or platform admin, signed in interactively (ADR 0031). Tokens are rejected
// by the middleware already; the check here is a second line.
func tokenManager(ctx context.Context) (auth.Principal, error) {
	p, err := principal(ctx, adminAccess)
	if err != nil {
		return auth.Principal{}, err
	}
	if err := auth.RequireInteractive(p); err != nil {
		return auth.Principal{}, &Problem{Status: http.StatusForbidden, Title: "Forbidden", Detail: err.Error()}
	}
	return p, nil
}

// ListApiTokens returns the API tokens of the caller's tenant, newest first.
//
//nolint:revive // the name is fixed by the generated interface (operationId listApiTokens)
func (h *Handlers) ListApiTokens(ctx context.Context, _ gen.ListApiTokensRequestObject) (gen.ListApiTokensResponseObject, error) {
	p, err := tokenManager(ctx)
	if err != nil {
		return nil, err
	}
	tokens, err := h.deps.State.ListAPITokens(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	now := h.deps.Now()
	out := make([]gen.ApiToken, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, apiToken(t, now))
	}
	return gen.ListApiTokens200JSONResponse(out), nil
}

// CreateApiToken creates an API token of the caller's tenant and returns its
// secret once (ADR 0031, section 1). The request has no Idempotency-Key: a
// retry would get the stored result of the first attempt, whose secret is
// lost, so every request creates a new token.
//
//nolint:revive // the name is fixed by the generated interface (operationId createApiToken)
func (h *Handlers) CreateApiToken(ctx context.Context, req gen.CreateApiTokenRequestObject) (gen.CreateApiTokenResponseObject, error) {
	p, err := tokenManager(ctx)
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, requestBodyRequired()
	}
	in := req.Body
	now := h.deps.Now()
	var fields []checker.FieldError
	if !cluster.ValidAPITokenName(in.Name) {
		fields = append(fields, fieldError("/name", apiTokenNameRule))
	}
	if !cluster.ValidAPITokenRole(string(in.Role)) {
		fields = append(fields, fieldError("/role", "must be tenant-viewer, tenant-editor or tenant-admin"))
	}
	if lifetime := in.ExpiresAt.Sub(now); lifetime < cluster.MinAPITokenLifetime || lifetime > cluster.MaxAPITokenLifetime {
		fields = append(fields, fieldError("/expiresAt", "must be between one hour and 366 days from now"))
	}
	if err := validationProblem(fields, "the API token is invalid"); err != nil {
		return nil, err
	}

	existing, err := h.deps.State.ListAPITokens(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	if slices.ContainsFunc(existing, func(t store.APIToken) bool { return t.Name == in.Name }) {
		return nil, &Problem{Status: http.StatusConflict, Title: "Conflict", Detail: "an API token with this name already exists"}
	}
	level, err := h.deps.State.CommandLevel(ctx)
	if err != nil {
		return nil, err
	}
	if cluster.EffectiveCommandLevel(level) < cluster.AuditCommandLevel {
		return nil, &Problem{Status: http.StatusConflict, Title: "Conflict",
			Detail: "API tokens need all voters to run a version with command level 9; upgrade them first"}
	}

	id, secret, secretHash, err := auth.NewAPIToken()
	if err != nil {
		return nil, fmt.Errorf("new API token: %w", err)
	}
	cmd := cluster.NewCreateAPIToken(uuid.NewString(), p.TenantID, nil, &pb.CreateAPIToken{
		Id:          id,
		Scope:       cluster.APITokenScopeTenant,
		Name:        in.Name,
		Role:        string(in.Role),
		SecretHash:  secretHash,
		ExpiresAtMs: in.ExpiresAt.UnixMilli(),
	})
	if _, err := h.apply(ctx, cmd); err != nil {
		return nil, err
	}
	h.log.Info("API token created", "tenant", p.TenantID, "token", id, "role", in.Role)

	tokens, err := h.deps.State.ListAPITokens(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(tokens, func(t store.APIToken) bool { return t.ID == id })
	if i < 0 {
		return nil, fmt.Errorf("created API token %s not found", id)
	}
	view := apiToken(tokens[i], h.deps.Now())
	noStore := "no-store"
	return gen.CreateApiToken201JSONResponse{
		Body: gen.ApiTokenCreated{
			CreatedAt: view.CreatedAt,
			CreatedBy: view.CreatedBy,
			Expired:   view.Expired,
			ExpiresAt: view.ExpiresAt,
			Id:        view.Id,
			Name:      view.Name,
			Role:      view.Role,
			Token:     secret,
		},
		Headers: gen.CreateApiToken201ResponseHeaders{CacheControl: &noStore},
	}, nil
}

// RevokeApiToken deletes an API token of the caller's tenant. Another tenant's
// token is not found. Its audit entries stay.
//
//nolint:revive // the name is fixed by the generated interface (operationId revokeApiToken)
func (h *Handlers) RevokeApiToken(ctx context.Context, req gen.RevokeApiTokenRequestObject) (gen.RevokeApiTokenResponseObject, error) {
	p, err := tokenManager(ctx)
	if err != nil {
		return nil, err
	}
	tokens, err := h.deps.State.ListAPITokens(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(tokens, func(t store.APIToken) bool { return t.ID == req.TokenId }) {
		return nil, notFound("API token")
	}
	if _, err := h.apply(ctx, cluster.NewRevokeAPIToken(uuid.NewString(), p.TenantID, nil, req.TokenId)); err != nil {
		return nil, err
	}
	h.log.Info("API token revoked", "tenant", p.TenantID, "token", req.TokenId)
	return gen.RevokeApiToken204Response{}, nil
}

// apiToken converts a token for the API. The secret hash never leaves the
// store; expired is computed from the clock of this node.
func apiToken(t store.APIToken, now time.Time) gen.ApiToken {
	expires := time.UnixMilli(t.ExpiresMs)
	return gen.ApiToken{
		Id:        t.ID,
		Name:      t.Name,
		Role:      gen.ApiTokenRole(t.Role),
		CreatedAt: time.UnixMilli(t.CreatedMs).UTC(),
		ExpiresAt: expires.UTC(),
		Expired:   !now.Before(expires),
		CreatedBy: gen.AuditActor{Kind: gen.AuditActorKind(t.CreatedByKind), Id: t.CreatedByID, Name: t.CreatedByName},
	}
}
