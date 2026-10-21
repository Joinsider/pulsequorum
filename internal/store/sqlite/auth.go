// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/store/sqlite/statedb"
)

// GetUser returns a user of the tenant.
func (s *StateStore) GetUser(ctx context.Context, tenantID, id string) (store.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return getUser(ctx, s.q, tenantID, id)
}

// GetUserByUsername returns the user with the username, compared case-insensitively.
func (s *StateStore) GetUserByUsername(ctx context.Context, username string) (store.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, err := s.q.GetUserByUsername(ctx, username)
	if err != nil {
		return store.User{}, mapErr(err)
	}
	return userFromRow(r), nil
}

// ListUsers returns the tenant's users ordered by username.
func (s *StateStore) ListUsers(ctx context.Context, tenantID string) ([]store.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.q.ListUsers(ctx, tenantID)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]store.User, 0, len(rows))
	for _, r := range rows {
		out = append(out, userFromRow(r))
	}
	return out, nil
}

// CountUsers returns the number of users of all tenants.
func (s *StateStore) CountUsers(ctx context.Context) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return countUsers(ctx, s.q)
}

// ListSigningKeys returns all signing keys ordered by creation time and KID.
func (s *StateStore) ListSigningKeys(ctx context.Context) ([]store.SigningKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.q.ListSigningKeys(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]store.SigningKey, 0, len(rows))
	for _, r := range rows {
		out = append(out, store.SigningKey{
			KID: r.Kid, Alg: r.Alg, PublicKey: r.PublicKey, PrivateKeyEnc: r.PrivateKeyEnc,
			CreatedMs: r.CreatedMs, RetiredMs: r.RetiredMs,
		})
	}
	return out, nil
}

// GetSession returns the refresh token with the hash.
func (s *StateStore) GetSession(ctx context.Context, tokenHash string) (store.Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return getSession(ctx, s.q, tokenHash)
}

func (t *stateTx) CountUsers(ctx context.Context) (int, error) {
	return countUsers(ctx, t.q)
}

func (t *stateTx) GetUser(ctx context.Context, tenantID, id string) (store.User, error) {
	return getUser(ctx, t.q, tenantID, id)
}

func (t *stateTx) PutUser(ctx context.Context, u store.User, expectedRevision uint64) error {
	if u.TenantID == "" || u.ID == "" || u.Username == "" {
		return errors.New("put user: empty tenant, user ID or username")
	}
	revision, err := toInt64(expectedRevision + 1)
	if err != nil {
		return err
	}
	if expectedRevision == 0 {
		return mapErr(t.q.InsertUser(ctx, statedb.InsertUserParams{
			TenantID: u.TenantID, ID: u.ID, Username: u.Username, DisplayName: u.DisplayName,
			PasswordHash: u.PasswordHash, Role: u.Role, Disabled: boolToInt(u.Disabled), Revision: revision,
			CreatedMs: u.CreatedMs, UpdatedMs: u.UpdatedMs, LastLoginMs: u.LastLoginMs,
		}))
	}
	expected, err := toInt64(expectedRevision)
	if err != nil {
		return err
	}
	n, err := t.q.UpdateUser(ctx, statedb.UpdateUserParams{
		Username: u.Username, DisplayName: u.DisplayName, PasswordHash: u.PasswordHash, Role: u.Role,
		Disabled: boolToInt(u.Disabled), Revision: revision, UpdatedMs: u.UpdatedMs,
		TenantID: u.TenantID, ID: u.ID, ExpectedRevision: expected,
	})
	if err != nil {
		return mapErr(err)
	}
	if n == 0 {
		if _, err := t.q.UserRevision(ctx, statedb.UserRevisionParams{TenantID: u.TenantID, ID: u.ID}); err != nil {
			return mapErr(err)
		}
		return fmt.Errorf("user %s: revision is not %d: %w", u.ID, expectedRevision, store.ErrConflict)
	}
	return nil
}

func (t *stateTx) DeleteUser(ctx context.Context, tenantID, id string) error {
	n, err := t.q.DeleteUser(ctx, statedb.DeleteUserParams{TenantID: tenantID, ID: id})
	if err != nil {
		return mapErr(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (t *stateTx) SetLastLogin(ctx context.Context, tenantID, userID string, ms int64) error {
	n, err := t.q.SetUserLastLogin(ctx, statedb.SetUserLastLoginParams{LastLoginMs: ms, TenantID: tenantID, ID: userID})
	if err != nil {
		return mapErr(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (t *stateTx) PutSigningKey(ctx context.Context, k store.SigningKey) error {
	if k.KID == "" || k.Alg == "" || len(k.PublicKey) == 0 || k.PrivateKeyEnc == "" {
		return errors.New("put signing key: KID, algorithm, public key and encrypted private key are required")
	}
	return mapErr(t.q.UpsertSigningKey(ctx, statedb.UpsertSigningKeyParams{
		Kid: k.KID, Alg: k.Alg, PublicKey: k.PublicKey, PrivateKeyEnc: k.PrivateKeyEnc,
		CreatedMs: k.CreatedMs, RetiredMs: k.RetiredMs,
	}))
}

func (t *stateTx) GetSession(ctx context.Context, tokenHash string) (store.Session, error) {
	return getSession(ctx, t.q, tokenHash)
}

func (t *stateTx) InsertSession(ctx context.Context, s store.Session) error {
	if s.TokenHash == "" || s.FamilyID == "" || s.TenantID == "" || s.UserID == "" {
		return errors.New("insert session: token hash, family, tenant and user ID are required")
	}
	return mapErr(t.q.InsertSession(ctx, statedb.InsertSessionParams{
		TokenHash: s.TokenHash, FamilyID: s.FamilyID, TenantID: s.TenantID, UserID: s.UserID,
		CreatedMs: s.CreatedMs, ExpiresMs: s.ExpiresMs, UsedMs: s.UsedMs, RevokedMs: s.RevokedMs,
	}))
}

func (t *stateTx) MarkSessionUsed(ctx context.Context, tokenHash string, ms int64) error {
	n, err := t.q.MarkSessionUsed(ctx, statedb.MarkSessionUsedParams{UsedMs: ms, TokenHash: tokenHash})
	if err != nil {
		return mapErr(err)
	}
	if n > 0 {
		return nil
	}
	// Either the token is missing or it was used before (the first time is kept).
	if _, err := getSession(ctx, t.q, tokenHash); err != nil {
		return err
	}
	return nil
}

func (t *stateTx) RevokeSessionFamily(ctx context.Context, familyID string, ms int64) error {
	return mapErr(t.q.RevokeSessionFamily(ctx, statedb.RevokeSessionFamilyParams{RevokedMs: ms, FamilyID: familyID}))
}

func (t *stateTx) RevokeUserSessions(ctx context.Context, tenantID, userID string, ms int64) error {
	return mapErr(t.q.RevokeUserSessions(ctx, statedb.RevokeUserSessionsParams{RevokedMs: ms, TenantID: tenantID, UserID: userID}))
}

func (t *stateTx) PruneSessions(ctx context.Context, beforeMs int64) error {
	return mapErr(t.q.PruneSessions(ctx, beforeMs))
}

func countUsers(ctx context.Context, q *statedb.Queries) (int, error) {
	n, err := q.CountUsers(ctx)
	if err != nil {
		return 0, mapErr(err)
	}
	return int(n), nil
}

func getUser(ctx context.Context, q *statedb.Queries, tenantID, id string) (store.User, error) {
	r, err := q.GetUser(ctx, statedb.GetUserParams{TenantID: tenantID, ID: id})
	if err != nil {
		return store.User{}, mapErr(err)
	}
	return userFromRow(r), nil
}

func getSession(ctx context.Context, q *statedb.Queries, tokenHash string) (store.Session, error) {
	r, err := q.GetSession(ctx, tokenHash)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Session{}, store.ErrNotFound
	}
	if err != nil {
		return store.Session{}, mapErr(err)
	}
	return store.Session{
		TokenHash: r.TokenHash, FamilyID: r.FamilyID, TenantID: r.TenantID, UserID: r.UserID,
		CreatedMs: r.CreatedMs, ExpiresMs: r.ExpiresMs, UsedMs: r.UsedMs, RevokedMs: r.RevokedMs,
	}, nil
}

func userFromRow(r statedb.User) store.User {
	return store.User{
		TenantID: r.TenantID, ID: r.ID, Username: r.Username, DisplayName: r.DisplayName,
		PasswordHash: r.PasswordHash, Role: r.Role, Disabled: r.Disabled != 0,
		Revision: uint64(max(r.Revision, 0)), CreatedMs: r.CreatedMs, UpdatedMs: r.UpdatedMs,
		LastLoginMs: r.LastLoginMs,
	}
}
