// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package node

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func TestLoadOrCreateID(t *testing.T) {
	dir := t.TempDir()
	id, err := LoadOrCreateID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uuid.Parse(id); err != nil {
		t.Fatalf("ID %q is not a UUID: %v", id, err)
	}

	again, err := LoadOrCreateID(dir)
	if err != nil || again != id {
		t.Fatalf("second load = %q, %v; want %q", again, err, id)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("data dir contains %d entries, want only %s", len(entries), IDFile)
	}
}

func TestLoadOrCreateIDRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, IDFile), []byte("not-a-uuid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateID(dir); err == nil {
		t.Fatal("invalid node ID accepted")
	}
}
