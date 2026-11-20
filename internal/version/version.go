// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package version exposes build metadata injected via -ldflags.
package version

// Set at build time, e.g.
// -X github.com/joinsider/pulsequorum/internal/version.Version=v0.1.0
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)
