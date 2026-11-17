// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

//go:build !linux && !darwin

package diskstat

// FreeRatio is not implemented on this platform and always returns ErrUnsupported.
func FreeRatio(string) (float64, error) {
	return 0, ErrUnsupported
}
