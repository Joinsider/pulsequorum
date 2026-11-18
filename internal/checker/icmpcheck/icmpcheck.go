// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package icmpcheck implements the ICMP (ping) monitor type (FA-CHK-30/31).
package icmpcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"regexp"
	"time"

	probing "github.com/prometheus-community/pro-bing"

	"github.com/joinsider/uptimecluster/internal/checker"
)

// Type is the monitor type key.
const Type = "icmp"

// Capability is the node capability ICMP monitors require (FA-CHK-31).
const Capability = "icmp"

// ICMP socket modes.
const (
	ModeAuto         = "auto"
	ModePrivileged   = "privileged"   // raw socket, needs CAP_NET_RAW
	ModeUnprivileged = "unprivileged" // UDP ICMP socket, needs net.ipv4.ping_group_range
	ModeOff          = "off"
)

// Config is the ICMP monitor configuration.
type Config struct {
	Host       string `json:"host"`
	Count      int    `json:"count,omitempty"`
	IntervalMs int    `json:"intervalMs,omitempty"`
	// MaxPacketLossPercent: DOWN if the loss exceeds it. The default 100 means
	// DOWN only when no reply arrives.
	MaxPacketLossPercent *int   `json:"maxPacketLossPercent,omitempty"`
	IPFamily             string `json:"ipFamily,omitempty"`
}

const schemaJSON = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "ICMP (Ping)",
  "type": "object",
  "additionalProperties": false,
  "required": ["host"],
  "properties": {
    "host": {"type": "string", "minLength": 1, "maxLength": 253},
    "count": {"type": "integer", "minimum": 1, "maximum": 20, "default": 3},
    "intervalMs": {"type": "integer", "minimum": 100, "maximum": 5000, "default": 250},
    "maxPacketLossPercent": {"type": "integer", "minimum": 0, "maximum": 100, "default": 100},
    "ipFamily": {"type": "string", "enum": ["auto", "ipv4", "ipv6"], "default": "auto"}
  }
}`

// Checker runs ICMP checks.
type Checker struct{}

var (
	_ checker.Checker            = (*Checker)(nil)
	_ checker.CapabilityRequirer = (*Checker)(nil)
)

var (
	_ checker.Checker      = (*Checker)(nil)
	_ checker.TargetLister = (*Checker)(nil)
)

func init() { checker.Default.Register(&Checker{}) }

// Type implements checker.Checker.
func (*Checker) Type() string { return Type }

// Name implements checker.Checker.
func (*Checker) Name() string { return "ICMP (Ping)" }

// RequiredCapability implements checker.CapabilityRequirer.
func (*Checker) RequiredCapability() string { return Capability }

// ConfigSchema implements checker.Checker.
func (*Checker) ConfigSchema() json.RawMessage { return json.RawMessage(schemaJSON) }

var numericHost = regexp.MustCompile(`(?i)^(?:[0-9.]+|[0-9a-fx.]*0x[0-9a-fx.]*)$`)

// Validate implements checker.Checker.
func (*Checker) Validate(raw json.RawMessage) error {
	cfg, err := parse(raw)
	if err != nil {
		return err
	}
	if _, perr := netip.ParseAddr(cfg.Host); perr != nil && numericHost.MatchString(cfg.Host) {
		return &checker.ValidationError{Fields: []checker.FieldError{{Field: "/host", Message: "non-canonical IP address notation is not allowed"}}}
	}
	return nil
}

// Targets implements checker.TargetLister: the configured host is the
// destination (ADR 0030).
func (*Checker) Targets(raw json.RawMessage) ([]checker.Target, error) {
	cfg, err := parse(raw)
	if err != nil {
		return nil, err
	}
	return []checker.Target{{Field: "/host", Host: cfg.Host}}, nil
}

func parse(raw json.RawMessage) (*Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("decode configuration: %w", err)
	}
	if c.Count == 0 {
		c.Count = 3
	}
	if c.IntervalMs == 0 {
		c.IntervalMs = 250
	}
	if c.MaxPacketLossPercent == nil {
		maxLoss := 100
		c.MaxPacketLossPercent = &maxLoss
	}
	return &c, nil
}

// Check implements checker.Checker. pro-bing opens its own sockets, so the
// target is resolved here and checked against the dialer's address policy
// before any packet is sent (FA-SEC-SSRF).
func (*Checker) Check(ctx context.Context, env *checker.Env, raw json.RawMessage) checker.Result {
	cfg, err := parse(raw)
	if err != nil {
		return checker.Result{Status: checker.StatusDown, Message: err.Error(), ErrorClass: checker.ClassInternal}
	}
	if env.ICMPMode != ModePrivileged && env.ICMPMode != ModeUnprivileged {
		return checker.Result{Status: checker.StatusDown, ErrorClass: checker.ClassInternal,
			Message: "ICMP is not available on this node (see the ICMP section of the operations docs)"}
	}

	start := time.Now()
	ip, err := resolve(ctx, cfg.Host, cfg.IPFamily)
	if err != nil {
		return checker.Down(err, int(time.Since(start).Milliseconds()), nil)
	}
	details := map[string]any{"resolvedIp": ip.String(), "mode": env.ICMPMode}
	if err := env.Dialer.Allow(ip); err != nil {
		return checker.Down(err, 0, details)
	}

	stats, err := ping(ctx, ip, env.ICMPMode, cfg.Count, time.Duration(cfg.IntervalMs)*time.Millisecond)
	if err != nil {
		return checker.Down(err, 0, details)
	}
	avgMs := int(math.Ceil(float64(stats.AvgRtt) / float64(time.Millisecond)))
	details["packetsSent"] = stats.PacketsSent
	details["packetsReceived"] = stats.PacketsRecv
	details["packetLossPercent"] = stats.PacketLoss
	details["rttMinMs"] = durationMs(stats.MinRtt)
	details["rttAvgMs"] = durationMs(stats.AvgRtt)
	details["rttMaxMs"] = durationMs(stats.MaxRtt)

	switch {
	case stats.PacketsRecv == 0:
		return checker.Result{Status: checker.StatusDown, ErrorClass: checker.ClassConnect, Details: details,
			Message: fmt.Sprintf("no reply from %s (100%% packet loss)", ip)}
	case stats.PacketLoss > float64(*cfg.MaxPacketLossPercent):
		return checker.Result{Status: checker.StatusDown, LatencyMs: avgMs, ErrorClass: checker.ClassAssertion, Details: details,
			Message: fmt.Sprintf("packet loss %.0f%% exceeds %d%%", stats.PacketLoss, *cfg.MaxPacketLossPercent)}
	}
	return checker.Result{Status: checker.StatusUp, LatencyMs: avgMs, Details: details,
		Message: fmt.Sprintf("%d/%d replies, avg %.2f ms", stats.PacketsRecv, stats.PacketsSent, durationMs(stats.AvgRtt))}
}

func durationMs(d time.Duration) float64 {
	return math.Round(float64(d)/float64(time.Microsecond)) / 1000
}

func resolve(ctx context.Context, host, family string) (netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap(), nil
	}
	network := map[string]string{"ipv4": "ip4", "ipv6": "ip6"}[family]
	if network == "" {
		network = "ip"
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, network, host)
	if err != nil {
		return netip.Addr{}, err
	}
	if len(ips) == 0 {
		return netip.Addr{}, &net.DNSError{Err: "no addresses", Name: host, IsNotFound: true}
	}
	return ips[0].Unmap(), nil
}

func ping(ctx context.Context, ip netip.Addr, mode string, count int, interval time.Duration) (*probing.Statistics, error) {
	p := probing.New("")
	p.SetIPAddr(&net.IPAddr{IP: ip.AsSlice()})
	p.SetPrivileged(mode == ModePrivileged)
	p.Count = count
	p.Interval = interval
	if deadline, ok := ctx.Deadline(); ok {
		p.Timeout = time.Until(deadline)
	} else {
		p.Timeout = time.Duration(count)*interval + 2*time.Second
	}
	if err := p.RunWithContext(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("ping %s (%s mode): %w", ip, mode, err)
	}
	return p.Statistics(), nil
}

// DetectMode returns the ICMP mode this node can use by pinging the loopback
// address (FA-CHK-31, FA-NODE-06). configured is auto, privileged,
// unprivileged or off. For auto, unprivileged is tried first because it needs
// no capability.
func DetectMode(ctx context.Context, configured string) (string, error) {
	var candidates []string
	switch configured {
	case ModeOff:
		return ModeOff, nil
	case ModePrivileged, ModeUnprivileged:
		candidates = []string{configured}
	case ModeAuto, "":
		candidates = []string{ModeUnprivileged, ModePrivileged}
	default:
		return ModeOff, fmt.Errorf("unknown ICMP mode %q", configured)
	}
	var errs []error
	for _, mode := range candidates {
		pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		stats, err := ping(pctx, netip.MustParseAddr("127.0.0.1"), mode, 1, 100*time.Millisecond)
		cancel()
		if err == nil && stats.PacketsRecv > 0 {
			return mode, nil
		}
		if err == nil {
			err = errors.New("no reply from 127.0.0.1")
		}
		errs = append(errs, fmt.Errorf("%s: %w", mode, err))
	}
	return ModeOff, errors.Join(errs...)
}
