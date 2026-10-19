// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package config loads and validates the node configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"
)

// Role is the role a node runs in.
type Role string

const (
	RoleVoter Role = "voter"
	RoleProbe Role = "probe"
)

// AuthMode selects how users authenticate.
type AuthMode string

const (
	AuthLocal   AuthMode = "local"
	AuthZitadel AuthMode = "zitadel"
)

// Config is the validated node configuration.
type Config struct {
	NodeName        string
	Role            Role
	DataDir         string
	HTTPAddr        string
	ClusterAddr     string
	PublicURL       string
	AuthMode        AuthMode
	LogLevel        slog.Level
	LogFormat       string
	ShutdownTimeout time.Duration
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

	timeout, err := time.ParseDuration(get("SHUTDOWN_TIMEOUT", "10s"))
	if err != nil || timeout <= 0 {
		errs = append(errs, fmt.Errorf("SHUTDOWN_TIMEOUT: must be a positive duration, got %q", getenv("SHUTDOWN_TIMEOUT")))
	}
	cfg.ShutdownTimeout = timeout

	return cfg, errors.Join(errs...)
}
