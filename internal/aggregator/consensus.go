// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package aggregator

import "github.com/joinsider/uptimecluster/internal/store"

// consensus returns the target status of one evaluation round from the
// heartbeats of its reporting nodes (FA-CON-01/02/05, ADR 0021). It is pure:
// the result depends only on the policy and the reported statuses.
//
// With n reporters, d of them DOWN and g DEGRADED:
//
//   - n = 0: no report, the result is "" (FA-CON-05).
//   - DOWN if the policy rule holds on d (majority: d > n/2, any: d >= 1,
//     all: d = n).
//   - PARTIAL if d >= 1 and the rule does not hold (positive, FA-CON-02).
//   - DEGRADED if d = 0 and the same rule holds on g.
//   - UP otherwise.
//
// An empty policy and unknown policies count as "majority" (the store default).
func consensus(policy string, reports []store.Heartbeat) store.MonitorStatus {
	n := len(reports)
	if n == 0 {
		return ""
	}
	var d, g int
	for _, r := range reports {
		switch r.Status {
		case store.CheckDown:
			d++
		case store.CheckDegraded:
			g++
		}
	}
	switch {
	case met(policy, d, n):
		return store.StatusDown
	case d >= 1:
		return store.StatusPartial
	case met(policy, g, n):
		return store.StatusDegraded
	default:
		return store.StatusUp
	}
}

// met reports whether count of n reporters satisfy the consensus policy.
func met(policy string, count, n int) bool {
	switch policy {
	case "any":
		return count >= 1
	case "all":
		return count == n
	default: // "majority", "" and unknown policies
		return 2*count > n
	}
}
