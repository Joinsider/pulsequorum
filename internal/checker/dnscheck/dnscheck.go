// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package dnscheck implements the DNS monitor type (FA-CHK-40/41).
package dnscheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/joinsider/uptimecluster/internal/checker"
)

// Type is the monitor type key.
const Type = "dns"

// Config is the DNS monitor configuration.
type Config struct {
	Hostname   string `json:"hostname"`
	RecordType string `json:"recordType,omitempty"`
	// Resolver is "ip" or "ip:port"; empty uses the node's system resolver.
	Resolver string   `json:"resolver,omitempty"`
	Expected []string `json:"expected,omitempty"`
	// MatchMode "contains" (default): every expected value is present;
	// "equals": the answer set equals the expected set (order-insensitive).
	MatchMode string `json:"matchMode,omitempty"`
	Transport string `json:"transport,omitempty"` // udp (default) | tcp
}

var recordTypes = map[string]uint16{
	"A": dns.TypeA, "AAAA": dns.TypeAAAA, "CNAME": dns.TypeCNAME, "MX": dns.TypeMX, "TXT": dns.TypeTXT,
	"NS": dns.TypeNS, "SOA": dns.TypeSOA, "SRV": dns.TypeSRV, "CAA": dns.TypeCAA,
}

const schemaJSON = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "DNS",
  "type": "object",
  "additionalProperties": false,
  "required": ["hostname"],
  "properties": {
    "hostname": {"type": "string", "minLength": 1, "maxLength": 253},
    "recordType": {"type": "string", "enum": ["A", "AAAA", "CNAME", "MX", "TXT", "NS", "SOA", "SRV", "CAA"], "default": "A"},
    "resolver": {"type": "string", "maxLength": 64, "description": "IP or IP:port; empty = system resolver"},
    "expected": {"type": "array", "items": {"type": "string", "maxLength": 1024}, "maxItems": 50},
    "matchMode": {"type": "string", "enum": ["contains", "equals"], "default": "contains"},
    "transport": {"type": "string", "enum": ["udp", "tcp"], "default": "udp"}
  }
}`

// Checker runs DNS checks.
type Checker struct {
	// SystemResolvers overrides /etc/resolv.conf (tests only).
	SystemResolvers []string
}

var (
	_ checker.Checker      = (*Checker)(nil)
	_ checker.TargetLister = (*Checker)(nil)
)

func init() { checker.Default.Register(&Checker{}) }

// Type implements checker.Checker.
func (*Checker) Type() string { return Type }

// Name implements checker.Checker.
func (*Checker) Name() string { return "DNS" }

// ConfigSchema implements checker.Checker.
func (*Checker) ConfigSchema() json.RawMessage { return json.RawMessage(schemaJSON) }

var hostnameRE = regexp.MustCompile(`^(?i)[a-z0-9_*]([a-z0-9_-]{0,62})(\.[a-z0-9_]([a-z0-9_-]{0,62}))*\.?$`)

// Validate implements checker.Checker.
func (*Checker) Validate(raw json.RawMessage) error {
	cfg, err := parse(raw)
	if err != nil {
		return err
	}
	var fields []checker.FieldError
	if !hostnameRE.MatchString(cfg.Hostname) {
		fields = append(fields, checker.FieldError{Field: "/hostname", Message: "not a valid host name"})
	}
	if cfg.Resolver != "" {
		if _, err := resolverAddr(cfg.Resolver); err != nil {
			fields = append(fields, checker.FieldError{Field: "/resolver", Message: err.Error()})
		}
	}
	if cfg.MatchMode == "equals" && len(cfg.Expected) == 0 {
		fields = append(fields, checker.FieldError{Field: "/expected", Message: "required for matchMode equals"})
	}
	if len(fields) > 0 {
		return &checker.ValidationError{Fields: fields}
	}
	return nil
}

// Targets implements checker.TargetLister. Only a configured resolver is a
// destination; the queried hostname is only looked up and is not checked
// (ADR 0030). The address is not validated here: ValidateHost does that, so
// a non-canonical notation gets its own message.
func (*Checker) Targets(raw json.RawMessage) ([]checker.Target, error) {
	cfg, err := parse(raw)
	if err != nil {
		return nil, err
	}
	if cfg.Resolver == "" {
		return nil, nil
	}
	return []checker.Target{{Field: "/resolver", Host: resolverHost(cfg.Resolver)}}, nil
}

// resolverHost returns the host part of a resolver given as "IP", "IP:port"
// or "[IPv6]:port". It does not check the address.
func resolverHost(s string) string {
	if host, _, err := net.SplitHostPort(s); err == nil {
		return host
	}
	return strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
}

func parse(raw json.RawMessage) (*Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("decode configuration: %w", err)
	}
	if c.RecordType == "" {
		c.RecordType = "A"
	}
	if c.Transport == "" {
		c.Transport = "udp"
	}
	return &c, nil
}

// resolverAddr accepts a canonical IP with optional port and returns ip:port.
func resolverAddr(s string) (string, error) {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.String(), nil
	}
	ip, err := netip.ParseAddr(s)
	if err != nil {
		return "", errors.New("resolver must be an IP address with optional port")
	}
	return netip.AddrPortFrom(ip, 53).String(), nil
}

// Check implements checker.Checker.
func (c *Checker) Check(ctx context.Context, env *checker.Env, raw json.RawMessage) checker.Result {
	start := time.Now()
	cfg, err := parse(raw)
	if err != nil {
		return checker.Result{Status: checker.StatusDown, Message: err.Error(), ErrorClass: checker.ClassInternal}
	}
	qtype, ok := recordTypes[cfg.RecordType]
	if !ok {
		return checker.Result{Status: checker.StatusDown, Message: "unsupported record type " + cfg.RecordType, ErrorClass: checker.ClassInternal}
	}

	servers, dialer, err := c.servers(cfg, env)
	if err != nil {
		return checker.Down(err, 0, nil)
	}

	msg := new(dns.Msg)
	msg.SetQuestion(dns.Fqdn(cfg.Hostname), qtype)
	msg.RecursionDesired = true
	msg.AuthenticatedData = true // ask the resolver to report DNSSEC validation (AD bit)
	msg.SetEdns0(1232, false)

	details := map[string]any{}
	var resp *dns.Msg
	// The system resolver is the operator's, often on an internal address;
	// with SSRF protection on, tenants only learn that it was used.
	hideResolver := cfg.Resolver == "" && env.Dialer != nil && env.Dialer.Policy != nil
	for _, server := range servers {
		details["resolver"] = server
		if hideResolver {
			details["resolver"] = "system"
		}
		resp, err = exchange(ctx, dialer, cfg.Transport, server, msg)
		if err == nil && resp.Truncated && cfg.Transport == "udp" {
			// Large answers are cut off over UDP; repeat over TCP (FA-CHK-41).
			details["truncatedRetry"] = true
			resp, err = exchange(ctx, dialer, "tcp", server, msg)
		}
		if err == nil {
			break
		}
	}
	latency := int(time.Since(start).Milliseconds())
	if err != nil {
		res := checker.Down(err, latency, details)
		if hideResolver {
			// Network errors name the resolver's and the node's addresses.
			res.Message = "system resolver failed (" + string(res.ErrorClass) + ")"
		}
		return res
	}

	details["rcode"] = dns.RcodeToString[resp.Rcode]
	details["authenticatedData"] = resp.AuthenticatedData
	if resp.Rcode != dns.RcodeSuccess {
		return checker.Result{Status: checker.StatusDown, LatencyMs: latency, ErrorClass: checker.ClassDNS,
			Message: fmt.Sprintf("%s for %s %s", dns.RcodeToString[resp.Rcode], cfg.RecordType, cfg.Hostname), Details: details}
	}

	// With SSRF protection on, an answer in a blocked range fails the check
	// and is not shown: otherwise a tenant could map internal names of the
	// operator through the system resolver (ADR 0030, FA-SEC-SSRF).
	if reason, blocked := blockedAnswer(env.Dialer, resp.Answer); blocked {
		delete(details, "authenticatedData")
		return checker.Result{Status: checker.StatusDown, LatencyMs: latency, ErrorClass: checker.ClassSSRFBlocked,
			Message: "answer in a blocked range: " + reason, Details: details}
	}

	answers := render(qtype, resp.Answer)
	details["answers"] = answers
	if len(answers) == 0 {
		return checker.Result{Status: checker.StatusDown, LatencyMs: latency, ErrorClass: checker.ClassDNS,
			Message: fmt.Sprintf("no %s records for %s", cfg.RecordType, cfg.Hostname), Details: details}
	}
	if err := match(qtype, cfg, answers); err != nil {
		return checker.Down(err, latency, details)
	}
	return checker.Result{Status: checker.StatusUp, LatencyMs: latency, Message: summarize(answers), Details: details}
}

// summarize lists up to five answers for the result message.
func summarize(answers []string) string {
	const shown = 5
	if len(answers) <= shown {
		return strings.Join(answers, ", ")
	}
	return fmt.Sprintf("%s (+%d more)", strings.Join(answers[:shown], ", "), len(answers)-shown)
}

// blockedAnswer reports whether an A or AAAA record of the answer section is
// denied by the dialer's address policy, and the reason without the address.
// All address records count, not only those of the queried type, because a
// CNAME query can carry the target's addresses as well.
func blockedAnswer(d *checker.Dialer, rrs []dns.RR) (string, bool) {
	for _, rr := range rrs {
		var ip net.IP
		switch r := rr.(type) {
		case *dns.A:
			ip = r.A
		case *dns.AAAA:
			ip = r.AAAA
		default:
			continue
		}
		addr, ok := netip.AddrFromSlice(ip)
		if !ok {
			return "invalid address", true
		}
		var berr *checker.BlockedError
		if err := d.Allow(addr); errors.As(err, &berr) {
			return berr.Reason, true
		} else if err != nil {
			return "address rejected", true
		}
	}
	return "", false
}

// servers returns the resolvers to ask and the dialer to use. A resolver set
// in the monitor is tenant input and goes through the SSRF policy; the system
// resolver is operator configuration (often private, e.g. 127.0.0.11 in
// Docker) and is contacted without the policy.
func (c *Checker) servers(cfg *Config, env *checker.Env) ([]string, *checker.Dialer, error) {
	if cfg.Resolver != "" {
		addr, err := resolverAddr(cfg.Resolver)
		if err != nil {
			return nil, nil, err
		}
		return []string{addr}, env.Dialer, nil
	}
	if len(c.SystemResolvers) > 0 {
		return c.SystemResolvers, &checker.Dialer{}, nil
	}
	conf, err := dns.ClientConfigFromFile("/etc/resolv.conf")
	if err != nil || len(conf.Servers) == 0 {
		return nil, nil, fmt.Errorf("no system resolver configured: %w", errors.Join(err, errors.New("empty /etc/resolv.conf")))
	}
	out := make([]string, 0, len(conf.Servers))
	for _, s := range conf.Servers {
		out = append(out, net.JoinHostPort(s, conf.Port))
	}
	return out, &checker.Dialer{}, nil
}

func exchange(ctx context.Context, dialer *checker.Dialer, network, server string, msg *dns.Msg) (*dns.Msg, error) {
	conn, err := dialer.DialContext(ctx, network, server)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	co := &dns.Conn{Conn: conn, UDPSize: 1232}
	client := &dns.Client{Net: network}
	resp, _, err := client.ExchangeWithConnContext(ctx, msg, co)
	if err != nil {
		return nil, fmt.Errorf("query %s over %s: %w", server, network, err)
	}
	return resp, nil
}

// render turns the answers of the queried type into comparable strings.
func render(qtype uint16, rrs []dns.RR) []string {
	var out []string
	for _, rr := range rrs {
		if rr.Header().Rrtype != qtype {
			continue // e.g. CNAME records in an A answer
		}
		var v string
		switch r := rr.(type) {
		case *dns.A:
			v = r.A.String()
		case *dns.AAAA:
			v = r.AAAA.String()
		case *dns.CNAME:
			v = r.Target
		case *dns.NS:
			v = r.Ns
		case *dns.MX:
			v = fmt.Sprintf("%d %s", r.Preference, r.Mx)
		case *dns.TXT:
			v = strings.Join(r.Txt, "")
		case *dns.SRV:
			v = fmt.Sprintf("%d %d %d %s", r.Priority, r.Weight, r.Port, r.Target)
		case *dns.SOA:
			v = fmt.Sprintf("%s %s %d", r.Ns, r.Mbox, r.Serial)
		case *dns.CAA:
			v = fmt.Sprintf("%d %s %s", r.Flag, r.Tag, r.Value)
		default:
			continue
		}
		out = append(out, normalize(qtype, v))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// normalize makes names case-insensitive and strips trailing dots; TXT and CAA
// values are compared exactly.
func normalize(qtype uint16, v string) string {
	v = strings.TrimSpace(v)
	switch qtype {
	case dns.TypeTXT, dns.TypeCAA:
		return v
	case dns.TypeAAAA:
		if ip, err := netip.ParseAddr(v); err == nil {
			return ip.String()
		}
	}
	fields := strings.Fields(strings.ToLower(v))
	for i, f := range fields {
		fields[i] = strings.TrimSuffix(f, ".")
	}
	return strings.Join(fields, " ")
}

func match(qtype uint16, cfg *Config, answers []string) error {
	if len(cfg.Expected) == 0 {
		return nil
	}
	expected := make([]string, 0, len(cfg.Expected))
	for _, e := range cfg.Expected {
		expected = append(expected, normalize(qtype, e))
	}
	slices.Sort(expected)
	expected = slices.Compact(expected)

	var missing []string
	for _, e := range expected {
		if !slices.Contains(answers, e) {
			missing = append(missing, e)
		}
	}
	if len(missing) > 0 {
		return &checker.AssertionError{Msg: fmt.Sprintf("expected %s not in answer [%s]", strings.Join(missing, ", "), strings.Join(answers, ", "))}
	}
	if cfg.MatchMode == "equals" && len(answers) != len(expected) {
		return &checker.AssertionError{Msg: fmt.Sprintf("answer [%s] has additional records", strings.Join(answers, ", "))}
	}
	return nil
}
