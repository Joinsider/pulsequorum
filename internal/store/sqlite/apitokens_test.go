// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package sqlite

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/joinsider/uptimecluster/internal/store"
)

// tokenStore opens a state store with the tenants t1 and t2.
func tokenStore(t *testing.T) *StateStore {
	t.Helper()
	ctx := context.Background()
	s := openState(t, t.TempDir())
	if err := s.Update(ctx, func(tx store.StateTx) error {
		for _, id := range []string{"t1", "t2"} {
			if err := tx.PutTenant(ctx, store.Tenant{ID: id, Name: id, Plan: "default", Source: "admin", CreatedMs: 1, UpdatedMs: 1}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

// token returns an API token with the given ID, tenant, name and hash.
func token(id, tenant, name, hash string, createdMs, expiresMs int64) store.APIToken {
	return store.APIToken{
		ID: id, TenantID: tenant, Scope: "tenant", Name: name, Role: "tenant-editor",
		SecretHash: hash, CreatedMs: createdMs, ExpiresMs: expiresMs,
		CreatedByKind: "user", CreatedByID: "usr-1", CreatedByName: "Admin",
	}
}

// insertToken stores the token in its own transaction.
func insertToken(t *testing.T, s *StateStore, tok store.APIToken) error {
	t.Helper()
	return s.Update(context.Background(), func(tx store.StateTx) error { return tx.InsertAPIToken(context.Background(), tok) })
}

func TestAPITokenRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := tokenStore(t)
	const far = 9_000_000_000_000
	older := token("tok_a", "t1", "ci", "hash-a", 100, far)
	newer := token("tok_b", "t1", "deploy", "hash-b", 200, far)
	if err := insertToken(t, s, older); err != nil {
		t.Fatalf("insert older: %v", err)
	}
	if err := insertToken(t, s, newer); err != nil {
		t.Fatalf("insert newer: %v", err)
	}

	got, err := s.GetAPITokenBySecretHash(ctx, "hash-a")
	if err != nil {
		t.Fatalf("GetAPITokenBySecretHash: %v", err)
	}
	if got != older {
		t.Errorf("by hash = %+v, want %+v", got, older)
	}
	if _, err := s.GetAPITokenBySecretHash(ctx, "unknown"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown hash: err = %v, want ErrNotFound", err)
	}

	list, err := s.ListAPITokens(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "tok_b" || list[1].ID != "tok_a" {
		t.Errorf("list t1 = %v, want newest first [tok_b tok_a]", tokenIDs(list))
	}
	if list, _ := s.ListAPITokens(ctx, "t2"); len(list) != 0 {
		t.Errorf("list t2 = %v, want empty", tokenIDs(list))
	}

	var count int
	if err := s.Update(ctx, func(tx store.StateTx) error {
		var err error
		count, err = tx.CountAPITokens(ctx, "t1")
		return err
	}); err != nil || count != 2 {
		t.Errorf("count t1 = %d, %v; want 2", count, err)
	}
}

func tokenIDs(list []store.APIToken) []string {
	out := make([]string, 0, len(list))
	for _, tok := range list {
		out = append(out, tok.ID)
	}
	return out
}

func TestAPITokenTenantSeparation(t *testing.T) {
	ctx := context.Background()
	s := tokenStore(t)
	if err := insertToken(t, s, token("tok_a", "t1", "ci", "hash-a", 1, 9_000_000_000_000)); err != nil {
		t.Fatal(err)
	}
	err := s.Update(ctx, func(tx store.StateTx) error {
		if _, err := tx.GetAPIToken(ctx, "t2", "tok_a"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetAPIToken of t2 = %v, want ErrNotFound", err)
		}
		if err := tx.DeleteAPIToken(ctx, "t2", "tok_a"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("DeleteAPIToken of t2 = %v, want ErrNotFound", err)
		}
		if n, _ := tx.CountAPITokens(ctx, "t2"); n != 0 {
			t.Errorf("count t2 = %d, want 0", n)
		}
		if _, err := tx.GetAPIToken(ctx, "t1", "tok_a"); err != nil {
			t.Errorf("GetAPIToken of t1: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The wrong tenant must not have removed the token.
	if _, err := s.GetAPITokenBySecretHash(ctx, "hash-a"); err != nil {
		t.Errorf("token gone after foreign delete: %v", err)
	}
}

func TestAPITokenConstraints(t *testing.T) {
	const far = 9_000_000_000_000
	base := token("tok_a", "t1", "ci", "hash-a", 1, far)
	tests := []struct {
		name    string
		tok     store.APIToken
		wantErr error
	}{
		{"duplicate ID", token("tok_a", "t1", "other", "hash-x", 1, far), store.ErrConflict},
		{"duplicate name in tenant", token("tok_b", "t1", "ci", "hash-b", 1, far), store.ErrConflict},
		{"duplicate hash in other tenant", token("tok_c", "t2", "ci", "hash-a", 1, far), store.ErrConflict},
		{"same name in other tenant", token("tok_d", "t2", "ci", "hash-d", 1, far), nil},
		{"unknown tenant", token("tok_e", "t9", "ci", "hash-e", 1, far), store.ErrNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := tokenStore(t)
			if err := insertToken(t, s, base); err != nil {
				t.Fatal(err)
			}
			err := insertToken(t, s, tc.tok)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("insert: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("insert err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestAPITokenDelete(t *testing.T) {
	ctx := context.Background()
	s := tokenStore(t)
	if err := insertToken(t, s, token("tok_a", "t1", "ci", "hash-a", 1, 9_000_000_000_000)); err != nil {
		t.Fatal(err)
	}
	for i, wantErr := range []error{nil, store.ErrNotFound} {
		err := s.Update(ctx, func(tx store.StateTx) error { return tx.DeleteAPIToken(ctx, "t1", "tok_a") })
		if wantErr == nil && err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
		if wantErr != nil && !errors.Is(err, wantErr) {
			t.Fatalf("delete %d: err = %v, want %v", i, err, wantErr)
		}
	}
	if _, err := s.GetAPITokenBySecretHash(ctx, "hash-a"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleted token still found: %v", err)
	}
}

func TestPruneAPITokens(t *testing.T) {
	ctx := context.Background()
	s := tokenStore(t)
	for i, exp := range []int64{100, 200, 300} {
		tok := token([]string{"tok_a", "tok_b", "tok_c"}[i], "t1", []string{"a", "b", "c"}[i], []string{"hash-a", "hash-b", "hash-c"}[i], 1, exp)
		if err := insertToken(t, s, tok); err != nil {
			t.Fatal(err)
		}
	}
	// Expired before 200 means expires_ms < 200: only tok_a goes.
	if err := s.Update(ctx, func(tx store.StateTx) error { return tx.PruneAPITokens(ctx, 200) }); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListAPITokens(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got := tokenIDs(list); !slices.Equal(got, []string{"tok_b", "tok_c"}) {
		t.Errorf("after prune = %v, want tok_b and tok_c", got)
	}
	if _, err := s.GetAPITokenBySecretHash(ctx, "hash-a"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expired token survived the prune: %v", err)
	}
}
