// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package checker

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"
)

func TestValidateHost(t *testing.T) {
	tests := []struct {
		host string
		// wantOff and wantOn: "" accepted, "noncanon", "blocked", "error"
		wantOff, wantOn string
	}{
		{"example.com", "", ""},
		{"example.com.", "", ""},
		{"localhost", "", ""}, // names are decided at dial time
		{"metadata.google.internal", "", ""},
		{"8.8.8.8", "", ""},
		{"1.1.1.1", "", ""},
		{"[2606:4700::1111]", "", ""},
		{"2606:4700::1111", "", ""},
		{"0x1.example.com", "", ""}, // only the last label counts
		{"123.example.com", "", ""},
		{"a.0xg", "", ""}, // not hex, so a name

		{"127.0.0.1", "", "blocked"},
		{"169.254.169.254", "", "blocked"},
		{"10.0.0.1", "", "blocked"},
		{"0.0.0.0", "", "blocked"},
		{"[::1]", "", "blocked"},
		{"::ffff:127.0.0.1", "", "blocked"},
		{"::ffff:169.254.169.254", "", "blocked"},
		{"64:ff9b::7f00:1", "", "blocked"},
		{"fd00:ec2::254", "", "blocked"},

		{"2130706433", "noncanon", "noncanon"},
		{"0x7f000001", "noncanon", "noncanon"},
		{"0x7f.1", "noncanon", "noncanon"},
		{"0177.0.0.1", "noncanon", "noncanon"},
		{"0177.0.0.01", "noncanon", "noncanon"},
		{"127.1", "noncanon", "noncanon"},
		{"127.0.1", "noncanon", "noncanon"},
		{"127.0.0.1.", "noncanon", "noncanon"},
		{"08.8.8.8", "noncanon", "noncanon"},
		{"8.8.8.08", "noncanon", "noncanon"},
		{"0X7F.0.0.1", "noncanon", "noncanon"},
		{"256.1.1.1", "noncanon", "noncanon"},
		{"1.2.3.4.5", "noncanon", "noncanon"},
		{"example.0x", "noncanon", "noncanon"},
		{"example.123", "noncanon", "noncanon"},
		{"１２７.０.０.１", "noncanon", "noncanon"}, // full-width digits, IDNA maps them to 127.0.0.1
		{"８.８.８.８", "noncanon", "noncanon"},

		{"[fe80::1%eth0]", "error", "error"},
		{"fe80::1%25eth0", "error", "error"},
		{"::1%lo", "error", "error"},
		{"[::1", "error", "error"},
		{"1.2.3.4:80", "error", "error"},
		{"0.0.0.0]", "error", "error"},
		{"exa mple.com", "error", "error"},
		{"example.com/path", "error", "error"},
		{"user@example.com", "error", "error"},
		{"_dmarc.example.com", "", ""},
		{"bücher.example", "", ""},
		{"", "error", "error"},
	}
	verdict := func(err error) string {
		switch {
		case err == nil:
			return ""
		case errors.Is(err, ErrNonCanonicalIP):
			return "noncanon"
		case errors.Is(err, ErrSSRFBlocked):
			return "blocked"
		default:
			return "error"
		}
	}
	for _, tt := range tests {
		if got := verdict(ValidateHost(tt.host, nil)); got != tt.wantOff {
			t.Errorf("ValidateHost(%q, off) = %q, want %q", tt.host, got, tt.wantOff)
		}
		if got := verdict(ValidateHost(tt.host, BlockPrivate{})); got != tt.wantOn {
			t.Errorf("ValidateHost(%q, on) = %q, want %q", tt.host, got, tt.wantOn)
		}
	}
}

// inetAton is a reference for the classic BSD parser: one to four parts,
// each decimal, octal (leading 0) or hex (0x), the last part filling the
// remaining bytes. Anything it accepts is a notation some resolver reads as
// an address.
func inetAton(s string) (netip.Addr, bool) {
	parts := strings.Split(s, ".")
	if len(parts) == 0 || len(parts) > 4 {
		return netip.Addr{}, false
	}
	vals := make([]uint64, len(parts))
	for i, p := range parts {
		base := 10
		switch {
		case len(p) > 1 && (p[:2] == "0x" || p[:2] == "0X"):
			base, p = 16, p[2:]
			if p == "" {
				p = "0"
			}
		case len(p) > 1 && p[0] == '0':
			base, p = 8, p[1:]
		}
		v, err := strconv.ParseUint(p, base, 32)
		if err != nil {
			return netip.Addr{}, false
		}
		vals[i] = v
	}
	var n uint64
	for i, v := range vals[:len(vals)-1] {
		if v > 255 {
			return netip.Addr{}, false
		}
		n |= v << (8 * (3 - i))
	}
	last := vals[len(vals)-1]
	if last >= 1<<(8*(4-len(vals)+1)) {
		return netip.Addr{}, false
	}
	n |= last
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(n)) //nolint:gosec // G115: n < 2^32 by construction
	return netip.AddrFrom4(b), true
}

// FuzzValidateHost checks the save-time rules against the reference parser:
// a host that inet_aton reads as an address is accepted only in canonical
// form, and a blocked address never passes with the policy (PF-29a).
func FuzzValidateHost(f *testing.F) {
	for _, s := range []string{"127.0.0.1", "0x7f.1", "2130706433", "0177.0.0.1", "127.1", "8.8.8.8",
		"example.com", "::1", "[fe80::1%eth0]", "1.2.3.4.", "0x", "１２７.０.０.１", "::ffff:a9fe:a9fe"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, host string) {
		errOff := ValidateHost(host, nil)
		errOn := ValidateHost(host, BlockPrivate{})
		if errOff != nil && errOn == nil {
			t.Fatalf("%q: rejected without policy (%v) but accepted with it", host, errOff)
		}
		if ip, ok := inetAton(host); ok && errOff == nil && ip.String() != host {
			t.Fatalf("%q: inet_aton reads %s, but the notation was accepted", host, ip)
		}
		// Whatever is accepted as an IP literal must get the policy verdict
		// of its normalized form.
		h := host
		if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
			h = h[1 : len(h)-1]
		}
		if ip, err := netip.ParseAddr(h); err == nil && errOn == nil {
			if BlockPrivate.Check(BlockPrivate{}, normalize(ip)) != nil {
				t.Fatalf("%q: blocked address accepted with policy", host)
			}
		}
		// net.ParseIP accepts what netip rejects in a few corner cases; such
		// a host must not slip through as a name when it is an address.
		if ip := net.ParseIP(h); ip != nil && errOn == nil {
			if a, ok := netip.AddrFromSlice(ip); ok && (BlockPrivate{}).Check(normalize(a)) != nil {
				t.Fatalf("%q: net.ParseIP reads blocked %s, accepted with policy", host, ip)
			}
		}
	})
}

type targetChecker struct {
	typ     string
	targets []Target
}

func (c targetChecker) Type() string                   { return c.typ }
func (c targetChecker) Name() string                   { return c.typ }
func (c targetChecker) ConfigSchema() json.RawMessage  { return json.RawMessage(`{"type":"object"}`) }
func (c targetChecker) Validate(json.RawMessage) error { return nil }
func (c targetChecker) Check(context.Context, *Env, json.RawMessage) Result {
	return Result{Status: StatusUp}
}

func (c targetChecker) Targets(json.RawMessage) ([]Target, error) { return c.targets, nil }

func TestRegistryValidateTargets(t *testing.T) {
	r := NewRegistry()
	r.Register(targetChecker{typ: "t", targets: []Target{
		{Field: "/url", Host: "169.254.169.254"},
		{Field: "/host", Host: "example.com"},
	}})
	r.Register(targetChecker{typ: "n", targets: []Target{{Field: "/host", Host: "0x7f.1"}}})

	if err := r.Validate("t", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Validate without policy: %v", err)
	}
	var verr *ValidationError
	err := r.ValidateTargets("t", json.RawMessage(`{}`), BlockPrivate{})
	if !errors.As(err, &verr) || len(verr.Fields) != 1 || verr.Fields[0].Field != "/url" || !errors.Is(ValidateHost("169.254.169.254", BlockPrivate{}), ErrSSRFBlocked) {
		t.Fatalf("ValidateTargets with policy = %v, want one field error at /url", err)
	}
	if err := r.Validate("n", json.RawMessage(`{}`)); !errors.As(err, &verr) || verr.Fields[0].Field != "/host" {
		t.Fatalf("Validate of non-canonical host = %v, want field error at /host", err)
	}
	if err := r.ValidateTargets("missing", nil, nil); !errors.Is(err, ErrUnknownType) {
		t.Fatalf("unknown type = %v", err)
	}
}
