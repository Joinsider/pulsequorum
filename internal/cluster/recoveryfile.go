// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// RecoveryRevokeFile is written by "admin recover" into the data directory. It
// lists the members the recovery dropped; the recovered node revokes them once
// it leads again, so they cannot rejoin with stale state (ADR 0019).
const RecoveryRevokeFile = "recovery-revoke.json"

// RecoveryRevoke is the content of RecoveryRevokeFile.
type RecoveryRevoke struct {
	NodeIDs     []string  `json:"nodeIds"`
	RecoveredAt time.Time `json:"recoveredAt"`
}

// WriteRecoveryRevoke stores r atomically with mode 0600.
func WriteRecoveryRevoke(dataDir string, r RecoveryRevoke) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dataDir, RecoveryRevokeFile+".tmp-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", RecoveryRevokeFile, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", RecoveryRevokeFile, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", RecoveryRevokeFile, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", RecoveryRevokeFile, err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dataDir, RecoveryRevokeFile)); err != nil {
		return fmt.Errorf("write %s: %w", RecoveryRevokeFile, err)
	}
	return nil
}

// ReadRecoveryRevoke returns the stored list and whether the file exists.
func ReadRecoveryRevoke(dataDir string) (RecoveryRevoke, bool, error) {
	data, err := os.ReadFile(filepath.Join(dataDir, RecoveryRevokeFile)) //nolint:gosec // G304: fixed name inside the data directory
	if errors.Is(err, fs.ErrNotExist) {
		return RecoveryRevoke{}, false, nil
	}
	if err != nil {
		return RecoveryRevoke{}, false, fmt.Errorf("read %s: %w", RecoveryRevokeFile, err)
	}
	var r RecoveryRevoke
	if err := json.Unmarshal(data, &r); err != nil {
		return RecoveryRevoke{}, false, fmt.Errorf("read %s: %w", RecoveryRevokeFile, err)
	}
	return r, true, nil
}

// RemoveRecoveryRevoke deletes the file; a missing file is no error.
func RemoveRecoveryRevoke(dataDir string) error {
	err := os.Remove(filepath.Join(dataDir, RecoveryRevokeFile))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", RecoveryRevokeFile, err)
	}
	return nil
}
