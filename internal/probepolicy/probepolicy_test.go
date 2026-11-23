// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package probepolicy

import (
	"net/netip"
	"strings"
	"testing"
)

func mustTargets(t *testing.T, raw string) *Targets {
	t.Helper()
	tg, err := ParseTargets(raw)
	if err != nil {
		t.Fatalf("ParseTargets(%q): %v", raw, err)
	}
	return tg
}

func TestParseTargetsValid(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string // String() of the parsed value
	}{
		{name: "single IPv4", raw: "10.0.0.5", want: "10.0.0.5"},
		{name: "IPv4 CIDR", raw: "10.0.0.0/8", want: "10.0.0.0/8"},
		{name: "IPv4 with port", raw: "10.0.0.5:443", want: "10.0.0.5:443"},
		{name: "IPv4 with port range", raw: "10.0.0.0/8:443-445", want: "10.0.0.0/8:443-445"},
		{name: "bare IPv6", raw: "2001:db8::1", want: "2001:db8::1"},
		{name: "IPv6 CIDR without port", raw: "2001:db8::/48", want: "2001:db8::/48"},
		{name: "IPv6 CIDR with port", raw: "[2001:db8::/48]:443", want: "[2001:db8::/48]:443"},
		{name: "IPv6 loopback with port", raw: "[::1]:8080", want: "[::1]:8080"},
		{name: "whitespace around entries", raw: "  10.0.0.1 , [::1]:8080 ,10.0.0.0/8:443 ", want: "10.0.0.1,[::1]:8080,10.0.0.0/8:443"},
		{name: "any", raw: "any", want: "any"},
		{name: "mixed list", raw: "10.20.0.0/16:443,[2001:db8::/48]:443,10.0.0.53", want: "10.20.0.0/16:443,[2001:db8::/48]:443,10.0.0.53"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tg, err := ParseTargets(tt.raw)
			if err != nil {
				t.Fatalf("ParseTargets(%q): %v", tt.raw, err)
			}
			if got := tg.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}

	if tg, err := ParseTargets(""); err != nil || tg != nil {
		t.Errorf(`ParseTargets("") = %v, %v; want nil, nil`, tg, err)
	}
	if tg, err := ParseTargets("any"); err != nil || !tg.Any() {
		t.Errorf(`ParseTargets("any").Any() = false, err %v`, err)
	}
	if tg, err := ParseTargets(strings.Repeat("10.0.0.1,", 255) + "10.0.0.1"); err != nil || tg == nil {
		t.Errorf("256 entries rejected: %v", err)
	}
}

func TestParseTargetsInvalid(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "hostname", raw: "example.com"},
		{name: "host bits set", raw: "10.0.0.5/16"},
		{name: "port zero in entry", raw: "10.0.0.0/8:0"},
		{name: "port out of range", raw: ":70000"},
		{name: "port with leading zero", raw: "10.0.0.1:080"},
		{name: "reversed range", raw: "10.0.0.1:90-80"},
		{name: "empty entry", raw: "10.0.0.1,,10.0.0.2"},
		{name: "trailing comma", raw: "10.0.0.1,"},
		{name: "any inside list", raw: "any,10.0.0.1"},
		{name: "IPv6 zone", raw: "fe80::1%eth0"},
		{name: "IPv4-mapped IPv6", raw: "::ffff:10.0.0.1"},
		{name: "brackets around IPv4", raw: "[10.0.0.1]:80"},
		{name: "unclosed bracket", raw: "[2001:db8::1"},
		{name: "257 entries", raw: strings.Repeat("10.0.0.1,", 256) + "10.0.0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tg, err := ParseTargets(tt.raw); err == nil {
				t.Errorf("ParseTargets accepted %q (%v), want error", tt.raw, tg)
			}
		})
	}
}

func TestTargetsAllowAddrPort(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		addr string
		port uint16
		want bool
	}{
		{"single port match", "10.0.0.5:443", "10.0.0.5", 443, true},
		{"single port other port", "10.0.0.5:443", "10.0.0.5", 80, false},
		{"single port other address", "10.0.0.5:443", "10.0.0.6", 443, false},
		{"range lower bound", "10.0.0.0/8:440-445", "10.1.2.3", 440, true},
		{"range upper bound", "10.0.0.0/8:440-445", "10.1.2.3", 445, true},
		{"range inside", "10.0.0.0/8:440-445", "10.1.2.3", 442, true},
		{"range below", "10.0.0.0/8:440-445", "10.1.2.3", 439, false},
		{"range above", "10.0.0.0/8:440-445", "10.1.2.3", 446, false},
		{"no port allows low port", "10.0.0.5", "10.0.0.5", 1, true},
		{"no port allows high port", "10.0.0.5", "10.0.0.5", 65535, true},
		{"IPv4-mapped input", "10.0.0.0/8", "::ffff:10.0.0.5", 443, true},
		{"zoned input like unzoned", "fe80::1", "fe80::1%eth0", 22, true},
		{"IPv6 explicit port", "[2001:db8::/48]:443", "2001:db8::5", 443, true},
		{"IPv6 explicit port other port", "[2001:db8::/48]:443", "2001:db8::5", 80, false},
		{"explicit loopback", "127.0.0.1:8080", "127.0.0.1", 8080, true},
		{"explicit IPv6 loopback", "[::1]:8080", "::1", 8080, true},
		{"explicit metadata address", "169.254.169.254:80", "169.254.169.254", 80, true},
		{"explicit loopback net", "127.0.0.0/8", "127.0.0.1", 80, true},
		{"/7 does not open loopback", "126.0.0.0/7", "127.0.0.1", 80, false},
		{"/7 opens the non-blocked half", "126.0.0.0/7", "126.1.1.1", 80, true},
		{"0.0.0.0/0 does not open loopback", "0.0.0.0/0", "127.0.0.1", 80, false},
		{"0.0.0.0/0 does not open metadata", "0.0.0.0/0", "169.254.169.254", 80, false},
		{"0.0.0.0/0 allows public", "0.0.0.0/0", "8.8.8.8", 443, true},
		{"::/0 does not open loopback", "::/0", "::1", 443, false},
		{"::/0 does not open link-local", "::/0", "fe80::1", 443, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tg := mustTargets(t, tt.raw)
			ap := netip.AddrPortFrom(netip.MustParseAddr(tt.addr), tt.port)
			if got := tg.AllowAddrPort(ap); got != tt.want {
				t.Errorf("AllowAddrPort(%s) with %q = %v, want %v", ap, tt.raw, got, tt.want)
			}
		})
	}
}

func TestTargetsAllowIP(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		addr string
		want bool
	}{
		{"entry with port allows the address", "10.0.0.5:443", "10.0.0.5", true},
		{"entry with port other address", "10.0.0.5:443", "10.0.0.6", false},
		{"range with port allows any port check", "10.0.0.0/8:443", "10.9.9.9", true},
		{"IPv6 entry with port", "[2001:db8::/48]:443", "2001:db8::1", true},
		{"any allows public", "any", "8.8.8.8", true},
		{"any blocks loopback", "any", "127.0.0.1", false},
		{"explicit loopback", "127.0.0.1", "127.0.0.1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tg := mustTargets(t, tt.raw)
			if got := tg.AllowIP(netip.MustParseAddr(tt.addr)); got != tt.want {
				t.Errorf("AllowIP(%s) with %q = %v, want %v", tt.addr, tt.raw, got, tt.want)
			}
		})
	}
}

func TestTargetsNil(t *testing.T) {
	var tg *Targets
	if tg.Any() {
		t.Error("nil Targets reports Any")
	}
	if tg.AllowIP(netip.MustParseAddr("8.8.8.8")) {
		t.Error("nil Targets allows an IP")
	}
	if tg.AllowAddrPort(netip.MustParseAddrPort("8.8.8.8:443")) {
		t.Error("nil Targets allows an address and port")
	}
}

func TestTargetsAnyBlockedRanges(t *testing.T) {
	tg := mustTargets(t, "any")
	if !tg.Any() {
		t.Fatal("any: Any() = false")
	}
	allowed := []string{"8.8.8.8", "10.1.2.3"}
	blocked := []string{
		"127.0.0.1", "::1", "169.254.169.254", "fe80::1", "224.0.0.1",
		"0.0.0.0", "::", "255.255.255.255", "ff02::1",
	}
	for _, s := range allowed {
		if !tg.AllowAddrPort(netip.AddrPortFrom(netip.MustParseAddr(s), 443)) {
			t.Errorf("any: %s blocked, want allowed", s)
		}
	}
	for _, s := range blocked {
		if tg.AllowAddrPort(netip.AddrPortFrom(netip.MustParseAddr(s), 443)) {
			t.Errorf("any: %s allowed, want blocked", s)
		}
		if tg.AllowIP(netip.MustParseAddr(s)) {
			t.Errorf("any: AllowIP(%s) = true, want false", s)
		}
	}
}

func TestTargetsString(t *testing.T) {
	tests := map[string]string{
		"10.0.0.5":            "10.0.0.5",
		"10.0.0.0/8:443":      "10.0.0.0/8:443",
		"[2001:db8::/48]:443": "[2001:db8::/48]:443",
		"[::1]:8080":          "[::1]:8080",
		"10.0.0.0/8:443-445":  "10.0.0.0/8:443-445",
		"2001:db8::/48":       "2001:db8::/48",
	}
	for raw, want := range tests {
		if got := mustTargets(t, raw).String(); got != want {
			t.Errorf("String() of %q = %q, want %q", raw, got, want)
		}
	}
}

func TestParseMethods(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "lower case normalized", raw: "get,head", want: "GET,HEAD"},
		{name: "duplicates removed", raw: "GET,get, GET ,HEAD", want: "GET,HEAD"},
		{name: "any", raw: "any", want: "any"},
		{name: "unknown method", raw: "TRACE", wantErr: true},
		{name: "any with method", raw: "GET,any", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := ParseMethods(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseMethods(%q) accepted, want error", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseMethods(%q): %v", tt.raw, err)
			}
			if got := m.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}

	if m, err := ParseMethods(""); err != nil || m != nil {
		t.Errorf(`ParseMethods("") = %v, %v; want nil, nil`, m, err)
	}
	var nilMethods *Methods
	if nilMethods.Allows("GET") {
		t.Error("nil Methods allows GET")
	}

	m, _ := ParseMethods("GET,HEAD")
	if !m.Allows("get") || !m.Allows("HEAD") || m.Allows("POST") {
		t.Error(`"GET,HEAD" allows the wrong methods`)
	}
	anyMethods, _ := ParseMethods("any")
	if !anyMethods.Allows("TRACE") {
		t.Error(`"any" does not allow TRACE`)
	}
}

func TestParseTypes(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{name: "valid list", raw: "http,dns,tcp", want: []string{"http", "dns", "tcp"}},
		{name: "duplicates removed", raw: "http, http,dns", want: []string{"http", "dns"}},
		{name: "underscore and digits", raw: "custom_type2", want: []string{"custom_type2"}},
		{name: "invalid character", raw: "HTTP!", wantErr: true},
		{name: "empty entry", raw: "http,,dns", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseTypes(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseTypes(%q) accepted, want error", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTypes(%q): %v", tt.raw, err)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("ParseTypes(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}

	if got, err := ParseTypes(""); err != nil || got != nil {
		t.Errorf(`ParseTypes("") = %v, %v; want nil, nil`, got, err)
	}
}
