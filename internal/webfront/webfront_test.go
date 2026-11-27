// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package webfront

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestParseNodes(t *testing.T) {
	got, err := ParseNodes(" https://N1.example.com/ , https://n2.example.com:8443,,https://n1.example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"https://n1.example.com", "https://n2.example.com:8443"}; !slices.Equal(got, want) {
		t.Fatalf("ParseNodes = %v, want %v", got, want)
	}
	if got, err := ParseNodes(""); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty: %v, %v; want empty non-nil list", got, err)
	}
	for _, bad := range []string{"http://n1.example.com", "https://n1.example.com/app", "https://a.example\"; x"} {
		if _, err := ParseNodes("https://ok.example.com," + bad); err == nil {
			t.Errorf("ParseNodes accepted %q", bad)
		}
	}
}

func TestSPACSPNodes(t *testing.T) {
	got := SPACSP("https://id.example.test/realms/x", []string{"https://n1.example.com", "http://localhost:8080", "https://n1.example.com", "https://x.example; script-src *"})
	want := "connect-src 'self' https://n1.example.com wss://n1.example.com http://localhost:8080 ws://localhost:8080 https://id.example.test;"
	if !strings.Contains(got, want) || strings.Contains(got, "script-src *") {
		t.Fatalf("SPACSP = %q", got)
	}
	if SPACSP("", nil) != CSPSPA {
		t.Fatal("no nodes, no issuer must give the base policy")
	}
	for _, issuer := range []string{"ftp://id.example", "://x", "https://id.example$host"} {
		if SPACSP(issuer, nil) != CSPSPA {
			t.Errorf("issuer %q widened the policy", issuer)
		}
	}
}

func TestSPAHeaders(t *testing.T) {
	h := SPAHeaders("CSP", true)
	names := make([]string, len(h))
	for i, x := range h {
		names[i] = x.Name
	}
	for _, want := range []string{"X-Content-Type-Options", "Referrer-Policy", "X-Frame-Options", "Cross-Origin-Opener-Policy", "Content-Security-Policy", "Strict-Transport-Security"} {
		if !slices.Contains(names, want) {
			t.Errorf("missing %s", want)
		}
	}
	if slices.ContainsFunc(SPAHeaders("CSP", false), func(x Header) bool { return x.Name == "Strict-Transport-Security" }) {
		t.Error("HSTS without TLS")
	}
}

func TestRenderedFiles(t *testing.T) {
	headers := SPAHeaders(SPACSP("", []string{"https://n1.example.com"}), true)
	conf, err := NginxConf(headers)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(conf, `add_header Content-Security-Policy "default-src 'self';`) || !strings.Contains(conf, "\" always;\n") {
		t.Fatalf("nginx conf:\n%s", conf)
	}
	file, err := HeadersFile(headers)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(file, "/*\n  X-Content-Type-Options: nosniff\n") || !strings.Contains(file, "/config.json\n  Cache-Control: no-store\n") {
		t.Fatalf("_headers:\n%s", file)
	}

	for _, v := range []string{"a\"b", "a\nb", "a\rb", "a$host", "a\\b", "a\x00b"} {
		bad := []Header{{"X-Test", v}}
		if _, err := NginxConf(bad); !errors.Is(err, errUnsafeValue) {
			t.Errorf("NginxConf accepted %q", v)
		}
		if _, err := HeadersFile(bad); !errors.Is(err, errUnsafeValue) {
			t.Errorf("HeadersFile accepted %q", v)
		}
	}
}
