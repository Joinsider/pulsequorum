// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package config loads and validates the node configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Role is the role a node runs in.
type Role string

// Supported node roles: a voter participates in the Raft cluster, a probe only
// runs checks.
const (
	RoleVoter Role = "voter"
	RoleProbe Role = "probe"
)

// AuthMode selects how users authenticate.
type AuthMode string

// Supported authentication modes.
const (
	AuthLocal   AuthMode = "local"
	AuthZitadel AuthMode = "zitadel"
)

// Config is the validated node configuration.
type Config struct {
	NodeName    string
	Role        Role
	DataDir     string
	HTTPAddr    string
	ClusterAddr string
	// ClusterAdvertise is the stable host:port other nodes use to reach this
	// node; it becomes the Raft server address (ADR 0005, FA-CLU-10).
	ClusterAdvertise string
	// RaftTimeout is the Raft heartbeat and election timeout (ADR 0012 A6).
	RaftTimeout time.Duration
	PublicURL   string
	// CORSOrigins are the origins (scheme://host[:port]) of external frontends
	// allowed to call the API from a browser (FA-API-05). The origin of
	// PublicURL is always included.
	CORSOrigins []string
	// MasterKey (base64) or MasterKeyFile provide the KEK (ADR 0011). Never log them.
	MasterKey     string
	MasterKeyFile string
	// ICMPMode is auto, privileged, unprivileged or off (FA-CHK-31).
	ICMPMode string
	// SSRFProtection is auto, on or off (FA-SEC-SSRF); auto enables it in
	// multi-tenant (Zitadel) mode.
	SSRFProtection string
	AuthMode       AuthMode
	// RawCompaction compacts heartbeats older than 48 h (FA-UPT-04).
	RawCompaction bool
	// Retention of raw data, hourly and daily rollups (FA-UPT-05); 0 = forever.
	RetentionRaw    time.Duration
	RetentionHourly time.Duration
	RetentionDaily  time.Duration
	// AuthCookieDomain is the Domain of the refresh cookie; empty = host-only cookie (ADR 0008).
	AuthCookieDomain string
	// AlertMaxAttempts is the number of delivery attempts per alert and channel (FA-ALR-03).
	AlertMaxAttempts int
	LogLevel         slog.Level
	LogFormat        string
	ShutdownTimeout  time.Duration
}

// Load reads the configuration using getenv (usually os.Getenv) and validates it.
// All validation errors are reported at once.
func Load(getenv func(string) string) (Config, error) {
	get := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}

	hostname, _ := os.Hostname()
	cfg := Config{
		NodeName:    get("NODE_NAME", hostname),
		Role:        Role(get("NODE_ROLE", string(RoleVoter))),
		DataDir:     get("DATA_DIR", "/data"),
		HTTPAddr:    get("HTTP_ADDR", ":8080"),
		ClusterAddr: get("CLUSTER_ADDR", ":9443"),
		PublicURL:   strings.TrimRight(get("PUBLIC_URL", ""), "/"),
		AuthMode:    AuthMode(get("AUTH_MODE", string(AuthLocal))),
		LogFormat:   get("LOG_FORMAT", "json"),
	}

	var errs []error
	switch cfg.Role {
	case RoleVoter, RoleProbe:
	default:
		errs = append(errs, fmt.Errorf("NODE_ROLE: must be %q or %q, got %q", RoleVoter, RoleProbe, cfg.Role))
	}

	switch cfg.AuthMode {
	case AuthLocal:
	case AuthZitadel:
		errs = append(errs, errors.New("AUTH_MODE: zitadel is not supported yet"))
	default:
		errs = append(errs, fmt.Errorf("AUTH_MODE: must be %q or %q, got %q", AuthLocal, AuthZitadel, cfg.AuthMode))
	}

	if cfg.NodeName == "" {
		errs = append(errs, errors.New("NODE_NAME: must not be empty"))
	}

	if err := cfg.LogLevel.UnmarshalText([]byte(get("LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
	}

	switch cfg.LogFormat {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("LOG_FORMAT: must be \"json\" or \"text\", got %q", cfg.LogFormat))
	}

	if cfg.PublicURL != "" {
		u, err := url.Parse(cfg.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("PUBLIC_URL: must be an absolute http(s) URL, got %q", cfg.PublicURL))
		}
	}

	if cfg.PublicURL != "" {
		if o, err := ParseOrigin(cfg.PublicURL); err == nil {
			cfg.CORSOrigins = append(cfg.CORSOrigins, o)
		}
	}
	for _, raw := range strings.Split(get("CORS_ORIGINS", ""), ",") {
		if raw = strings.TrimSpace(raw); raw == "" {
			continue
		}
		o, err := ParseOrigin(raw)
		if u, _ := url.Parse(raw); err == nil && u.Path != "" && u.Path != "/" {
			err = fmt.Errorf("must be an origin without path, got %q", raw)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("CORS_ORIGINS: %w", err))
			continue
		}
		if !slices.Contains(cfg.CORSOrigins, o) {
			cfg.CORSOrigins = append(cfg.CORSOrigins, o)
		}
	}

	cfg.MasterKey = getenv("MASTER_KEY")
	cfg.MasterKeyFile = get("MASTER_KEY_FILE", "")
	if cfg.MasterKey != "" && cfg.MasterKeyFile != "" {
		errs = append(errs, errors.New("MASTER_KEY and MASTER_KEY_FILE: set only one"))
	}

	cfg.ICMPMode = get("ICMP_MODE", "auto")
	switch cfg.ICMPMode {
	case "auto", "privileged", "unprivileged", "off":
	default:
		errs = append(errs, fmt.Errorf("ICMP_MODE: must be auto, privileged, unprivileged or off, got %q", cfg.ICMPMode))
	}

	cfg.SSRFProtection = get("SSRF_PROTECTION", "auto")
	switch cfg.SSRFProtection {
	case "auto", "on", "off":
	default:
		errs = append(errs, fmt.Errorf("SSRF_PROTECTION: must be auto, on or off, got %q", cfg.SSRFProtection))
	}

	cfg.ClusterAdvertise = get("CLUSTER_ADVERTISE", "")
	if cfg.ClusterAdvertise == "" {
		_, port, _ := net.SplitHostPort(cfg.ClusterAddr)
		cfg.ClusterAdvertise = net.JoinHostPort(cfg.NodeName, port)
	}
	if host, port, err := net.SplitHostPort(cfg.ClusterAdvertise); err != nil || host == "" || port == "" {
		errs = append(errs, fmt.Errorf("CLUSTER_ADVERTISE: must be host:port, got %q", cfg.ClusterAdvertise))
	}

	raftTimeout, err := time.ParseDuration(get("RAFT_TIMEOUT", "1s"))
	if err != nil || raftTimeout < 100*time.Millisecond {
		errs = append(errs, fmt.Errorf("RAFT_TIMEOUT: must be a duration >= 100ms, got %q", getenv("RAFT_TIMEOUT")))
	}
	cfg.RaftTimeout = raftTimeout

	switch v := get("RAW_COMPACTION", "true"); v {
	case "true", "false":
		cfg.RawCompaction = v == "true"
	default:
		errs = append(errs, fmt.Errorf("RAW_COMPACTION: must be true or false, got %q", v))
	}
	days := func(key, def string, minDays int) time.Duration {
		v := get(key, def)
		n, err := strconv.Atoi(v)
		if err != nil || n < minDays {
			errs = append(errs, fmt.Errorf("%s: must be a whole number of days >= %d, got %q", key, minDays, v))
			return 0
		}
		return time.Duration(n) * 24 * time.Hour
	}
	// Raw data must outlive compaction and the rollup of the last day.
	cfg.RetentionRaw = days("RETENTION_RAW_DAYS", "30", 3)
	cfg.RetentionHourly = days("RETENTION_HOURLY_DAYS", "365", 1)
	cfg.RetentionDaily = days("RETENTION_DAILY_DAYS", "0", 0)

	cfg.AuthCookieDomain = get("AUTH_COOKIE_DOMAIN", "")
	if strings.ContainsAny(cfg.AuthCookieDomain, " ;,/:") {
		errs = append(errs, fmt.Errorf("AUTH_COOKIE_DOMAIN: must be a domain name, got %q", cfg.AuthCookieDomain))
	}

	attemptsRaw := get("ALERT_MAX_ATTEMPTS", "10")
	attempts, err := strconv.Atoi(attemptsRaw)
	if err != nil || attempts < 1 || attempts > 100 {
		errs = append(errs, fmt.Errorf("ALERT_MAX_ATTEMPTS: must be a whole number from 1 to 100, got %q", attemptsRaw))
	}
	cfg.AlertMaxAttempts = attempts

	timeout, err := time.ParseDuration(get("SHUTDOWN_TIMEOUT", "10s"))
	if err != nil || timeout <= 0 {
		errs = append(errs, fmt.Errorf("SHUTDOWN_TIMEOUT: must be a positive duration, got %q", getenv("SHUTDOWN_TIMEOUT")))
	}
	cfg.ShutdownTimeout = timeout

	return cfg, errors.Join(errs...)
}

// ParseOrigin returns the canonical origin (lower-case scheme://host[:port])
// of an absolute http(s) URL. URLs with a path, query or user info are
// rejected for CORS_ORIGINS; PUBLIC_URL may carry a path, which is dropped.
func ParseOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("must be an absolute http(s) origin, got %q", raw)
	}
	return strings.ToLower(u.Scheme + "://" + u.Host), nil
}

// SSRFProtectionEnabled reports whether check and notification targets in
// private networks are blocked.
func (c Config) SSRFProtectionEnabled() bool {
	return c.SSRFProtection == "on" || (c.SSRFProtection == "auto" && c.AuthMode == AuthZitadel)
}
