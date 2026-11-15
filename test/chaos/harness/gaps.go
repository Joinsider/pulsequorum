// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package harness

import (
	"errors"
	"sort"
	"time"
)

// MaxGap returns the largest uncovered time within [from, to] for a series of
// heartbeat timestamps (in any order).
//
// The series used is: the last timestamp before from (if any), then the
// timestamps inside the window, then the first timestamp after to (if any).
// When there is no timestamp before from, the series starts at from; when
// there is none after to, it ends at to. The result is the largest difference
// of consecutive values. A window without any timestamp therefore yields the
// window length.
func MaxGap(ts []time.Time, from, to time.Time) (time.Duration, error) {
	if !to.After(from) {
		return 0, errors.New("window end must be after its start")
	}
	sorted := append([]time.Time(nil), ts...)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a].Before(sorted[b]) })

	var prev, next *time.Time
	var inside []time.Time
	for i := range sorted {
		t := sorted[i]
		switch {
		case t.Before(from):
			tt := t
			prev = &tt
		case t.After(to):
			if next == nil {
				tt := t
				next = &tt
			}
		default:
			inside = append(inside, t)
		}
	}

	series := make([]time.Time, 0, len(inside)+3)
	if prev != nil {
		series = append(series, *prev)
	} else {
		series = append(series, from)
	}
	series = append(series, inside...)
	if next != nil {
		series = append(series, *next)
	} else {
		series = append(series, to)
	}

	var maxGap time.Duration
	for i := 1; i < len(series); i++ {
		if d := series[i].Sub(series[i-1]); d > maxGap {
			maxGap = d
		}
	}
	return maxGap, nil
}
