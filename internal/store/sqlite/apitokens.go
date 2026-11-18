// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package sqlite

import (
	"context"
	"fmt"

	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/store/sqlite/statedb"
)

// GetAPITokenBySecretHash returns the API token with the secret hash or ErrNotFound (ADR 0031).
func (s *StateStore) GetAPITokenBySecretHash(ctx context.Context, secretHash string) (store.APIToken, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, err := s.q.GetAPITokenBySecretHash(ctx, secretHash)
	if err != nil {
		return store.APIToken{}, mapErr(err)
	}
	return apiTokenFromRow(r), nil
}

// ListAPITokens returns the API tokens of the tenant, newest first.
func (s *StateStore) ListAPITokens(ctx context.Context, tenantID string) ([]store.APIToken, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.q.ListAPITokens(ctx, tenantID)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]store.APIToken, 0, len(rows))
	for _, r := range rows {
		out = append(out, apiTokenFromRow(r))
	}
	return out, nil
}

// InsertAPIToken stores a new API token. A duplicate ID, secret hash or name
// within the tenant is ErrConflict; an unknown tenant is ErrNotFound (ADR 0031).
func (t *stateTx) InsertAPIToken(ctx context.Context, tok store.APIToken) error {
	if tok.ID == "" || tok.TenantID == "" || tok.SecretHash == "" {
		return fmt.Errorf("insert api token: empty ID, tenant or secret hash")
	}
	return mapErr(t.q.InsertAPIToken(ctx, statedb.InsertAPITokenParams{
		ID:            tok.ID,
		TenantID:      tok.TenantID,
		Scope:         tok.Scope,
		Name:          tok.Name,
		Role:          tok.Role,
		SecretHash:    tok.SecretHash,
		CreatedMs:     tok.CreatedMs,
		ExpiresMs:     tok.ExpiresMs,
		CreatedByKind: tok.CreatedByKind,
		CreatedByID:   tok.CreatedByID,
		CreatedByName: tok.CreatedByName,
	}))
}

// GetAPIToken returns the API token of the tenant or ErrNotFound.
func (t *stateTx) GetAPIToken(ctx context.Context, tenantID, id string) (store.APIToken, error) {
	r, err := t.q.GetAPIToken(ctx, statedb.GetAPITokenParams{TenantID: tenantID, ID: id})
	if err != nil {
		return store.APIToken{}, mapErr(err)
	}
	return apiTokenFromRow(r), nil
}

// CountAPITokens returns the number of API tokens of the tenant.
func (t *stateTx) CountAPITokens(ctx context.Context, tenantID string) (int, error) {
	n, err := t.q.CountAPITokens(ctx, tenantID)
	if err != nil {
		return 0, mapErr(err)
	}
	return int(n), nil
}

// DeleteAPIToken removes the API token of the tenant or returns ErrNotFound.
func (t *stateTx) DeleteAPIToken(ctx context.Context, tenantID, id string) error {
	n, err := t.q.DeleteAPIToken(ctx, statedb.DeleteAPITokenParams{TenantID: tenantID, ID: id})
	if err != nil {
		return mapErr(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// PruneAPITokens deletes the API tokens that expired before beforeMs.
func (t *stateTx) PruneAPITokens(ctx context.Context, beforeMs int64) error {
	return mapErr(t.q.PruneAPITokens(ctx, beforeMs))
}

func apiTokenFromRow(r statedb.ApiToken) store.APIToken {
	return store.APIToken{
		ID:            r.ID,
		TenantID:      r.TenantID,
		Scope:         r.Scope,
		Name:          r.Name,
		Role:          r.Role,
		SecretHash:    r.SecretHash,
		CreatedMs:     r.CreatedMs,
		ExpiresMs:     r.ExpiresMs,
		CreatedByKind: r.CreatedByKind,
		CreatedByID:   r.CreatedByID,
		CreatedByName: r.CreatedByName,
	}
}
