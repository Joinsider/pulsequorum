// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package auth

import (
	"sync"
	"time"
)

// Throttle slows down password guessing per account and per IP (FA-AUTH-04,
// PF-33). After Free failures each further failure doubles the wait, up to
// Max. Entries expire after Max without failures. State is per node; in a
// cluster an attacker gains at most a factor of the number of voters.
type Throttle struct {
	Free  int           // failures without delay
	Base  time.Duration // first delay
	Max   time.Duration // longest delay
	Limit int           // entries kept; oldest are dropped first

	mu      sync.Mutex
	entries map[string]*throttleEntry
}

type throttleEntry struct {
	failures int
	until    time.Time // no attempt before this time
	last     time.Time
}

// NewThrottle returns a throttle with the given number of free failures.
func NewThrottle(free int) *Throttle {
	return &Throttle{Free: free, Base: time.Second, Max: 15 * time.Minute, Limit: 100_000}
}

// Wait returns how long key must wait before the next attempt (0 = allowed).
func (t *Throttle) Wait(key string, now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entries[key]
	if e == nil || !now.Before(e.until) {
		return 0
	}
	return e.until.Sub(now)
}

// Fail records a failed attempt.
func (t *Throttle) Fail(key string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = map[string]*throttleEntry{}
	}
	e := t.entries[key]
	if e == nil {
		if len(t.entries) >= t.Limit {
			t.evict(now)
		}
		e = &throttleEntry{}
		t.entries[key] = e
	}
	e.failures++
	e.last = now
	if over := e.failures - t.Free; over > 0 {
		d := t.Base
		for i := 1; i < over && d < t.Max; i++ {
			d *= 2
		}
		e.until = now.Add(min(d, t.Max))
	}
}

// Succeed clears the failures of key.
func (t *Throttle) Succeed(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, key)
}

// evict drops expired entries, or the oldest one if none expired.
func (t *Throttle) evict(now time.Time) {
	var oldest string
	var oldestAt time.Time
	for k, e := range t.entries {
		if now.Sub(e.last) > t.Max && !now.Before(e.until) {
			delete(t.entries, k)
			continue
		}
		if oldest == "" || e.last.Before(oldestAt) {
			oldest, oldestAt = k, e.last
		}
	}
	if len(t.entries) >= t.Limit && oldest != "" {
		delete(t.entries, oldest)
	}
}
