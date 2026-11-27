// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package sqlite

import (
	"context"
	"testing"

	"github.com/joinsider/pulsequorum/internal/store"
)

// TestNodePublicURLRoundTrip checks that the public URL of a voter is stored
// and read back by both the registry listing and a single lookup (ADR 0036).
func TestNodePublicURLRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openState(t, t.TempDir())
	want := store.Node{ID: "v1", Name: "one", Role: store.NodeRoleVoter, Version: "1.0.0",
		MaxCommandVersion: 11, PublicURL: "https://n1.example.com", Healthy: true,
		HealthChangedMs: 1, RegisteredMs: 1, UpdatedMs: 1}
	if err := s.Update(ctx, func(tx store.StateTx) error { return tx.PutNode(ctx, want) }); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].PublicURL != want.PublicURL {
		t.Fatalf("listed nodes = %+v, want public URL %q", list, want.PublicURL)
	}
	err = s.Update(ctx, func(tx store.StateTx) error {
		got, err := tx.GetNode(ctx, "v1")
		if err != nil {
			return err
		}
		if got.PublicURL != want.PublicURL {
			t.Errorf("GetNode public URL = %q, want %q", got.PublicURL, want.PublicURL)
		}
		got.PublicURL = ""
		return tx.PutNode(ctx, got)
	})
	if err != nil {
		t.Fatal(err)
	}
	list, err = s.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].PublicURL != "" {
		t.Errorf("cleared public URL = %q, want empty", list[0].PublicURL)
	}
}
