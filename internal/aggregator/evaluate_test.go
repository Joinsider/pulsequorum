// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package aggregator

import (
	"fmt"
	"slices"
	"testing"

	"github.com/joinsider/uptimecluster/internal/checker"
	"github.com/joinsider/uptimecluster/internal/store"
)

// TestNext covers the retry and recovery rules of FA-CON-03 (PF-20). Results
// are one second apart, starting at 0; U = up, D = down, G = degraded.
func TestNext(t *testing.T) {
	tests := []struct {
		name              string
		start             store.MonitorStatus
		retries, recovery int
		results           string
		want              []string // "STATUS@sinceMs"
	}{
		{"new monitor goes UP on first success", store.StatusPending, 1, 1, "U", []string{"UP@0"}},
		{"new monitor goes DOWN after retries+1 failures", store.StatusPending, 1, 1, "DD", []string{"DOWN@0"}},
		{"retries 0 goes DOWN at once", store.StatusUp, 0, 1, "D", []string{"DOWN@0"}},
		{"PENDING during retries, DOWN dates back", store.StatusUp, 2, 1, "DDDD", []string{"PENDING@0", "DOWN@0"}},
		{"success during retries returns to UP", store.StatusUp, 2, 1, "DDU", []string{"PENDING@0", "UP@2000"}},
		{"recovery needs consecutive successes", store.StatusDown, 1, 3, "UUDUUU", []string{"UP@3000"}},
		{"recovery 1 returns at once", store.StatusDown, 1, 1, "DU", []string{"UP@1000"}},
		{"DEGRADED and back", store.StatusUp, 1, 1, "GGU", []string{"DEGRADED@0", "UP@2000"}},
		{"recovery into DEGRADED", store.StatusDown, 1, 2, "GG", []string{"DEGRADED@0"}},
		{"DEGRADED fails like UP", store.StatusDegraded, 1, 1, "DD", []string{"PENDING@0", "DOWN@0"}},
		{"paused is never left", store.StatusPaused, 0, 1, "DUG", nil},
		{"maintenance is never left", store.StatusMaintenance, 0, 1, "DU", nil},
		{"full cycle", store.StatusPending, 1, 2, "UDDDUUU", []string{"UP@0", "PENDING@1000", "DOWN@1000", "UP@4000"}},
	}
	statuses := map[rune]checker.Status{'U': checker.StatusUp, 'D': checker.StatusDown, 'G': checker.StatusDegraded}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := &tracker{status: tt.start}
			var got []string
			for i, c := range tt.results {
				at := int64(i) * 1000
				tr.observe(checker.Result{Status: statuses[c]}, at)
				if to, since, ok := tr.next(statuses[c], at, tt.retries, tt.recovery); ok {
					got = append(got, fmt.Sprintf("%s@%d", to, since))
					tr.status = to
				}
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("transitions = %v, want %v", got, tt.want)
			}
		})
	}
}
