// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package alert

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/store"
)

// TestValidateWebhookHost checks the save-time host rules of ADR 0030 for
// webhook channels.
func TestValidateWebhookHost(t *testing.T) {
	off := NewSender(&checker.Dialer{}, slog.Default())
	on := NewSender(&checker.Dialer{Policy: checker.BlockPrivate{}}, slog.Default())
	tests := []struct {
		url             string
		wantOff, wantOn string // "" accepted, else a substring of the error
	}{
		{"https://hooks.example.com/x", "", ""},
		{"https://localhost:9000/x", "", ""}, // names are checked when sending
		{"http://8.8.8.8/x", "", ""},
		{"http://127.0.0.1:8080/x", "", "blocked by SSRF protection"},
		{"http://169.254.169.254/latest/meta-data/", "", "cloud metadata"},
		{"http://[::1]:8080/x", "", "blocked by SSRF protection"},
		{"http://[::ffff:10.0.0.1]/x", "", "blocked by SSRF protection"},
		{"http://2130706433/x", "non-canonical", "non-canonical"},
		{"http://0x7f.1/x", "non-canonical", "non-canonical"},
		{"http://127.1:8080/x", "non-canonical", "non-canonical"},
		{"http://[fe80::1%25eth0]/x", "zones", "zones"},
	}
	for _, tt := range tests {
		for _, c := range []struct {
			s    *Sender
			want string
		}{{off, tt.wantOff}, {on, tt.wantOn}} {
			err := c.s.Validate(Target{Kind: store.ChannelWebhook, URL: tt.url})
			switch {
			case c.want == "" && err != nil:
				t.Errorf("%s (policy %v): %v, want accepted", tt.url, c.s.policy() != nil, err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Errorf("%s (policy %v): %v, want error containing %q", tt.url, c.s.policy() != nil, err, c.want)
			}
		}
	}
}

// TestValidateShoutrrrHost checks the save-time host rules for Shoutrrr
// services that contact the host of their URL (ADR 0030).
func TestValidateShoutrrrHost(t *testing.T) {
	off := NewSender(&checker.Dialer{}, slog.Default())
	on := NewSender(&checker.Dialer{Policy: checker.BlockPrivate{}}, slog.Default())
	tests := []struct {
		url             string
		wantOff, wantOn string
	}{
		{"gotify://gotify.example.com/AAAAAAAAAAAAAAA", "", ""},
		{"gotify://127.0.0.1/AAAAAAAAAAAAAAA", "", "blocked by SSRF protection"},
		{"gotify://0x7f.1/AAAAAAAAAAAAAAA", "non-canonical", "non-canonical"},
		{"generic://169.254.169.254/hook", "", "cloud metadata"},
		{"generic+https://10.0.0.1/hook", "", "blocked by SSRF protection"},
		{"smtp://user:pass@2130706433:25/?from=a@example.com&to=b@example.com", "non-canonical", "non-canonical"},
		{"ntfy://[::1]/topic", "", "blocked by SSRF protection"},
		{"telegram://123456:ABCdef@telegram?chats=1&apiurl=http://127.0.0.1:8081", "", "blocked by SSRF protection"},
		{"telegram://123456:ABCdef@telegram?chats=1", "", ""},
		// Fixed-host services use the URL host for IDs, which may be numeric.
		{"discord://token@123456789012345678", "", ""},
	}
	for _, tt := range tests {
		for _, c := range []struct {
			s    *Sender
			want string
		}{{off, tt.wantOff}, {on, tt.wantOn}} {
			err := c.s.validateShoutrrrHost(tt.url)
			switch {
			case c.want == "" && err != nil:
				t.Errorf("%s (policy %v): %v, want accepted", tt.url, c.s.policy() != nil, err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Errorf("%s (policy %v): %v, want error containing %q", tt.url, c.s.policy() != nil, err, c.want)
			}
		}
	}
	// Through Validate, the message is redacted and permanent.
	err := on.Validate(Target{Kind: store.ChannelShoutrrr, URL: "gotify://127.0.0.1/AAAAAAAAAAAAAAA"})
	if err == nil || !strings.Contains(err.Error(), "blocked by SSRF protection") {
		t.Errorf("Validate gotify 127.0.0.1 = %v", err)
	}
}

// TestValidateMessageKeepsPrefix checks that masking a short path segment
// does not garble the fixed text of the message.
func TestValidateMessageKeepsPrefix(t *testing.T) {
	on := NewSender(&checker.Dialer{Policy: checker.BlockPrivate{}}, slog.Default())
	err := on.Validate(Target{Kind: store.ChannelWebhook, URL: "http://10.99.0.10/hook"})
	if err == nil || !strings.HasPrefix(err.Error(), "invalid webhook URL: ") {
		t.Fatalf("Validate = %v, want the plain prefix", err)
	}
	err = on.Validate(Target{Kind: store.ChannelWebhook, URL: "http://10.0.0.1/s3cr3tt0ken"})
	if err == nil || strings.Contains(err.Error(), "s3cr3tt0ken") {
		t.Fatalf("Validate = %v, want the path segment masked", err)
	}
}
