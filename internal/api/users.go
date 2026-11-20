// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/pulsequorum/internal/api/gen"
	"github.com/joinsider/pulsequorum/internal/auth"
	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/config"
	"github.com/joinsider/pulsequorum/internal/store"
)

// User management for local authentication (ADR 0008, FA-AUTH-09). The
// authorization rules live in user_policy.go; every write is checked there
// before it is proposed. The tenant always comes from the actor.

// maxDisplayName is the length limit of a display name (openapi: maxLength 128).
const maxDisplayName = 128

func unprocessable(detail string) *Problem {
	return &Problem{Status: http.StatusUnprocessableEntity, Title: "Validation Failed", Detail: detail}
}

// toAPIUser maps a stored user. The password hash is never part of the response.
func toAPIUser(u store.User) (gen.User, error) {
	id, err := uuid.Parse(u.ID)
	if err != nil {
		return gen.User{}, fmt.Errorf("user %q has an invalid ID: %w", u.ID, err)
	}
	out := gen.User{
		Id:        id,
		Username:  u.Username,
		Role:      gen.Role(u.Role),
		Disabled:  u.Disabled,
		CreatedAt: time.UnixMilli(u.CreatedMs).UTC(),
	}
	if u.DisplayName != "" {
		name := u.DisplayName
		out.DisplayName = &name
	}
	if u.LastLoginMs != 0 {
		last := time.UnixMilli(u.LastLoginMs).UTC()
		out.LastLoginAt = &last
	}
	return out, nil
}

// managedUser loads a user of the caller's tenant. Users of other tenants are
// reported as not found.
func (h *Handlers) managedUser(ctx context.Context, p auth.Principal, id uuid.UUID) (store.User, error) {
	u, err := h.deps.State.GetUser(ctx, p.TenantID, id.String())
	if errors.Is(err, store.ErrNotFound) {
		return store.User{}, notFound("user")
	}
	if err != nil {
		return store.User{}, err
	}
	return u, nil
}

// ListUsers returns the users of the caller's tenant (admins only).
func (h *Handlers) ListUsers(ctx context.Context, _ gen.ListUsersRequestObject) (gen.ListUsersResponseObject, error) {
	p, err := principal(ctx, adminAccess)
	if err != nil {
		return nil, err
	}
	if h.cfg.AuthMode == config.AuthOIDC {
		return nil, errManagedByIDP()
	}
	users, err := h.deps.State.ListUsers(ctx, p.TenantID)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	out := make(gen.ListUsers200JSONResponse, 0, len(users))
	for _, u := range users {
		item, err := toAPIUser(u)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

// CreateUser creates a local user. The password is hashed here; the FSM only
// accepts Argon2id hashes.
func (h *Handlers) CreateUser(ctx context.Context, req gen.CreateUserRequestObject) (gen.CreateUserResponseObject, error) {
	p, err := principal(ctx, readAccess)
	if err != nil {
		return nil, err
	}
	if h.cfg.AuthMode == config.AuthOIDC {
		return nil, errManagedByIDP()
	}
	if req.Body == nil {
		return nil, unprocessable("request body is required")
	}
	role := auth.Role(req.Body.Role)
	if !auth.ValidRole(role) {
		return nil, unprocessable(fmt.Sprintf("unknown role %q", req.Body.Role))
	}
	if err := h.authorizeUserChange(ctx, p, userChange{NewRole: role}); err != nil {
		return nil, err
	}
	username := strings.TrimSpace(req.Body.Username)
	if !validUsername(username) {
		return nil, unprocessable("username must have 3 to 64 characters")
	}
	displayName := ""
	if req.Body.DisplayName != nil {
		displayName = strings.TrimSpace(*req.Body.DisplayName)
		if len(displayName) > maxDisplayName {
			return nil, unprocessable("display name must have at most 128 characters")
		}
	}
	if err := auth.CheckPassword(req.Body.Password); err != nil {
		return nil, unprocessable(err.Error())
	}
	hash, err := auth.HashPassword(req.Body.Password)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}
	cmd, err := cluster.NewPutUser(requestID(p.TenantID, req.Params.IdempotencyKey), p.TenantID, uuid.NewString(), cluster.UserDocument{
		Username: username, DisplayName: displayName, PasswordHash: hash, Role: string(role),
	}, 0)
	if err != nil {
		return nil, err
	}
	res, err := h.apply(ctx, cmd)
	if err != nil {
		return nil, err
	}
	// A deduplicated retry returns the ID of the first attempt.
	var put cluster.PutResult
	if err := json.Unmarshal(res.Result, &put); err != nil {
		return nil, fmt.Errorf("decode put result: %w", err)
	}
	u, err := h.deps.State.GetUser(ctx, p.TenantID, put.ID)
	if err != nil {
		return nil, err
	}
	out, err := toAPIUser(u)
	if err != nil {
		return nil, err
	}
	return gen.CreateUser201JSONResponse(out), nil
}

// UpdateUser changes display name, role, disabled flag or (admin reset) the
// password. Unchanged fields keep their current values. A password change or
// disabling ends the user's sessions in the FSM.
func (h *Handlers) UpdateUser(ctx context.Context, req gen.UpdateUserRequestObject) (gen.UpdateUserResponseObject, error) {
	p, err := principal(ctx, readAccess)
	if err != nil {
		return nil, err
	}
	if h.cfg.AuthMode == config.AuthOIDC {
		return nil, errManagedByIDP()
	}
	// The admin check runs before the lookup, so non-admins learn nothing about user IDs.
	if err := h.authorizeUserChange(ctx, p, userChange{}); err != nil {
		return nil, err
	}
	current, err := h.managedUser(ctx, p, req.UserId)
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, unprocessable("request body is required")
	}
	role := auth.Role(current.Role)
	change := userChange{Target: &current, Disable: req.Body.Disabled != nil && *req.Body.Disabled, Password: req.Body.Password != nil}
	if req.Body.Role != nil {
		role = auth.Role(*req.Body.Role)
		if !auth.ValidRole(role) {
			return nil, unprocessable(fmt.Sprintf("unknown role %q", *req.Body.Role))
		}
		change.NewRole = role
	}
	if err := h.authorizeUserChange(ctx, p, change); err != nil {
		return nil, err
	}

	doc := cluster.UserDocument{
		Username: current.Username, DisplayName: current.DisplayName, PasswordHash: current.PasswordHash,
		Role: string(role), Disabled: current.Disabled,
	}
	if req.Body.DisplayName != nil {
		name := strings.TrimSpace(*req.Body.DisplayName)
		if len(name) > maxDisplayName {
			return nil, unprocessable("display name must have at most 128 characters")
		}
		doc.DisplayName = name
	}
	if req.Body.Disabled != nil {
		doc.Disabled = *req.Body.Disabled
	}
	if req.Body.Password != nil {
		if err := auth.CheckPassword(*req.Body.Password); err != nil {
			return nil, unprocessable(err.Error())
		}
		hash, err := auth.HashPassword(*req.Body.Password)
		if err != nil {
			return nil, fmt.Errorf("hash password: %w", err)
		}
		doc.PasswordHash = hash
	}

	cmd, err := cluster.NewPutUser(uuid.NewString(), p.TenantID, current.ID, doc, current.Revision)
	if err != nil {
		return nil, err
	}
	if _, err := h.apply(ctx, cmd); err != nil {
		return nil, err
	}
	u, err := h.deps.State.GetUser(ctx, p.TenantID, current.ID)
	if err != nil {
		return nil, err
	}
	out, err := toAPIUser(u)
	if err != nil {
		return nil, err
	}
	return gen.UpdateUser200JSONResponse(out), nil
}

// DeleteUser removes a user and, by cascade, its sessions.
func (h *Handlers) DeleteUser(ctx context.Context, req gen.DeleteUserRequestObject) (gen.DeleteUserResponseObject, error) {
	p, err := principal(ctx, readAccess)
	if err != nil {
		return nil, err
	}
	if h.cfg.AuthMode == config.AuthOIDC {
		return nil, errManagedByIDP()
	}
	if err := h.authorizeUserChange(ctx, p, userChange{}); err != nil {
		return nil, err
	}
	current, err := h.managedUser(ctx, p, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := h.authorizeUserChange(ctx, p, userChange{Target: &current, Delete: true}); err != nil {
		return nil, err
	}
	if _, err := h.apply(ctx, cluster.NewDeleteUser(uuid.NewString(), p.TenantID, current.ID)); err != nil {
		return nil, err
	}
	return gen.DeleteUser204Response{}, nil
}
