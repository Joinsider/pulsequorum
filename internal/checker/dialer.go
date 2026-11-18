// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package checker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
)

// ErrSSRFBlocked is matched by errors for connections the address policy denies.
var ErrSSRFBlocked = errors.New("address blocked by SSRF protection")

// BlockedError reports a denied destination.
type BlockedError struct {
	Addr   netip.Addr
	Reason string
}

func (e *BlockedError) Error() string {
	return fmt.Sprintf("connection to %s blocked by SSRF protection (%s)", e.Addr, e.Reason)
}

// Is makes errors.Is(err, ErrSSRFBlocked) work.
func (e *BlockedError) Is(target error) bool { return target == ErrSSRFBlocked }

// AddressPolicy decides whether a destination IP may be contacted.
type AddressPolicy interface {
	// Check returns a *BlockedError for forbidden addresses.
	Check(ip netip.Addr) error
}

// Dialer is the single entry point for outgoing connections of checkers and
// notifications (FA-CHK-04). The policy is evaluated in net.Dialer.Control,
// i.e. on the IP the socket actually connects to, after DNS resolution and for
// every redirect hop; DNS rebinding cannot bypass it (FA-SEC-SSRF, PF-29a).
type Dialer struct {
	// Policy may be nil, which allows every address (SSRF protection off).
	Policy AddressPolicy
}

// DialContext connects like net.Dialer.DialContext. Keep-alive probes are
// disabled: every check uses a fresh, short-lived connection (FA-CHK-11).
func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	nd := net.Dialer{KeepAlive: -1, Control: d.control}
	return nd.DialContext(ctx, network, address)
}

// Allow checks an address for code paths that cannot use DialContext, such
// as ICMP.
func (d *Dialer) Allow(ip netip.Addr) error {
	if d == nil || d.Policy == nil {
		return nil
	}
	return d.Policy.Check(normalize(ip))
}

func (d *Dialer) control(_, address string, _ syscall.RawConn) error {
	if d.Policy == nil {
		return nil
	}
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		// Control always receives a numeric address; anything else is unexpected.
		return fmt.Errorf("%w: unparsable destination %q", ErrSSRFBlocked, address)
	}
	return d.Policy.Check(normalize(ap.Addr()))
}

// normalize unmaps IPv4-mapped IPv6 addresses and drops the IPv6 zone.
// netip.Prefix.Contains never matches a zoned address, so "fe80::1%eth0" or
// "::1%lo" would otherwise pass every prefix check.
func normalize(ip netip.Addr) netip.Addr {
	return ip.Unmap().WithZone("")
}

// BlockPrivate denies loopback, private, link-local (including cloud metadata
// at 169.254.169.254), CGNAT, multicast, reserved and the IPv6 forms that embed
// IPv4 addresses (Pflichtenheft FA-SEC-SSRF).
type BlockPrivate struct{}

var blockedPrefixes = func() []struct {
	prefix netip.Prefix
	reason string
} {
	list := []struct{ cidr, reason string }{
		{"0.0.0.0/8", "unspecified"},
		{"10.0.0.0/8", "private"},
		{"100.64.0.0/10", "carrier-grade NAT"},
		{"127.0.0.0/8", "loopback"},
		{"169.254.0.0/16", "link-local / cloud metadata"},
		{"172.16.0.0/12", "private"},
		{"192.0.0.0/24", "IETF protocol assignments"},
		{"192.168.0.0/16", "private"},
		{"198.18.0.0/15", "benchmarking"},
		{"224.0.0.0/4", "multicast"},
		{"240.0.0.0/4", "reserved / broadcast"},
		{"::/96", "IPv4-compatible / unspecified / loopback"},
		{"::ffff:0:0/96", "IPv4-mapped"},
		{"::ffff:0:0:0/96", "IPv4-translated"},
		{"64:ff9b::/96", "NAT64"},
		{"64:ff9b:1::/48", "local NAT64"},
		{"100::/64", "discard"},
		{"2001::/32", "Teredo"},
		{"2002::/16", "6to4"},
		{"fc00::/7", "unique local"},
		{"fe80::/10", "link-local"},
		{"fec0::/10", "site-local"},
		{"ff00::/8", "multicast"},
	}
	out := make([]struct {
		prefix netip.Prefix
		reason string
	}, len(list))
	for i, e := range list {
		out[i].prefix = netip.MustParsePrefix(e.cidr)
		out[i].reason = e.reason
	}
	return out
}()

// Check implements AddressPolicy.
func (BlockPrivate) Check(ip netip.Addr) error {
	if !ip.IsValid() {
		return &BlockedError{Addr: ip, Reason: "invalid address"}
	}
	ip = normalize(ip)
	for _, b := range blockedPrefixes {
		if b.prefix.Contains(ip) {
			return &BlockedError{Addr: ip, Reason: b.reason}
		}
	}
	return nil
}
