// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package checker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"syscall"
	"testing"
	"time"
)

func TestBlockPrivate(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.255.255.254", "10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.1",
		"169.254.169.254", "100.64.0.1", "0.0.0.0", "224.0.0.1", "255.255.255.255", "198.18.0.1",
		"::1", "::", "::ffff:127.0.0.1", "::ffff:169.254.169.254", "::127.0.0.1",
		"64:ff9b::7f00:1", "64:ff9b::a9fe:a9fe", "2002:7f00:1::1", "2001:0:4136:e378::1",
		"fc00::1", "fd12:3456::1", "fe80::1", "ff02::1",
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "172.32.0.1", "192.169.0.1", "100.128.0.1", "2606:4700::1111", "2a00:1450::1"}

	var p BlockPrivate
	for _, s := range blocked {
		if err := p.Check(netip.MustParseAddr(s)); !errors.Is(err, ErrSSRFBlocked) {
			t.Errorf("%s: allowed, want blocked", s)
		}
	}
	for _, s := range allowed {
		if err := p.Check(netip.MustParseAddr(s)); err != nil {
			t.Errorf("%s: %v, want allowed", s, err)
		}
	}
	if err := p.Check(netip.Addr{}); err == nil {
		t.Error("invalid address allowed")
	}
}

// FuzzAllowMapped checks that an IPv4 address and its IPv4-mapped IPv6 form
// get the same verdict through Dialer.Allow (PF-29a).
func FuzzAllowMapped(f *testing.F) {
	for _, seed := range [][4]byte{{127, 0, 0, 1}, {8, 8, 8, 8}, {169, 254, 169, 254}, {10, 0, 0, 1}} {
		f.Add(seed[0], seed[1], seed[2], seed[3])
	}
	d := &Dialer{Policy: BlockPrivate{}}
	f.Fuzz(func(t *testing.T, a, b, c, e byte) {
		v4 := netip.AddrFrom4([4]byte{a, b, c, e})
		mapped := netip.AddrFrom16(v4.As16())
		if (d.Allow(v4) == nil) != (d.Allow(mapped) == nil) {
			t.Fatalf("%s and %s get different verdicts", v4, mapped)
		}
	})
}

func listen(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().String()
}

func TestDialerPolicy(t *testing.T) {
	addr := listen(t)
	_, port, _ := net.SplitHostPort(addr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	open := &Dialer{}
	conn, err := open.DialContext(ctx, "tcp", addr)
	if err != nil {
		t.Fatalf("dial without policy: %v", err)
	}
	_ = conn.Close()

	guarded := &Dialer{Policy: BlockPrivate{}}
	// "localhost" resolves at dial time; the check sees the resolved IP, which
	// is what defeats DNS rebinding.
	for _, target := range []string{addr, net.JoinHostPort("localhost", port)} {
		_, err := guarded.DialContext(ctx, "tcp", target)
		if !errors.Is(err, ErrSSRFBlocked) || Classify(err) != ClassSSRFBlocked {
			t.Errorf("dial %s = %v, want blocked", target, err)
		}
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want ErrorClass
	}{
		{"nil", nil, ClassNone},
		{"ssrf", fmt.Errorf("dial: %w", &BlockedError{Addr: netip.MustParseAddr("127.0.0.1"), Reason: "loopback"}), ClassSSRFBlocked},
		{"status", &StatusCodeError{Code: 500, Expected: "200-299"}, ClassHTTPStatus},
		{"assertion", &AssertionError{Msg: "keyword missing"}, ClassAssertion},
		{"own tls check", &TLSError{Err: errors.New("expired")}, ClassTLS},
		{"unknown authority", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, ClassTLS},
		{"hostname", x509.HostnameError{Host: "a"}, ClassTLS},
		{"tls alert", fmt.Errorf("handshake: %w", tls.AlertError(40)), ClassTLS},
		{"deadline", fmt.Errorf("get: %w", context.DeadlineExceeded), ClassTimeout},
		{"dns", &net.DNSError{Err: "no such host", Name: "x.invalid", IsNotFound: true}, ClassDNS},
		{"dns timeout", &net.DNSError{Err: "timeout", IsTimeout: true}, ClassTimeout},
		{"refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, ClassConnect},
		{"reset", fmt.Errorf("read: %w", syscall.ECONNRESET), ClassConnect},
		{"eof", fmt.Errorf("read: %w", io.ErrUnexpectedEOF), ClassConnect},
		{"other", errors.New("boom"), ClassInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.err); got != tt.want {
				t.Errorf("Classify(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}
