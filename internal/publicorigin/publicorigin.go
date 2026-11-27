// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package publicorigin checks the public URLs of voters (ADR 0036). The rule
// steers bearer tokens of the SPA and ends up in CORS and CSP headers; it is
// security code and the only implementation of the rule in Go. The package has
// no dependencies besides the standard library, so small binaries can use it.
package publicorigin

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// MaxLen is the longest public node URL the registry accepts, in
// bytes, before normalization (ADR 0036).
const MaxLen = 256

// errInvalidPublicURL is wrapped by every rejection of NormalizePublicOrigin.
var errInvalidPublicURL = errors.New("invalid public node URL")

// Normalize checks the public URL of a voter and returns its
// canonical origin (ADR 0036). The origin steers bearer tokens of the SPA and
// ends up in CORS and CSP headers, so the rule is strict:
//
//   - absolute URL with scheme https, or http only for localhost, 127.0.0.1
//     and [::1];
//   - no userinfo, path (a single "/" is allowed), query or fragment, not even
//     an empty "?" or "#";
//   - host is an ASCII DNS name of [a-z0-9.-] or an IP literal without zone;
//   - the port, if any, is 1 to 65535; the default port of the scheme is
//     dropped, as browsers do in the Origin header;
//   - at most MaxLen bytes, no whitespace or control characters.
//
// The result is lower case: scheme://host[:port], an IPv6 host in brackets.
// The function is deterministic and safe to call in the Raft FSM.
func Normalize(raw string) (string, error) {
	if raw == "" || len(raw) > MaxLen {
		return "", invalidPublicURL("must be 1 to %d bytes", MaxLen)
	}
	for i := 0; i < len(raw); i++ {
		if c := raw[i]; c <= ' ' || c >= 0x7f {
			return "", invalidPublicURL("must be printable ASCII without spaces")
		}
	}
	if strings.ContainsAny(raw, "?#\\") {
		return "", invalidPublicURL("must not have a query or fragment")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", invalidPublicURL("cannot be parsed")
	}
	if u.Opaque != "" || u.User != nil || u.Host == "" {
		return "", invalidPublicURL("must be an absolute URL without userinfo")
	}
	if u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return "", invalidPublicURL("must not have a path")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", invalidPublicURL("scheme must be https")
	}

	host, port, err := splitPublicHost(u.Host)
	if err != nil {
		return "", err
	}
	if scheme == "http" && !isLoopbackHost(host) {
		return "", invalidPublicURL("http is only allowed for localhost, 127.0.0.1 and [::1]")
	}
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}

	hostPart := host
	if strings.Contains(host, ":") {
		hostPart = "[" + host + "]"
	}
	if port != "" {
		hostPart += ":" + port
	}
	return scheme + "://" + hostPart, nil
}

// splitPublicHost validates the host[:port] of a public URL and returns the
// lower-case host (an IPv6 address without brackets) and the port without
// leading zeros, or "" if there is none.
func splitPublicHost(hostport string) (host, port string, err error) {
	host, port = hostport, ""
	if strings.HasPrefix(hostport, "[") {
		end := strings.IndexByte(hostport, ']')
		if end < 0 {
			return "", "", invalidPublicURL("has an unterminated IPv6 literal")
		}
		host, port = hostport[1:end], hostport[end+1:]
		if port != "" {
			if !strings.HasPrefix(port, ":") {
				return "", "", invalidPublicURL("has a malformed host")
			}
			port = port[1:]
			if port == "" {
				return "", "", invalidPublicURL("has an empty port")
			}
		}
		addr, perr := netip.ParseAddr(host)
		if perr != nil || !addr.Is6() || addr.Zone() != "" || addr.Is4In6() {
			return "", "", invalidPublicURL("has an invalid IPv6 literal")
		}
		host = addr.String()
	} else {
		if i := strings.LastIndexByte(hostport, ':'); i >= 0 {
			host, port = hostport[:i], hostport[i+1:]
			if port == "" {
				return "", "", invalidPublicURL("has an empty port")
			}
		}
		host = strings.ToLower(host)
		if !validDNSName(host) {
			if _, perr := netip.ParseAddr(host); perr != nil || strings.Contains(host, ":") {
				return "", "", invalidPublicURL("host must be a DNS name or IP address")
			}
		}
	}
	if port != "" {
		n, perr := strconv.Atoi(port)
		if perr != nil || n < 1 || n > 65535 || strings.TrimLeft(port, "0123456789") != "" {
			return "", "", invalidPublicURL("port must be 1 to 65535")
		}
		port = strconv.Itoa(n)
	}
	return host, port, nil
}

// validDNSName reports whether name is an ASCII host name: labels of
// [a-z0-9-], 1 to 63 bytes, not starting or ending with "-", at most 253
// bytes in total, optionally without a trailing dot.
func validDNSName(name string) bool {
	if name == "" || len(name) > 253 || strings.HasSuffix(name, ".") {
		return false
	}
	for label := range strings.SplitSeq(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	// Browsers read a name whose last label is numeric (127.1, 0x7f000001)
	// as an IPv4 address (WHATWG URL); only the dotted-quad form is
	// accepted, as an IP literal.
	last := name[strings.LastIndexByte(name, '.')+1:]
	if strings.HasPrefix(last, "0x") || strings.Trim(last, "0123456789") == "" {
		return false
	}
	return true
}

// isLoopbackHost reports whether host is one of the loopback names for which
// http is allowed.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.IPv6loopback))
}

func invalidPublicURL(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errInvalidPublicURL, fmt.Sprintf(format, args...))
}
