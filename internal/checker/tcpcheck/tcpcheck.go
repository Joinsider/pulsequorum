// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package tcpcheck implements the TCP monitor type (FA-CHK-20).
package tcpcheck

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"time"

	"github.com/joinsider/pulsequorum/internal/checker"
)

// Type is the monitor type key.
const Type = "tcp"

// Config is the TCP monitor configuration.
type Config struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	// TLS performs a TLS handshake after connecting and verifies the certificate.
	TLS            bool   `json:"tls,omitempty"`
	TLSServerName  string `json:"tlsServerName,omitempty"`
	IgnoreTLSError bool   `json:"ignoreTlsErrors,omitempty"`
	IPFamily       string `json:"ipFamily,omitempty"`
}

const schemaJSON = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "TCP",
  "type": "object",
  "additionalProperties": false,
  "required": ["host", "port"],
  "properties": {
    "host": {"type": "string", "minLength": 1, "maxLength": 253},
    "port": {"type": "integer", "minimum": 1, "maximum": 65535},
    "tls": {"type": "boolean", "default": false},
    "tlsServerName": {"type": "string", "maxLength": 253, "description": "Defaults to host"},
    "ignoreTlsErrors": {"type": "boolean", "default": false},
    "ipFamily": {"type": "string", "enum": ["auto", "ipv4", "ipv6"], "default": "auto"}
  }
}`

// Checker runs TCP checks.
type Checker struct {
	// RootCAs overrides the system trust store (tests only).
	RootCAs *x509.CertPool
}

var _ checker.Checker = (*Checker)(nil)
var _ checker.TargetLister = (*Checker)(nil)

func init() { checker.Default.Register(&Checker{}) }

// Type implements checker.Checker.
func (*Checker) Type() string { return Type }

// Name implements checker.Checker.
func (*Checker) Name() string { return "TCP" }

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
	return &c, nil
}

// Check implements checker.Checker. The latency is the connect time, plus the
// handshake time when TLS is enabled.
func (c *Checker) Check(ctx context.Context, env *checker.Env, raw json.RawMessage) checker.Result {
	cfg, err := parse(raw)
	if err != nil {
		return checker.Result{Status: checker.StatusDown, Message: err.Error(), ErrorClass: checker.ClassInternal}
	}
	network := map[string]string{"ipv4": "tcp4", "ipv6": "tcp6"}[cfg.IPFamily]
	if network == "" {
		network = "tcp"
	}

	start := time.Now()
	conn, err := env.Dialer.DialContext(ctx, network, net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	connectMs := int(time.Since(start).Milliseconds())
	details := map[string]any{"connectMs": connectMs}
	if err != nil {
		return checker.Down(err, connectMs, details)
	}
	defer func() { _ = conn.Close() }()
	if host, _, err := net.SplitHostPort(conn.RemoteAddr().String()); err == nil {
		details["resolvedIp"] = host
	}
	if !cfg.TLS {
		return checker.Result{Status: checker.StatusUp, LatencyMs: connectMs, Message: "connected", Details: details}
	}

	serverName := cfg.TLSServerName
	if serverName == "" {
		serverName = cfg.Host
	}
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName:         serverName, // always set, so the host name is verified
		RootCAs:            c.RootCAs,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.IgnoreTLSError, //nolint:gosec // G402: explicit per-monitor opt-out
	})
	err = tlsConn.HandshakeContext(ctx)
	latency := int(time.Since(start).Milliseconds())
	if err != nil {
		return checker.Down(err, latency, details)
	}
	state := tlsConn.ConnectionState()
	details["tlsVersion"] = tls.VersionName(state.Version)
	if len(state.PeerCertificates) > 0 {
		details["tlsExpiresAt"] = state.PeerCertificates[0].NotAfter.UTC().Format(time.RFC3339)
	}
	return checker.Result{Status: checker.StatusUp, LatencyMs: latency, Message: "TLS handshake completed", Details: details}
}
