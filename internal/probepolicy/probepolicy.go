// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package probepolicy parses the local policy of a probe (ADR 0044): the
// targets it may connect to, the HTTP methods and monitor types it may run
// and how much of a result it reports. The operator of the probe sets the
// policy locally; the cluster can neither relax nor see its entries.
package probepolicy

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// Any disables a restriction on purpose (ADR 0044, section 1).
const Any = "any"

// maxEntries bounds the size of a list setting.
const maxEntries = 256

// alwaysBlocked are ranges that only entries lying completely inside them
// can open; Any, 0.0.0.0/0 or ::/0 never do (ADR 0044, section 1).
var alwaysBlocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),          // "this network", unspecified
	netip.MustParsePrefix("127.0.0.0/8"),        // loopback
	netip.MustParsePrefix("169.254.0.0/16"),     // link-local, cloud metadata
	netip.MustParsePrefix("224.0.0.0/4"),        // multicast
	netip.MustParsePrefix("255.255.255.255/32"), // limited broadcast
	netip.MustParsePrefix("::/128"),             // unspecified
	netip.MustParsePrefix("::1/128"),            // loopback
	netip.MustParsePrefix("fe80::/10"),          // link-local
	netip.MustParsePrefix("ff00::/8"),           // multicast
}

// Targets is a parsed PROBE_ALLOW_TARGETS value. A nil *Targets means the
// setting is unset.
type Targets struct {
	any     bool
	entries []target
}

type target struct {
	prefix  netip.Prefix
	anyPort bool
	lo, hi  uint16
}

// ParseTargets parses a comma-separated list of IP or CIDR entries, each
// optionally followed by :port or :from-to; IPv6 entries with a port use
// brackets, e.g. "10.20.0.0/16:443,[2001:db8::/48]:443,10.0.0.53". The value
// Any allows every target outside the always blocked ranges. An empty value
// returns nil.
func ParseTargets(raw string) (*Targets, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if raw == Any {
		return &Targets{any: true}, nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) > maxEntries {
		return nil, fmt.Errorf("at most %d entries are allowed", maxEntries)
	}
	t := &Targets{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		e, err := parseTarget(p)
		if err != nil {
			return nil, fmt.Errorf("entry %q: %w", p, err)
		}
		t.entries = append(t.entries, e)
	}
	return t, nil
}

func parseTarget(s string) (target, error) {
	switch s {
	case "":
		return target{}, errors.New("empty entry")
	case Any:
		return target{}, errors.New(`"any" must be the only value`)
	}
	host, ports, hasPort := s, "", false
	switch {
	case strings.HasPrefix(s, "["):
		end := strings.Index(s, "]")
		if end < 0 {
			return target{}, errors.New("missing ]")
		}
		host, ports = s[1:end], s[end+1:]
		if ports != "" {
			if !strings.HasPrefix(ports, ":") {
				return target{}, errors.New("expected :port after ]")
			}
			ports, hasPort = ports[1:], true
		}
		if !strings.Contains(host, ":") {
			return target{}, errors.New("brackets are only for IPv6")
		}
	case strings.Count(s, ":") == 1:
		i := strings.LastIndex(s, ":")
		host, ports, hasPort = s[:i], s[i+1:], true
	}
	prefix, err := parsePrefix(host)
	if err != nil {
		return target{}, err
	}
	e := target{prefix: prefix, anyPort: !hasPort}
	if hasPort {
		if e.lo, e.hi, err = parsePorts(ports); err != nil {
			return target{}, err
		}
	}
	return e, nil
}

func parsePrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "%") {
		return netip.Prefix{}, errors.New("IPv6 zones are not allowed")
	}
	var prefix netip.Prefix
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, errors.New("not an IP address or CIDR (hostnames are not allowed)")
		}
		if p != p.Masked() {
			return netip.Prefix{}, fmt.Errorf("host bits are set, use %s", p.Masked())
		}
		prefix = p
	} else {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return netip.Prefix{}, errors.New("not an IP address or CIDR (hostnames are not allowed)")
		}
		prefix = netip.PrefixFrom(a, a.BitLen())
	}
	if prefix.Addr().Is4In6() {
		return netip.Prefix{}, errors.New("IPv4-mapped IPv6 is not allowed, use the IPv4 form")
	}
	return prefix, nil
}

func parsePorts(s string) (lo, hi uint16, err error) {
	from, to, isRange := strings.Cut(s, "-")
	if lo, err = parsePort(from); err != nil {
		return 0, 0, err
	}
	hi = lo
	if isRange {
		if hi, err = parsePort(to); err != nil {
			return 0, 0, err
		}
		if hi < lo {
			return 0, 0, fmt.Errorf("port range %d-%d is reversed", lo, hi)
		}
	}
	return lo, hi, nil
}

func parsePort(s string) (uint16, error) {
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil || n == 0 || strconv.FormatUint(n, 10) != s {
		return 0, fmt.Errorf("port %q must be a number from 1 to 65535", s)
	}
	return uint16(n), nil
}

// Any reports whether the target restriction is switched off with Any.
func (t *Targets) Any() bool { return t != nil && t.any }

// AllowIP reports whether ip may be contacted on some port. It serves checks
// without a port: ICMP and DNS answers (ADR 0044, section 1).
func (t *Targets) AllowIP(ip netip.Addr) bool {
	return t.allow(ip, 0, false)
}

// AllowAddrPort reports whether a TCP or UDP connection to ap is allowed.
func (t *Targets) AllowAddrPort(ap netip.AddrPort) bool {
	return t.allow(ap.Addr(), ap.Port(), true)
}

func (t *Targets) allow(ip netip.Addr, port uint16, withPort bool) bool {
	if t == nil {
		return false
	}
	ip = ip.Unmap().WithZone("")
	blocked, inBlocked := blockedRange(ip)
	if t.any {
		return !inBlocked
	}
	for _, e := range t.entries {
		if !e.prefix.Contains(ip) {
			continue
		}
		if inBlocked && !insideRange(e.prefix, blocked) {
			continue
		}
		if !withPort || e.anyPort || (port >= e.lo && port <= e.hi) {
			return true
		}
	}
	return false
}

// insideRange reports whether p lies completely inside r.
func insideRange(p, r netip.Prefix) bool {
	return p.Bits() >= r.Bits() && r.Contains(p.Addr())
}

func blockedRange(ip netip.Addr) (netip.Prefix, bool) {
	for _, p := range alwaysBlocked {
		if p.Contains(ip) {
			return p, true
		}
	}
	return netip.Prefix{}, false
}

// String lists the entries for the local startup log.
func (t *Targets) String() string {
	switch {
	case t == nil:
		return ""
	case t.any:
		return Any
	}
	out := make([]string, 0, len(t.entries))
	for _, e := range t.entries {
		host := e.prefix.String()
		if e.prefix.IsSingleIP() {
			host = e.prefix.Addr().String()
		}
		switch {
		case e.anyPort:
			out = append(out, host)
		case e.prefix.Addr().Is6():
			out = append(out, "["+host+"]:"+portString(e.lo, e.hi))
		default:
			out = append(out, host+":"+portString(e.lo, e.hi))
		}
	}
	return strings.Join(out, ",")
}

func portString(lo, hi uint16) string {
	if lo == hi {
		return strconv.Itoa(int(lo))
	}
	return strconv.Itoa(int(lo)) + "-" + strconv.Itoa(int(hi))
}

// httpMethods are the methods the HTTP checker supports.
var httpMethods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}

// DefaultPrivateMethods are the HTTP methods of a private probe without
// PROBE_ALLOW_HTTP_METHODS (ADR 0044, section 2).
const DefaultPrivateMethods = "GET,HEAD"

// Methods is a parsed PROBE_ALLOW_HTTP_METHODS value.
type Methods struct {
	any bool
	set []string
}

// ParseMethods parses a comma-separated list of HTTP methods or Any. An
// empty value returns nil.
func ParseMethods(raw string) (*Methods, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if raw == Any {
		return &Methods{any: true}, nil
	}
	m := &Methods{}
	for _, p := range strings.Split(raw, ",") {
		p = strings.ToUpper(strings.TrimSpace(p))
		if !slices.Contains(httpMethods, p) {
			return nil, fmt.Errorf("method %q must be one of %s or %q alone", p, strings.Join(httpMethods, ", "), Any)
		}
		if !slices.Contains(m.set, p) {
			m.set = append(m.set, p)
		}
	}
	return m, nil
}

// Allows reports whether method may be used. A nil *Methods allows nothing.
func (m *Methods) Allows(method string) bool {
	return m != nil && (m.any || slices.Contains(m.set, strings.ToUpper(method)))
}

// String lists the methods for the local startup log.
func (m *Methods) String() string {
	switch {
	case m == nil:
		return ""
	case m.any:
		return Any
	}
	return strings.Join(m.set, ",")
}

// ParseTypes parses a comma-separated list of monitor types. An empty value
// returns nil, which allows every registered type. Whether a type exists is
// checked against the checker registry at startup.
func ParseTypes(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p == "" || strings.Trim(p, "abcdefghijklmnopqrstuvwxyz0123456789_-") != "" {
			return nil, fmt.Errorf("type %q must be a non-empty monitor type such as http or dns", p)
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out, nil
}

// Result detail levels (ADR 0044, section 3).
const (
	DetailFull    = "full"
	DetailMinimal = "minimal"
)
