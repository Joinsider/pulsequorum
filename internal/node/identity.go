// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package node manages the persistent identity of a node.
package node

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// IDFile is the file inside the data directory that holds the node ID.
const IDFile = "node_id"

// LoadOrCreateID returns the node ID stored in dataDir, creating a random one on
// first start. The ID is never derived from the IP address or host name, so it
// survives address changes (FA-NODE-03, PF-03, PF-07).
func LoadOrCreateID(dataDir string) (string, error) {
	path := filepath.Join(dataDir, IDFile)
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is inside the operator-configured data directory
	if err == nil {
		id := strings.TrimSpace(string(raw))
		if _, perr := uuid.Parse(id); perr != nil {
			return "", fmt.Errorf("node ID in %s is invalid: %w", path, perr)
		}
		return id, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("read node ID: %w", err)
	}

	id := uuid.NewString()
	// Write atomically so a crash never leaves a truncated ID behind.
	tmp, err := os.CreateTemp(dataDir, IDFile+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("create node ID: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(id + "\n"); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write node ID: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("sync node ID: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close node ID: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("store node ID: %w", err)
	}
	return id, nil
}
