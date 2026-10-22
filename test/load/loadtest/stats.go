// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package main

import (
	"math"
	"slices"
)

// distribution summarizes a set of measurements in milliseconds.
type distribution struct {
	Count int     `json:"count"`
	Mean  float64 `json:"meanMs"`
	P50   float64 `json:"p50Ms"`
	P95   float64 `json:"p95Ms"`
	P99   float64 `json:"p99Ms"`
	Max   float64 `json:"maxMs"`
}

func summarize(values []float64) distribution {
	if len(values) == 0 {
		return distribution{}
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	var sum float64
	for _, v := range sorted {
		sum += v
	}
	return distribution{
		Count: len(sorted),
		Mean:  sum / float64(len(sorted)),
		P50:   percentile(sorted, 50),
		P95:   percentile(sorted, 95),
		P99:   percentile(sorted, 99),
		Max:   sorted[len(sorted)-1],
	}
}

// percentile returns the nearest-rank p-th percentile (0..100) of an
// ascending sorted slice. It returns 0 for an empty slice.
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(n)))
	rank = min(max(rank, 1), n)
	return sorted[rank-1]
}
