// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package alert

import (
	"strings"
	"testing"
)

// TestMaskURL covers FA-ALR-09: no credential of a channel URL is shown.
func TestMaskURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"telegram://123456:ABCdefGhIJKlmnOPQRstuVWXyz@telegram?chats=@ops", "telegram://***@telegram?chats=@ops"},
		{"ntfy://user:secretpass@ntfy.sh/alerts-topic", "ntfy://***@ntfy.sh/***"},
		{"gotify://gotify.example.com/AbCdEfGhIjKlMnO", "gotify://gotify.example.com/***"},
		{"smtp://u:p@mail.example.com:587/?from=a@example.com&to=b@example.com&password=x", "smtp://***@mail.example.com:587?from=a@example.com&password=***&to=b@example.com"},
		{"https://hooks.slack.com/services/T000/B000/XXXX?token=abc", "https://hooks.slack.com/***?token=***"},
		{"https://example.com", "https://example.com"},
		{"not a url", "***"},
		{"%zz://bad", "***"},
	}
	for _, tt := range tests {
		if got := MaskURL(tt.in); got != tt.want {
			t.Errorf("MaskURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRedact(t *testing.T) {
	target := Target{ //nolint:gosec // fake credentials under test
		URL:     "ntfy://alice:s3cr3t-pass@ntfy.example.com/private-topic?auth=tok%2Fen123",
		Headers: map[string]string{"Authorization": "Bearer abcdefgh"},
	}
	//nolint:gosec // fake credentials under test
	msg := `post "https://ntfy.example.com/private-topic": user alice:s3cr3t-pass, auth=tok%2Fen123 tok/en123, Bearer abcdefgh, ntfy://alice:s3cr3t-pass@ntfy.example.com/private-topic?auth=tok%2Fen123`
	got := Redact(msg, target.secrets()...)
	for _, secret := range []string{"s3cr3t-pass", "private-topic", "tok/en123", "tok%2Fen123", "abcdefgh", "alice"} {
		if strings.Contains(got, secret) {
			t.Errorf("redacted message still contains %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "ntfy.example.com") {
		t.Errorf("host should stay readable: %s", got)
	}
	if Redact("abc short", "abc") != "abc short" {
		t.Error("parts shorter than 4 characters must not be redacted")
	}
}
