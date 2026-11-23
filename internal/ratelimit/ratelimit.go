// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package ratelimit provides a per-key token-bucket rate limiter. It is used
// to throttle login and public HTTP endpoints (FA-API-07).
//
// The limiter keeps one bucket per key. Each bucket holds up to Burst tokens
// and refills at Rate tokens per second; a request consumes one token. The
// number of buckets is bounded by Limit to protect against memory exhaustion
// through many distinct keys (for example spoofed client addresses).
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// DefaultLimit is the number of keys a Limiter created by New keeps before
// it starts evicting buckets. A zero Limit field also falls back to it.
const DefaultLimit = 100_000

// Limiter allows Burst requests per key at once and refills Rate tokens per second.
//
// A Limiter must not be copied after first use. The zero value is usable once
// Rate and Burst are set; Limit falls back to DefaultLimit when it is not
// positive. Limiter is safe for concurrent use.
type Limiter struct {
	// Rate is the number of tokens refilled per second. It should be > 0;
	// a non-positive Rate disables refilling.
	Rate float64
	// Burst is the bucket size, the maximum number of requests allowed at once.
	// Values below 1 are treated as 1.
	Burst int
	// Limit is the maximum number of keys kept. When the limit is reached and
	// a new key arrives, buckets that are full again are dropped first; if none
	// are full, the least recently used bucket is dropped.
	Limit int
	// Now returns the current time. Nil means time.Now. It is intended to be
	// replaced in tests.
	Now func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

// bucket is the state of a single key. tokens is the value as of last.
type bucket struct {
	tokens float64
	last   time.Time
}

// New returns a limiter with rate tokens per second, the given burst and a key
// limit of DefaultLimit.
func New(rate float64, burst int) *Limiter {
	return &Limiter{Rate: rate, Burst: burst, Limit: DefaultLimit}
}

// Allow takes one token for key. If none is available it returns false and how
// long until the next token is available. The returned duration is always > 0
// when ok is false and is rounded up, so waiting for it is sufficient. When ok
// is true the duration is 0.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	rate := l.rate()
	burst := float64(l.burst())

	if l.buckets == nil {
		l.buckets = make(map[string]*bucket)
	}

	b, found := l.buckets[key]
	if found {
		b.tokens = l.projected(b, now, rate, burst)
		if now.After(b.last) {
			b.last = now
		}
	} else {
		if len(l.buckets) >= l.limit() {
			l.evict(now, rate, burst)
		}
		b = &bucket{tokens: burst, last: now}
		l.buckets[key] = b
	}

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, retryAfterFor(b.tokens, rate)
}

// now returns the current time from Now, or time.Now if Now is nil.
func (l *Limiter) now() time.Time {
	if l.Now == nil {
		return time.Now()
	}
	return l.Now()
}

// rate returns the refill rate, or 0 if it is not positive.
func (l *Limiter) rate() float64 {
	if math.IsNaN(l.Rate) || l.Rate <= 0 {
		return 0
	}
	return l.Rate
}

// burst returns the bucket size, at least 1.
func (l *Limiter) burst() int {
	if l.Burst < 1 {
		return 1
	}
	return l.Burst
}

// limit returns the key limit, falling back to DefaultLimit if not positive.
func (l *Limiter) limit() int {
	if l.Limit < 1 {
		return DefaultLimit
	}
	return l.Limit
}

// projected returns the number of tokens b would hold at now, without
// modifying b. The refill is capped at burst.
func (l *Limiter) projected(b *bucket, now time.Time, rate, burst float64) float64 {
	elapsed := now.Sub(b.last)
	if elapsed <= 0 {
		return b.tokens
	}
	t := b.tokens + elapsed.Seconds()*rate
	if t > burst {
		t = burst
	}
	return t
}

// evict makes room for one new key. It first drops all buckets that would be
// full at now, and if the map is still at the limit it drops the least
// recently used bucket. Ties in last use are broken by key for determinism.
// The caller must hold l.mu.
func (l *Limiter) evict(now time.Time, rate, burst float64) {
	for k, b := range l.buckets {
		if l.projected(b, now, rate, burst) >= burst {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) < l.limit() {
		return
	}

	var lruKey string
	var lruTime time.Time
	first := true
	for k, b := range l.buckets {
		if first || b.last.Before(lruTime) || (b.last.Equal(lruTime) && k < lruKey) {
			lruKey, lruTime, first = k, b.last, false
		}
	}
	if !first {
		delete(l.buckets, lruKey)
	}
}

// retryAfterFor returns the time until the bucket holds one whole token, given
// its current token count and a positive rate. It is rounded up to the next
// nanosecond and is never 0.
func retryAfterFor(tokens, rate float64) time.Duration {
	if rate <= 0 {
		return time.Duration(math.MaxInt64)
	}
	ns := math.Ceil((1 - tokens) / rate * float64(time.Second))
	if ns >= float64(math.MaxInt64) {
		return time.Duration(math.MaxInt64)
	}
	if ns < 1 {
		ns = 1
	}
	return time.Duration(ns)
}
