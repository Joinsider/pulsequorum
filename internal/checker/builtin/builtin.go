// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package builtin registers all built-in monitor types with checker.Default.
// Import it for its side effects in binaries and tests that need the types.
package builtin

import (
	_ "github.com/joinsider/uptimecluster/internal/checker/dnscheck"  // DNS
	_ "github.com/joinsider/uptimecluster/internal/checker/httpcheck" // HTTP(S)
	_ "github.com/joinsider/uptimecluster/internal/checker/icmpcheck" // ICMP
	_ "github.com/joinsider/uptimecluster/internal/checker/tcpcheck"  // TCP
)
