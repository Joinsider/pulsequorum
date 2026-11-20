// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"math"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/joinsider/pulsequorum/internal/store"
)

// mapErr translates driver errors into store errors.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() {
		case sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY, sqlite3.SQLITE_CONSTRAINT_UNIQUE:
			return fmt.Errorf("%w: %w", store.ErrConflict, err)
		case sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY:
			// The referenced parent (e.g. the tenant) does not exist.
			return fmt.Errorf("%w: %w", store.ErrNotFound, err)
		}
	}
	return err
}

func toInt64(v uint64) (int64, error) {
	if v > math.MaxInt64 {
		return 0, fmt.Errorf("value %d exceeds int64", v)
	}
	return int64(v), nil
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func clampLimit(limit, def, maxLimit int) int {
	if limit <= 0 {
		return def
	}
	return min(limit, maxLimit)
}
