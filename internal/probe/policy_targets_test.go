// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package probe

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/probepolicy"
)

// TestPolicyWithTargets checks that the local target policy of ADR 0044
// narrows the SSRF policy: a connection must pass both.
func TestPolicyWithTargets(t *testing.T) {
	targets, err := probepolicy.ParseTargets("10.0.0.0/8:443")
	if err != nil {
		t.Fatal(err)
	}
	ap := netip.MustParseAddrPort
	addr := netip.MustParseAddr

	t.Run("private probe without SSRF protection", func(t *testing.T) {
		p := NewPolicy(false, true).WithTargets(targets)
		tests := []struct {
			name    string
			check   func() error
			wantErr bool
		}{
			{"addr-port inside target", func() error { return p.CheckAddrPort(ap("10.1.1.1:443")) }, false},
			{"addr-port wrong port", func() error { return p.CheckAddrPort(ap("10.1.1.1:80")) }, true},
			{"addr-port outside network", func() error { return p.CheckAddrPort(ap("192.168.1.1:443")) }, true},
			{"portless check inside network", func() error { return p.Check(addr("10.1.1.1")) }, false},
			{"portless check outside network", func() error { return p.Check(addr("192.168.1.1")) }, true},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				err := tt.check()
				if (err != nil) != tt.wantErr {
					t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
				}
				if err != nil && !errors.Is(err, checker.ErrSSRFBlocked) {
					t.Errorf("err = %v, want ErrSSRFBlocked", err)
				}
			})
		}
	})

	t.Run("targets narrow SSRF protection", func(t *testing.T) {
		p := NewPolicy(true, true).WithTargets(targets)
		err := p.CheckAddrPort(ap("10.1.1.1:443"))
		if !errors.Is(err, checker.ErrSSRFBlocked) {
			t.Errorf("CheckAddrPort(10.1.1.1:443) with SSRF on = %v, want blocked", err)
		}
	})

	t.Run("public probe with remote SSRF and targets", func(t *testing.T) {
		p := NewPolicy(false, false).WithTargets(targets)
		p.SetRemote(true)
		if err := p.CheckAddrPort(ap("10.1.1.1:443")); !errors.Is(err, checker.ErrSSRFBlocked) {
			t.Errorf("CheckAddrPort(10.1.1.1:443) with remote SSRF = %v, want blocked", err)
		}
	})

	t.Run("without targets behaves as before", func(t *testing.T) {
		tests := []struct {
			name    string
			p       *Policy
			addr    string
			wantErr bool
		}{
			{"private probe, SSRF off, private address", NewPolicy(false, true), "10.1.1.1", false},
			{"private probe, SSRF off, public address", NewPolicy(false, true), "8.8.8.8", false},
			{"private probe, SSRF on, private address", NewPolicy(true, true), "10.1.1.1", true},
			{"public probe, SSRF on, private address", NewPolicy(true, false), "10.1.1.1", true},
			{"public probe, SSRF off, private address", NewPolicy(false, false), "10.1.1.1", false},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				a := addr(tt.addr)
				if err := tt.p.CheckAddrPort(netip.AddrPortFrom(a, 443)); (err != nil) != tt.wantErr {
					t.Errorf("CheckAddrPort = %v, wantErr %v", err, tt.wantErr)
				}
				if err := tt.p.Check(a); (err != nil) != tt.wantErr {
					t.Errorf("Check = %v, wantErr %v", err, tt.wantErr)
				}
			})
		}
	})
}
