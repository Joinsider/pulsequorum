// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package checker

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"
)

// addrPortFake allows exactly one port and records what it was asked.
type addrPortFake struct {
	allowPort uint16
	gotAP     []netip.AddrPort
}

// Check is never reached for a policy that implements AddrPortPolicy.
func (f *addrPortFake) Check(netip.Addr) error {
	return nil
}

func (f *addrPortFake) CheckAddrPort(ap netip.AddrPort) error {
	f.gotAP = append(f.gotAP, ap)
	if ap.Port() == f.allowPort {
		return nil
	}
	return &BlockedError{Addr: ap.Addr(), Reason: "port not allowed by test policy"}
}

// addrOnlyFake implements only AddressPolicy and records the addresses.
type addrOnlyFake struct {
	gotIP []netip.Addr
}

func (f *addrOnlyFake) Check(ip netip.Addr) error {
	f.gotIP = append(f.gotIP, ip)
	return nil
}

// closedPort returns a loopback port with nothing listening on it.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestDialerUsesAddrPortPolicy(t *testing.T) {
	open := listen(t)
	_, openPort, _ := net.SplitHostPort(open)
	allowed64, err := strconv.ParseUint(openPort, 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	allowed := uint16(allowed64)
	closed := closedPort(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	policy := &addrPortFake{allowPort: allowed}
	d := &Dialer{Policy: policy}

	conn, err := d.DialContext(ctx, "tcp", open)
	if err != nil {
		t.Fatalf("dial allowed port %s: %v", open, err)
	}
	_ = conn.Close()

	_, err = d.DialContext(ctx, "tcp", closed)
	if !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("dial closed port %s = %v, want ErrSSRFBlocked (policy decides before connect)", closed, err)
	}

	if len(policy.gotAP) < 2 {
		t.Fatalf("CheckAddrPort calls = %v, want at least 2", policy.gotAP)
	}
	if got := policy.gotAP[0].Port(); got != allowed {
		t.Errorf("first CheckAddrPort port = %d, want %d", got, allowed)
	}
	if got := policy.gotAP[1].Port(); got == allowed {
		t.Errorf("second CheckAddrPort port = %d, want the closed port", got)
	}
}

func TestDialerWithoutAddrPortPolicyUsesCheck(t *testing.T) {
	addr := listen(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	policy := &addrOnlyFake{}
	d := &Dialer{Policy: policy}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close()

	want := netip.MustParseAddr("127.0.0.1")
	if len(policy.gotIP) == 0 {
		t.Fatal("Check was not called")
	}
	for _, ip := range policy.gotIP {
		if ip != want {
			t.Errorf("Check called with %s, want %s", ip, want)
		}
	}
}
