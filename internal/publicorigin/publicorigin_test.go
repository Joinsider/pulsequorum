// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package publicorigin

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		raw  string
		want string // "" means rejected
	}{
		{"https://n1.example.com", "https://n1.example.com"},
		{"https://n1.example.com/", "https://n1.example.com"},
		{"HTTPS://N1.Example.COM", "https://n1.example.com"},
		{"https://n1.example.com:443", "https://n1.example.com"},
		{"https://n1.example.com:8443", "https://n1.example.com:8443"},
		{"https://n1.example.com:08443", "https://n1.example.com:8443"},
		{"https://192.0.2.10", "https://192.0.2.10"},
		{"https://[2001:DB8::1]:8443", "https://[2001:db8::1]:8443"},
		{"http://localhost:8080", "http://localhost:8080"},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"http://[::1]:8080", "http://[::1]:8080"},
		{"http://localhost:80", "http://localhost"},
		{"https://localhost", "https://localhost"},

		// Scheme and loopback rule.
		{"http://n1.example.com", ""},
		{"http://127.0.0.2", ""},
		{"http://localhost.example.com", ""},
		{"ftp://n1.example.com", ""},
		{"wss://n1.example.com", ""},
		{"//n1.example.com", ""},
		{"n1.example.com", ""},
		{"https:n1.example.com", ""},
		// Path, query, fragment, userinfo.
		{"https://n1.example.com/app", ""},
		{"https://n1.example.com//", ""},
		{"https://n1.example.com/%2e", ""},
		{"https://n1.example.com?", ""},
		{"https://n1.example.com?a=1", ""},
		{"https://n1.example.com#", ""},
		{"https://n1.example.com#x", ""},
		{"https://user@n1.example.com", ""},
		{"https://user:pw@n1.example.com", ""},
		{"https://n1.example.com\\@evil.example", ""},
		// Host.
		{"https://", ""},
		{"https://:8443", ""},
		{"https://n1_example.com", ""},
		{"https://-n1.example.com", ""},
		{"https://n1-.example.com", ""},
		{"https://n1..example.com", ""},
		{"https://n1.example.com.", ""},
		{"https://bücher.example", ""},
		{"https://xn--bcher-kva.example", "https://xn--bcher-kva.example"},
		{"https://127.1", ""},
		{"https://0x7f000001", ""},
		{"https://n1.example.123", ""},
		{"https://n1.0xab", ""},
		{"https://123.example.com", "https://123.example.com"},
		{"https://[fe80::1%25eth0]", ""},
		{"https://[::ffff:127.0.0.1]", ""},
		{"https://[::1", ""},
		{"https://[::1]x", ""},
		{"https://::1", ""},
		{"https://" + strings.Repeat("a", 64) + ".example", ""},
		// Port.
		{"https://n1.example.com:", ""},
		{"https://[::1]:", ""},
		{"https://n1.example.com:0", ""},
		{"https://n1.example.com:65536", ""},
		{"https://n1.example.com:+443", ""},
		{"https://n1.example.com:44a", ""},
		// Characters and length.
		{"", ""},
		{" https://n1.example.com", ""},
		{"https://n1.example.com\r\nX-Evil: 1", ""},
		{"https://n1.example.com\t", ""},
		{"https://n1.exa\"mple.com", ""},
		{"https://n1.exa;mple.com", ""},
		{"https://" + strings.Repeat("a.", 130) + "com", ""},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, err := Normalize(tt.raw)
			if tt.want == "" {
				if err == nil {
					t.Fatalf("Normalize(%q) = %q, want error", tt.raw, got)
				}
				if !errors.Is(err, errInvalidPublicURL) {
					t.Fatalf("error %v does not wrap errInvalidPublicURL", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("Normalize(%q) = %q, %v; want %q", tt.raw, got, err, tt.want)
			}
		})
	}
}

// FuzzNormalize checks the invariants every accepted origin must
// keep: it is short, header-safe, idempotent and a plain origin.
func FuzzNormalize(f *testing.F) {
	for _, s := range []string{
		"https://n1.example.com", "http://[::1]:8080", "https://a:443/", "https://u@h", "https://h?#",
		"HTTP://LOCALHOST:80", "https://[2001:db8::1]", "https://h\r\n", "https://h/%2F",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		got, err := Normalize(raw)
		if err != nil {
			return
		}
		if len(got) > MaxLen {
			t.Fatalf("origin %q longer than %d", got, MaxLen)
		}
		if strings.ContainsAny(got, " \t\r\n\"';,?#@\\") || got != strings.ToLower(got) {
			t.Fatalf("origin %q has unsafe characters or upper case", got)
		}
		again, err := Normalize(got)
		if err != nil || again != got {
			t.Fatalf("not idempotent: %q -> %q (%v)", got, again, err)
		}
		u, err := url.Parse(got)
		if err != nil || u.Path != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			t.Fatalf("origin %q is not a plain origin", got)
		}
		if u.Scheme != "https" && (u.Scheme != "http" || !isLoopbackHost(u.Hostname())) {
			t.Fatalf("origin %q has a disallowed scheme", got)
		}
	})
}
