// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package aggregator

import (
	"strings"
	"testing"

	"github.com/joinsider/uptimecluster/internal/store"
)

// nodeLetters names the nodes of a report string, one letter per node.
const nodeLetters = "abcdefghijklmnopqrstuvwxyz"

// reportsOf builds one heartbeat per letter: U = UP, D = DOWN, G = DEGRADED.
func reportsOf(s string) []store.Heartbeat {
	out := make([]store.Heartbeat, 0, len(s))
	for i, c := range s {
		var st store.CheckStatus
		switch c {
		case 'U':
			st = store.CheckUp
		case 'D':
			st = store.CheckDown
		case 'G':
			st = store.CheckDegraded
		}
		out = append(out, store.Heartbeat{NodeID: string(nodeLetters[i]), Status: st})
	}
	return out
}

// TestConsensus covers the table of ADR 0021 for all policies and combinations
// of results (FA-CON-01/02/05).
func TestConsensus(t *testing.T) {
	const (
		up       = store.StatusUp
		down     = store.StatusDown
		degraded = store.StatusDegraded
		partial  = store.StatusPartial
		none     = store.MonitorStatus("")
	)
	tests := []struct {
		policy, reports string
		want            store.MonitorStatus
	}{
		// n = 0: no report.
		{"majority", "", none},
		{"any", "", none},
		{"all", "", none},

		// n = 1.
		{"majority", "U", up},
		{"majority", "D", down},
		{"majority", "G", degraded},
		{"any", "D", down},
		{"all", "D", down},
		{"any", "G", degraded},
		{"all", "G", degraded},

		// n = 2.
		{"majority", "UU", up},
		{"majority", "UD", partial}, // 1 of 2 is not a majority
		{"majority", "DD", down},
		{"majority", "UG", up},
		{"majority", "DG", partial},
		{"majority", "GG", degraded},
		{"any", "UD", down},
		{"any", "UU", up},
		{"any", "UG", degraded},
		{"any", "GG", degraded},
		{"all", "UD", partial},
		{"all", "DD", down},
		{"all", "UU", up},
		{"all", "GG", degraded},
		{"all", "GU", up},

		// n = 3.
		{"majority", "UUU", up},
		{"majority", "DUU", partial}, // one of three is PARTIAL, not DOWN
		{"majority", "DDU", down},
		{"majority", "DDD", down},
		{"majority", "DGG", partial},
		{"majority", "GGU", degraded},
		{"majority", "GUU", up},
		{"any", "DUU", down},
		{"any", "UUU", up},
		{"any", "GUU", degraded},
		{"all", "DUU", partial},
		{"all", "DDU", partial},
		{"all", "DDD", down},
		{"all", "GGU", up}, // DEGRADED needs all reporters degraded
		{"all", "GGG", degraded},

		// The empty policy is the default majority.
		{"", "UD", partial},
		{"", "DDU", down},
		{"", "UUU", up},
	}
	for _, tt := range tests {
		t.Run(tt.policy+"/"+tt.reports, func(t *testing.T) {
			if got := consensus(tt.policy, reportsOf(tt.reports)); got != tt.want {
				t.Fatalf("consensus(%q, %q) = %q, want %q", tt.policy, tt.reports, got, tt.want)
			}
		})
	}
}

// TestConsensusExhaustive checks every combination of up to four results
// against the rules of ADR 0021, for every policy.
func TestConsensusExhaustive(t *testing.T) {
	for n := 1; n <= 4; n++ {
		for combo := range pow(3, n) {
			var sb strings.Builder
			for i := range n {
				sb.WriteByte("UDG"[combo/pow(3, i)%3])
			}
			s := sb.String()
			reports := reportsOf(s)
			d, g := strings.Count(s, "D"), strings.Count(s, "G")
			for _, policy := range []string{"majority", "any", "all", ""} {
				got := consensus(policy, reports)
				switch {
				case met(policy, d, n):
					if got != store.StatusDown {
						t.Errorf("%s %q: got %q, want DOWN", policy, s, got)
					}
				case d >= 1:
					if got != store.StatusPartial {
						t.Errorf("%s %q: got %q, want PARTIAL", policy, s, got)
					}
				case met(policy, g, n):
					if got != store.StatusDegraded {
						t.Errorf("%s %q: got %q, want DEGRADED", policy, s, got)
					}
				default:
					if got != store.StatusUp {
						t.Errorf("%s %q: got %q, want UP", policy, s, got)
					}
				}
			}
		}
	}
}

func pow(b, e int) int {
	r := 1
	for range e {
		r *= b
	}
	return r
}
