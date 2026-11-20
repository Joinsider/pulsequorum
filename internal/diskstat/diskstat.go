// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package diskstat reports the free space of the file system that holds a
// path (ADR 0025, DISK_FREE_ALERT). It is a thin wrapper around statfs.
package diskstat

import "errors"

// ErrUnsupported is returned on platforms without a statfs implementation.
var ErrUnsupported = errors.New("diskstat: not supported on this platform")
