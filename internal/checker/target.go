// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package checker

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"golang.org/x/net/idna"
)

// Target is one destination host named in a monitor configuration.
type Target struct {
	// Field is the JSON pointer of the configuration field, e.g. "/url".
	Field string
	// Host is the host name or IP literal (IPv6 with or without brackets).
	Host string
}

// TargetLister is implemented by checkers whose configuration names hosts
// that the check connects to. The registry validates them with ValidateHost
// (ADR 0030). A query name that is only looked up, never contacted, is not a
// target.
type TargetLister interface {
	// Targets returns the destination hosts of cfg. cfg has passed the
	// checker's schema and Validate.
	Targets(cfg json.RawMessage) ([]Target, error)
}

// ErrNonCanonicalIP is matched by ValidateHost errors for IP addresses in an
// ambiguous notation.
var ErrNonCanonicalIP = errors.New("non-canonical IP address notation")

// ValidateHost checks a destination host before it is saved (ADR 0030,
// FA-SEC-SSRF, PF-29a). It is the save-time companion of the dial-time check
// in Dialer and never resolves names: a name can change its addresses at any
// time, so only the dial-time check decides for names.
//
// Always rejected, whatever the policy:
//   - IPv6 zones ("fe80::1%eth0"): a tenant has no business naming an
//     interface of the node.
//   - Hosts that look like an IPv4 address but are not one in canonical
//     dotted-decimal form ("0x7f.1", "2130706433", "0177.0.0.1", "127.1",
//     "127.0.0.1."). Resolvers disagree on these: the Go resolver looks them
//     up as names, inet_aton reads them as addresses. A host "looks like
//     IPv4" when its last label is a number, the WHATWG URL rule browsers use;
//     no real top-level domain is numeric. The check runs on the IDNA form
//     too, because net/http maps full-width digits to ASCII.
//
// With a policy, IP literals are also checked against it, so a blocked
// address such as 169.254.169.254 is rejected when the monitor or channel is
// saved instead of failing every check.
func ValidateHost(host string, policy AddressPolicy) error {
	if host == "" {
		return errors.New("host is empty")
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	if strings.ContainsRune(host, '%') {
		return errors.New("IPv6 zones are not allowed")
	}
	if strings.ContainsRune(host, ':') {
		ip, err := netip.ParseAddr(host)
		if err != nil || !ip.Is6() {
			return fmt.Errorf("invalid IPv6 address %q", host)
		}
		return checkPolicy(ip, policy)
	}

	candidates := []string{host}
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil {
		ascii = host // e.g. underscores, which DNS allows but IDNA does not
	}
	if !validHostChars(ascii) {
		return fmt.Errorf("invalid host %q", host)
	}
	if ascii != host {
		candidates = append(candidates, ascii)
	}
	for _, h := range candidates {
		if !endsInNumber(h) {
			continue
		}
		// The IDNA form only differs when the tenant wrote something else
		// (full-width digits or dots); that is never the canonical notation.
		ip, err := netip.ParseAddr(h)
		if err != nil || !ip.Is4() || ip.String() != host {
			return fmt.Errorf("%w: %q; write IPv4 addresses as four decimal numbers without leading zeros", ErrNonCanonicalIP, host)
		}
		return checkPolicy(ip, policy)
	}
	return nil
}

// validHostChars reports whether host has only the characters of a DNS
// name or IPv4 address: letters, digits, '-', '_' and '.'.
func validHostChars(host string) bool {
	for i := 0; i < len(host); i++ {
		c := host[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

func checkPolicy(ip netip.Addr, policy AddressPolicy) error {
	if policy == nil {
		return nil
	}
	return policy.Check(normalize(ip))
}

// endsInNumber reports whether the last label of host (ignoring one trailing
// dot) is a number in the sense of the WHATWG URL standard: decimal digits,
// or "0x" followed by hex digits, which includes a bare "0x".
func endsInNumber(host string) bool {
	h := strings.TrimSuffix(host, ".")
	label := h[strings.LastIndexByte(h, '.')+1:]
	if label == "" {
		return false
	}
	if len(label) >= 2 && label[0] == '0' && (label[1] == 'x' || label[1] == 'X') {
		return strings.Trim(label[2:], "0123456789abcdefABCDEF") == ""
	}
	return strings.Trim(label, "0123456789") == ""
}

// ValidateTargets checks the destination hosts of a monitor configuration
// with ValidateHost. Validate already does this without a policy; the API
// calls ValidateTargets with the node's policy when SSRF protection is on.
// Checkers without TargetLister have no targets.
func (r *Registry) ValidateTargets(typ string, cfg json.RawMessage, policy AddressPolicy) error {
	r.mu.RLock()
	e, ok := r.checkers[typ]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownType, typ)
	}
	return validateTargets(e.checker, cfg, policy)
}

func validateTargets(c Checker, cfg json.RawMessage, policy AddressPolicy) error {
	tl, ok := c.(TargetLister)
	if !ok {
		return nil
	}
	targets, err := tl.Targets(cfg)
	if err != nil {
		return &ValidationError{Fields: []FieldError{{Message: err.Error()}}}
	}
	var fields []FieldError
	for _, t := range targets {
		if err := ValidateHost(t.Host, policy); err != nil {
			fields = append(fields, FieldError{Field: t.Field, Message: err.Error()})
		}
	}
	if len(fields) > 0 {
		return &ValidationError{Fields: fields}
	}
	return nil
}
