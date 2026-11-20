// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import (
	"regexp"
	"testing"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestIdempotencyKeyIsDeterministic(t *testing.T) {
	a := idempotencyKey("load-", 42)
	b := idempotencyKey("load-", 42)
	if a != b {
		t.Errorf("same input gave different keys: %s vs %s", a, b)
	}
	if !uuidPattern.MatchString(a) {
		t.Errorf("key %q is not a version 5 UUID", a)
	}
}

func TestIdempotencyKeyDiffers(t *testing.T) {
	seen := map[string]string{}
	for _, tc := range []struct {
		prefix string
		index  int
	}{{"load-", 0}, {"load-", 1}, {"load-", 42}, {"other-", 0}, {"other-", 1}} {
		k := idempotencyKey(tc.prefix, tc.index)
		if prev, ok := seen[k]; ok {
			t.Errorf("key collision between %s and %s%d", prev, tc.prefix, tc.index)
		}
		seen[k] = tc.prefix
	}
}

func TestUUIDv5KnownValue(t *testing.T) {
	// RFC 4122 example: UUIDv5 of the DNS namespace and "www.example.com".
	ns := parseUUID("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	got := uuidV5(ns, "www.example.com")
	const want = "2ed6657d-e927-568b-95e1-2665a8aea6a2"
	if got != want {
		t.Errorf("uuidV5 = %s, want %s", got, want)
	}
}

func TestTargetIndexFromURL(t *testing.T) {
	tests := []struct {
		raw    string
		want   int
		wantOK bool
	}{
		{"http://host.containers.internal:18900/t/17", 17, true},
		{"http://127.0.0.1:18900/t/0", 0, true},
		{"https://example.com/status", 0, false},
		{"http://example.com/t/abc", 0, false},
		{"http://example.com/t/-1", 0, false},
	}
	for _, tt := range tests {
		got, ok := targetIndexFromURL(tt.raw)
		if ok != tt.wantOK || (ok && got != tt.want) {
			t.Errorf("targetIndexFromURL(%q) = %d, %v; want %d, %v", tt.raw, got, ok, tt.want, tt.wantOK)
		}
	}
}
