// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package sqlitetest opens SQLite stores for tests with a cheaper setup.
//
// It is for tests only and must not be imported by production code. Opening a
// fresh state.db applies all migrations, which costs far more under the race
// detector than the rest of a unit test. OpenState therefore migrates a fresh
// database once per test binary, keeps its bytes as a template, and copies that
// template into each new directory. The real sqlite.OpenState still runs on the
// copy, so schema checks and pragmas are exercised as usual.
//
// Only the state store is templated. The metrics store generates a random
// incarnation on every new file (ADR 0012 B4), so a copied metrics.db would
// share its identity with all others. Tests that need metrics stores call
// sqlite.OpenMetrics directly.
//
// OpenState does not register a cleanup. The caller closes the store, which
// keeps the close semantics of the helpers it replaces.
package sqlitetest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/joinsider/pulsequorum/internal/store/sqlite"
)

var (
	stateOnce     sync.Once
	stateTemplate []byte
	stateErr      error
)

// loadStateTemplate migrates a fresh state.db once and returns its bytes.
func loadStateTemplate() ([]byte, error) {
	stateOnce.Do(func() {
		dir, err := os.MkdirTemp("", "sqlitetest-state-*")
		if err != nil {
			stateErr = fmt.Errorf("sqlitetest: create template dir: %w", err)
			return
		}
		defer func() { _ = os.RemoveAll(dir) }()
		s, err := sqlite.OpenState(context.Background(), dir)
		if err != nil {
			stateErr = fmt.Errorf("sqlitetest: migrate template: %w", err)
			return
		}
		if err := s.Close(); err != nil {
			stateErr = fmt.Errorf("sqlitetest: close template: %w", err)
			return
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			stateErr = fmt.Errorf("sqlitetest: open template dir: %w", err)
			return
		}
		defer func() { _ = root.Close() }()
		stateTemplate, stateErr = root.ReadFile(sqlite.StateFile)
		if stateErr != nil {
			stateErr = fmt.Errorf("sqlitetest: read template: %w", stateErr)
		}
	})
	return stateTemplate, stateErr
}

// OpenState opens dir/state.db. If the file does not exist yet, it is created
// from the migrated template. An existing file is opened as is, so a test can
// reopen a directory and keep its data.
func OpenState(tb testing.TB, dir string) *sqlite.StateStore {
	tb.Helper()
	path := filepath.Join(dir, sqlite.StateFile)
	data, err := loadStateTemplate()
	if err != nil {
		tb.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		tb.Fatal(fmt.Errorf("sqlitetest: open %s: %w", dir, err))
	}
	defer func() { _ = root.Close() }()
	// O_EXCL: never overwrite an existing database.
	f, err := root.OpenFile(sqlite.StateFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	switch {
	case err == nil:
		if _, err := f.Write(data); err != nil {
			_ = f.Close()
			tb.Fatal(fmt.Errorf("sqlitetest: write %s: %w", path, err))
		}
		if err := f.Close(); err != nil {
			tb.Fatal(fmt.Errorf("sqlitetest: close %s: %w", path, err))
		}
	case errors.Is(err, fs.ErrExist):
		// Reopen: keep the existing data.
	default:
		tb.Fatal(fmt.Errorf("sqlitetest: create %s: %w", path, err))
	}
	s, err := sqlite.OpenState(context.Background(), dir)
	if err != nil {
		tb.Fatal(err)
	}
	return s
}
