// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package backoff provides exponential backoff with jitter and context-aware
// sleeping.
package backoff

import (
	"context"
	"math/rand/v2"
	"time"
)

// Backoff computes exponential reconnect delays with jitter (ADR 0006, ADR 0034).
// It is not safe for concurrent use by multiple goroutines.
type Backoff struct {
	min, max, cur time.Duration
	fullJitter    bool
	randIntN      func(int64) int64
}

// HalfJitter returns a Backoff whose Wait returns delays in [cur/2, cur) and
// doubles cur up to maxD on each call.
func HalfJitter(minD, maxD time.Duration) *Backoff {
	return &Backoff{
		min: minD,
		max: maxD,
		cur: minD,
	}
}

// FullJitter returns a Backoff whose Wait returns delays in [0, cur) where
// cur = min(maxD, minD*2^n). If randIntN is nil, math/rand/v2.Int64N is used.
func FullJitter(minD, maxD time.Duration, randIntN func(int64) int64) *Backoff {
	if randIntN == nil {
		randIntN = func(n int64) int64 {
			return rand.Int64N(n) //nolint:gosec // G404: jitter, not a secret
		}
	}
	initial := min(minD, maxD)
	return &Backoff{
		min:        initial,
		max:        maxD,
		cur:        initial,
		fullJitter: true,
		randIntN:   randIntN,
	}
}

// Wait returns the next jittered delay and advances the backoff state.
func (b *Backoff) Wait() time.Duration {
	d := b.cur
	if d > 0 && b.max > 0 && d > b.max/2 {
		b.cur = b.max
	} else {
		b.cur = min(2*d, b.max)
	}
	if d <= 0 {
		return 0
	}
	if b.fullJitter {
		return time.Duration(b.randIntN(int64(d)))
	}
	half := d / 2
	return half + rand.N(d-half) //nolint:gosec // G404: jitter, not a secret
}

// Reset restores the backoff delay to its initial minimum.
func (b *Backoff) Reset() {
	b.cur = b.min
}

// Sleep waits for d and reports false if ctx is cancelled first.
// If d <= 0, it returns ctx.Err() == nil without waiting.
func Sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
