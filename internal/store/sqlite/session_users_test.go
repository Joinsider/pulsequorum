// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package sqlite

import (
	"context"
	"slices"
	"testing"

	"github.com/joinsider/pulsequorum/internal/store"
)

// sessionUsersState opens a state store with the tenants t1 and t2 and the
// users in the given tenant/user pairs, each with the same creation time.
func sessionUsersState(t *testing.T, pairs ...[2]string) *StateStore {
	t.Helper()
	ctx := context.Background()
	s := openState(t, t.TempDir())
	if err := s.Update(ctx, func(tx store.StateTx) error {
		for _, id := range []string{"t1", "t2"} {
			if err := tx.PutTenant(ctx, store.Tenant{ID: id, Name: id, Plan: "default", Source: "admin", CreatedMs: 1, UpdatedMs: 1}); err != nil {
				return err
			}
		}
		for _, p := range pairs {
			if err := tx.PutUser(ctx, store.User{TenantID: p[0], ID: p[1], Username: p[0] + "-" + p[1], PasswordHash: "hash",
				Role: "viewer", CreatedMs: 1, UpdatedMs: 1}, 0); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

// insertSession stores an unexpired refresh token of the user.
func insertSession(t *testing.T, s *StateStore, hash, family, tenantID, userID string) {
	t.Helper()
	if err := s.Update(context.Background(), func(tx store.StateTx) error {
		return tx.InsertSession(context.Background(), store.Session{TokenHash: hash, FamilyID: family, TenantID: tenantID,
			UserID: userID, CreatedMs: 1, ExpiresMs: 9_000_000_000_000})
	}); err != nil {
		t.Fatal(err)
	}
}

func TestListUnrevokedSessionUsers(t *testing.T) {
	ctx := context.Background()
	s := sessionUsersState(t,
		[2]string{"t1", "u-a"}, [2]string{"t1", "u-b"}, [2]string{"t1", "u-revoked"},
		[2]string{"t2", "v-a"}, [2]string{"t2", "v-c"}, [2]string{"t2", "v-family"})

	// Inserted out of order; u-b holds two tokens, which must yield one row.
	insertSession(t, s, "h1", "f1", "t2", "v-c")
	insertSession(t, s, "h2", "f2", "t1", "u-b")
	insertSession(t, s, "h3", "f3", "t1", "u-b")
	insertSession(t, s, "h4", "f4", "t1", "u-a")
	insertSession(t, s, "h5", "f5", "t2", "v-a")
	insertSession(t, s, "h6", "f6", "t1", "u-revoked")
	insertSession(t, s, "h7", "f7", "t2", "v-family")
	insertSession(t, s, "h8", "f8", "t2", "v-family")

	if err := s.Update(ctx, func(tx store.StateTx) error {
		if err := tx.RevokeUserSessions(ctx, "t1", "u-revoked", 10); err != nil {
			return err
		}
		// v-family keeps h8; only the family of h7 is revoked.
		return tx.RevokeSessionFamily(ctx, "f7", 10)
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListUnrevokedSessionUsers(ctx)
	if err != nil {
		t.Fatalf("ListUnrevokedSessionUsers: %v", err)
	}
	want := []store.SessionUser{
		{TenantID: "t1", UserID: "u-a"},
		{TenantID: "t1", UserID: "u-b"},
		{TenantID: "t2", UserID: "v-a"},
		{TenantID: "t2", UserID: "v-c"},
		{TenantID: "t2", UserID: "v-family"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("ListUnrevokedSessionUsers = %+v, want %+v", got, want)
	}
}

func TestListUnrevokedSessionUsersWithoutSessions(t *testing.T) {
	ctx := context.Background()
	s := sessionUsersState(t, [2]string{"t1", "u-a"})
	got, err := s.ListUnrevokedSessionUsers(ctx)
	if err != nil {
		t.Fatalf("ListUnrevokedSessionUsers: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListUnrevokedSessionUsers = %+v, want none", got)
	}
}
