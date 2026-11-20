// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

//go:build linux || darwin

package diskstat

import (
	"fmt"
	"syscall"
)

// FreeRatio returns the share of blocks available to unprivileged users on the
// file system that contains path, in the range (0, 1].
func FreeRatio(path string) (float64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	if st.Blocks == 0 {
		return 0, fmt.Errorf("statfs %s: file system reports no blocks", path)
	}
	return float64(st.Bavail) / float64(st.Blocks), nil
}
