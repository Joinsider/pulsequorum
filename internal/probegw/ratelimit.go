// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package probegw

import (
	"context"
	"sync"
	"time"
)

// tokenBucket throttles heartbeat uploads without dropping batches (ADR 0035 D5).
type tokenBucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
	now    func() time.Time
}

func newTokenBucket(rate, burst float64, now func() time.Time) *tokenBucket {
	if now == nil {
		now = time.Now
	}
	return &tokenBucket{
		rate:   rate,
		burst:  burst,
		tokens: burst,
		last:   now(),
		now:    now,
	}
}

func (b *tokenBucket) setRate(rate float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(b.now())
	b.rate = rate
}

func (b *tokenBucket) refillLocked(now time.Time) {
	if now.After(b.last) && b.rate > 0 {
		elapsed := now.Sub(b.last).Seconds()
		b.tokens = min(b.burst, b.tokens+elapsed*b.rate)
		b.last = now
	}
}

// wait blocks until n tokens are available or ctx ends.
func (b *tokenBucket) wait(ctx context.Context, n int) error {
	if n <= 0 {
		return nil
	}
	need := float64(n)
	for {
		b.mu.Lock()
		now := b.now()
		b.refillLocked(now)
		// Never wait for more than the bucket's burst capacity at once.
		req := min(need, b.burst)
		if b.tokens >= req {
			b.tokens -= req
			b.mu.Unlock()
			return nil
		}
		rate := b.rate
		if rate <= 0 {
			rate = 1
		}
		waitFor := time.Duration(((req - b.tokens) / rate) * float64(time.Second))
		if waitFor < time.Millisecond {
			waitFor = time.Millisecond
		}
		b.mu.Unlock()

		t := time.NewTimer(waitFor)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}
