// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package sqlite

import (
	"context"
	"strings"
	"testing"
)

// TestLatestHeartbeatsByMonitorPlan guards the batch query of the monitor list
// (NFA-PERF-03): every monitor must be served by a seek on heartbeats_monitor_ts,
// never by a scan of the heartbeats.
func TestLatestHeartbeatsByMonitorPlan(t *testing.T) {
	m := openMetrics(t, t.TempDir())
	ctx := context.Background()
	rows, err := m.db.reader.QueryContext(ctx, "EXPLAIN QUERY PLAN "+latestHeartbeatsByMonitorSQL, `["a","b"]`, "t", "t")
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")
	t.Logf("query plan:\n%s", joined)
	if !strings.Contains(joined, "heartbeats_monitor_ts") {
		t.Errorf("plan does not use heartbeats_monitor_ts:\n%s", joined)
	}
	for _, line := range plan {
		if strings.HasPrefix(line, "SCAN") && strings.Contains(line, "heartbeats") {
			t.Errorf("plan scans heartbeats: %s\n%s", line, joined)
		}
	}
}
