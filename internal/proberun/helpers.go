// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package proberun

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/joinsider/pulsequorum/internal/config"
	"github.com/joinsider/pulsequorum/internal/store/sqlite"
)

// CheckpointInterval is the period of the WAL checkpoint job (ADR 0013 C4).
const CheckpointInterval = 60 * time.Second

// ReadJoinToken returns the join token from JOIN_TOKEN or JOIN_TOKEN_FILE.
// It is a secret and never logged.
func ReadJoinToken(cfg config.Config) (string, error) {
	if cfg.JoinTokenFile == "" {
		return cfg.JoinToken, nil
	}
	raw, err := os.ReadFile(cfg.JoinTokenFile)
	if err != nil {
		return "", fmt.Errorf("JOIN_TOKEN_FILE: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// DBFileSizes returns the size of the database file and of its WAL in bytes.
// A file that cannot be stat'ed counts as 0; the metric then shows the drop.
func DBFileSizes(db *sqlite.DB) struct{ Size, WAL int64 } {
	var sizes struct{ Size, WAL int64 }
	if info, err := os.Stat(db.Path()); err == nil {
		sizes.Size = info.Size()
	}
	if wal, err := db.WALSize(); err == nil {
		sizes.WAL = wal
	}
	return sizes
}
