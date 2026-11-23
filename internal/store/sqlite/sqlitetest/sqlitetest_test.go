// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package sqlitetest

import (
	"context"
	"errors"
	"testing"

	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/store/sqlite"
)

func putTenant(ctx context.Context, s *sqlite.StateStore, id, name string) error {
	return s.Update(ctx, func(tx store.StateTx) error {
		return tx.PutTenant(ctx, store.Tenant{ID: id, Name: name, Plan: "default", CreatedMs: 1, UpdatedMs: 1})
	})
}

func TestOpenStateIndependentDirs(t *testing.T) {
	ctx := context.Background()
	a := OpenState(t, t.TempDir())
	defer func() { _ = a.Close() }()
	b := OpenState(t, t.TempDir())
	defer func() { _ = b.Close() }()

	if err := putTenant(ctx, a, "tenant-a", "A"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.GetTenant(ctx, "tenant-a"); err != nil {
		t.Fatalf("store a does not see its own tenant: %v", err)
	}
	if _, err := b.GetTenant(ctx, "tenant-a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("store b sees tenant of store a: %v", err)
	}
}

func TestOpenStateReopenKeepsData(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := OpenState(t, dir)
	if err := putTenant(ctx, s, "tenant-r", "R"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	again := OpenState(t, dir)
	defer func() { _ = again.Close() }()
	got, err := again.GetTenant(ctx, "tenant-r")
	if err != nil {
		t.Fatalf("reopened store lost tenant: %v", err)
	}
	if got.Name != "R" {
		t.Fatalf("name = %q, want R", got.Name)
	}
}

// The helper result must match a plain sqlite.OpenState on an empty dir.
func TestOpenStateMatchesRealOpen(t *testing.T) {
	ctx := context.Background()
	s := OpenState(t, t.TempDir())
	defer func() { _ = s.Close() }()
	plain, err := sqlite.OpenState(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = plain.Close() }()
	a, err := s.ListTenants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, err := plain.ListTenants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Fatalf("tenant count %d, plain open has %d", len(a), len(b))
	}
}
