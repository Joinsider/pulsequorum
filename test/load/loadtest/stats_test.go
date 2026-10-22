// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package main

import (
	"math"
	"testing"
)

func TestPercentile(t *testing.T) {
	hundred := make([]float64, 100)
	for i := range hundred {
		hundred[i] = float64(i + 1)
	}
	tests := []struct {
		name   string
		sorted []float64
		p      float64
		want   float64
	}{
		{"empty", nil, 50, 0},
		{"single value p50", []float64{7}, 50, 7},
		{"single value p99", []float64{7}, 99, 7},
		{"p0 clamps to first", hundred, 0, 1},
		{"p50 of 1..100", hundred, 50, 50},
		{"p95 of 1..100", hundred, 95, 95},
		{"p99 of 1..100", hundred, 99, 99},
		{"p100 is max", hundred, 100, 100},
		{"p95 of 1..10 rounds up the rank", []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 95, 10},
		{"p50 of 1..10", []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 50, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := percentile(tt.sorted, tt.p); got != tt.want {
				t.Errorf("percentile(p=%v) = %v, want %v", tt.p, got, tt.want)
			}
		})
	}
}

func TestSummarize(t *testing.T) {
	d := summarize([]float64{30, 10, 20})
	if d.Count != 3 || d.Max != 30 || d.P50 != 20 || math.Abs(d.Mean-20) > 1e-9 {
		t.Errorf("summarize = %+v", d)
	}
	if empty := summarize(nil); empty.Count != 0 || empty.Max != 0 {
		t.Errorf("summarize(nil) = %+v, want zero value", empty)
	}
}
