// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package auth

import (
	"encoding/base64"
	"regexp"
	"testing"
)

func TestNewOpaqueToken(t *testing.T) {
	alphabet := regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	seen := map[string]bool{}
	for range 200 {
		tok, err := NewOpaqueToken()
		if err != nil {
			t.Fatal(err)
		}
		if !alphabet.MatchString(tok) {
			t.Fatalf("token %q is not 43 base64url characters", tok)
		}
		raw, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil || len(raw) != 32 {
			t.Fatalf("token decodes to %d bytes, %v; want 32 (256 bit)", len(raw), err)
		}
		if seen[tok] {
			t.Fatalf("duplicate token %q", tok)
		}
		seen[tok] = true
	}
}

func TestHashToken(t *testing.T) {
	hexDigits := regexp.MustCompile(`^[0-9a-f]{64}$`)
	h1 := HashToken("abc")
	if !hexDigits.MatchString(h1) {
		t.Fatalf("HashToken = %q, want 64 lowercase hex characters", h1)
	}
	if h2 := HashToken("abc"); h1 != h2 {
		t.Errorf("HashToken is not deterministic: %q vs %q", h1, h2)
	}
	if HashToken("abd") == h1 {
		t.Error("different tokens produce the same hash")
	}
}

func TestTokenMatches(t *testing.T) {
	tok, err := NewOpaqueToken()
	if err != nil {
		t.Fatal(err)
	}
	stored := HashToken(tok)
	tests := []struct {
		name  string
		token string
		hash  string
		want  bool
	}{
		{"matching token", tok, stored, true},
		{"other token", tok + "x", stored, false},
		{"empty token", "", stored, false},
		{"hash of other token", tok, HashToken("other"), false},
		{"empty hash", tok, "", false},
		{"raw token stored as hash", tok, tok, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TokenMatches(tt.token, tt.hash); got != tt.want {
				t.Errorf("TokenMatches = %v, want %v", got, tt.want)
			}
		})
	}
}
