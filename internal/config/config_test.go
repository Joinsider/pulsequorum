// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package config

import (
	"log/slog"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{"NODE_NAME": "n1"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Role != RoleVoter || cfg.AuthMode != AuthLocal || cfg.HTTPAddr != ":8080" ||
		cfg.ClusterAddr != ":9443" || cfg.DataDir != "/data" || cfg.LogFormat != "json" ||
		cfg.LogLevel != slog.LevelInfo || cfg.ShutdownTimeout != 10*time.Second {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
	day := 24 * time.Hour
	if !cfg.RawCompaction || cfg.RetentionRaw != 30*day || cfg.RetentionHourly != 365*day || cfg.RetentionDaily != 0 {
		t.Errorf("unexpected data defaults: compaction %v, retention %v/%v/%v", cfg.RawCompaction, cfg.RetentionRaw, cfg.RetentionHourly, cfg.RetentionDaily)
	}
	if cfg.AlertMaxAttempts != 10 {
		t.Errorf("AlertMaxAttempts = %d, want 10", cfg.AlertMaxAttempts)
	}
	if cfg.DegradedAfter != 15*time.Second {
		t.Errorf("DegradedAfter = %v, want 15s", cfg.DegradedAfter)
	}
	if cfg.AuditRetention != 8760*time.Hour {
		t.Errorf("AuditRetention = %v, want 8760h", cfg.AuditRetention)
	}
}

// ADR 0031: AUDIT_RETENTION is a duration of at least 24 hours.
func TestLoadAuditRetention(t *testing.T) {
	cfg, err := Load(env(map[string]string{"NODE_NAME": "n1", "AUDIT_RETENTION": "720h"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AuditRetention != 720*time.Hour {
		t.Errorf("AuditRetention = %v, want 720h", cfg.AuditRetention)
	}
	cfg, err = Load(env(map[string]string{"NODE_NAME": "n1", "AUDIT_RETENTION": "24h"}))
	if err != nil {
		t.Fatalf("minimum 24h rejected: %v", err)
	}
	if cfg.AuditRetention != 24*time.Hour {
		t.Errorf("AuditRetention = %v, want 24h", cfg.AuditRetention)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"NODE_NAME":            "probe-1",
		"NODE_ROLE":            "probe",
		"PUBLIC_URL":           "https://mon1.example.com/",
		"LOG_LEVEL":            "debug",
		"LOG_FORMAT":           "text",
		"SHUTDOWN_TIMEOUT":     "3s",
		"RAW_COMPACTION":       "false",
		"RETENTION_RAW_DAYS":   "7",
		"RETENTION_DAILY_DAYS": "3650",
		"ALERT_MAX_ATTEMPTS":   "3",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Role != RoleProbe {
		t.Errorf("Role = %q", cfg.Role)
	}
	if cfg.PublicURL != "https://mon1.example.com" {
		t.Errorf("PublicURL = %q, want trailing slash trimmed", cfg.PublicURL)
	}
	if cfg.RawCompaction || cfg.RetentionRaw != 7*24*time.Hour || cfg.RetentionDaily != 3650*24*time.Hour {
		t.Errorf("data settings = %v %v %v", cfg.RawCompaction, cfg.RetentionRaw, cfg.RetentionDaily)
	}
	if cfg.AlertMaxAttempts != 3 {
		t.Errorf("AlertMaxAttempts = %d, want 3", cfg.AlertMaxAttempts)
	}
	if cfg.LogLevel != slog.LevelDebug || cfg.LogFormat != "text" || cfg.ShutdownTimeout != 3*time.Second {
		t.Errorf("unexpected values: %+v", cfg)
	}
}

func TestLoadInvalid(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{"role", map[string]string{"NODE_ROLE": "leader"}, "NODE_ROLE"},
		{"auth mode", map[string]string{"AUTH_MODE": "ldap"}, "AUTH_MODE"},
		{"log level", map[string]string{"LOG_LEVEL": "loud"}, "LOG_LEVEL"},
		{"log format", map[string]string{"LOG_FORMAT": "xml"}, "LOG_FORMAT"},
		{"public url relative", map[string]string{"PUBLIC_URL": "mon1.example.com"}, "PUBLIC_URL"},
		{"public url scheme", map[string]string{"PUBLIC_URL": "ftp://mon1.example.com"}, "PUBLIC_URL"},
		{"shutdown timeout", map[string]string{"SHUTDOWN_TIMEOUT": "soon"}, "SHUTDOWN_TIMEOUT"},
		{"shutdown timeout negative", map[string]string{"SHUTDOWN_TIMEOUT": "-1s"}, "SHUTDOWN_TIMEOUT"},
		{"advertise without port", map[string]string{"CLUSTER_ADVERTISE": "raft-n1"}, "CLUSTER_ADVERTISE"},
		{"raft timeout too small", map[string]string{"RAFT_TIMEOUT": "10ms"}, "RAFT_TIMEOUT"},
		{"two master keys", map[string]string{"MASTER_KEY": "a", "MASTER_KEY_FILE": "/k"}, "MASTER_KEY"},
		{"icmp mode", map[string]string{"ICMP_MODE": "raw"}, "ICMP_MODE"},
		{"raw compaction", map[string]string{"RAW_COMPACTION": "yes"}, "RAW_COMPACTION"},
		{"raw retention too short", map[string]string{"RETENTION_RAW_DAYS": "2"}, "RETENTION_RAW_DAYS"},
		{"hourly retention not a number", map[string]string{"RETENTION_HOURLY_DAYS": "1y"}, "RETENTION_HOURLY_DAYS"},
		{"daily retention negative", map[string]string{"RETENTION_DAILY_DAYS": "-1"}, "RETENTION_DAILY_DAYS"},
		{"ssrf protection", map[string]string{"SSRF_PROTECTION": "maybe"}, "SSRF_PROTECTION"},
		{"alert attempts not a number", map[string]string{"ALERT_MAX_ATTEMPTS": "many"}, "ALERT_MAX_ATTEMPTS"},
		{"alert attempts zero", map[string]string{"ALERT_MAX_ATTEMPTS": "0"}, "ALERT_MAX_ATTEMPTS"},
		{"alert attempts above limit", map[string]string{"ALERT_MAX_ATTEMPTS": "101"}, "ALERT_MAX_ATTEMPTS"},
		{"alert bundle window not a duration", map[string]string{"ALERT_BUNDLE_WINDOW": "soon"}, "ALERT_BUNDLE_WINDOW"},
		{"alert bundle window negative", map[string]string{"ALERT_BUNDLE_WINDOW": "-5s"}, "ALERT_BUNDLE_WINDOW"},
		{"audit retention not a duration", map[string]string{"AUDIT_RETENTION": "a year"}, "AUDIT_RETENTION"},
		{"audit retention below 24h", map[string]string{"AUDIT_RETENTION": "23h"}, "AUDIT_RETENTION"},
		{"audit retention zero", map[string]string{"AUDIT_RETENTION": "0"}, "AUDIT_RETENTION"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.env["NODE_NAME"] = "n1"
			_, err := Load(env(tt.env))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadReportsAllErrors(t *testing.T) {
	_, err := Load(env(map[string]string{"NODE_NAME": "n1", "NODE_ROLE": "x", "LOG_FORMAT": "y"}))
	if err == nil || !strings.Contains(err.Error(), "NODE_ROLE") || !strings.Contains(err.Error(), "LOG_FORMAT") {
		t.Fatalf("error = %v, want both NODE_ROLE and LOG_FORMAT", err)
	}
}

func TestClusterAdvertiseDefault(t *testing.T) {
	cfg, err := Load(env(map[string]string{"NODE_NAME": "vps1", "CLUSTER_ADDR": ":9500"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClusterAdvertise != "vps1:9500" || cfg.RaftTimeout != time.Second {
		t.Errorf("ClusterAdvertise = %q, RaftTimeout = %s", cfg.ClusterAdvertise, cfg.RaftTimeout)
	}
	cfg, err = Load(env(map[string]string{"NODE_NAME": "vps1", "CLUSTER_ADVERTISE": "mon1.example.com:9443", "RAFT_TIMEOUT": "500ms"}))
	if err != nil || cfg.ClusterAdvertise != "mon1.example.com:9443" || cfg.RaftTimeout != 500*time.Millisecond {
		t.Errorf("override: %+v, %v", cfg, err)
	}
}

func TestSSRFProtectionEnabled(t *testing.T) {
	zitadel := OIDCConfig{Profile: ProfileZitadel}
	generic := OIDCConfig{Profile: ProfileGeneric}
	for _, tt := range []struct {
		name    string
		setting string
		cfg     Config
		want    bool
	}{
		{"auto local", "auto", Config{AuthMode: AuthLocal}, false},
		{"auto legacy zitadel value", "auto", Config{AuthMode: AuthZitadel}, true},
		{"auto oidc zitadel profile", "auto", Config{AuthMode: AuthOIDC, OIDC: zitadel}, true},
		{"auto oidc generic profile", "auto", Config{AuthMode: AuthOIDC, OIDC: generic}, false},
		{"on local", "on", Config{AuthMode: AuthLocal}, true},
		{"off zitadel profile", "off", Config{AuthMode: AuthOIDC, OIDC: zitadel}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			cfg.SSRFProtection = tt.setting
			if got := cfg.SSRFProtectionEnabled(); got != tt.want {
				t.Errorf("SSRFProtectionEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAuthCookieDomain covers ADR 0008: empty means a host-only refresh cookie,
// a domain is accepted, anything that is not a bare domain name is rejected.
func TestAuthCookieDomain(t *testing.T) {
	cfg, err := Load(env(map[string]string{"NODE_NAME": "n1"}))
	if err != nil || cfg.AuthCookieDomain != "" {
		t.Fatalf("default = %q, %v; want empty", cfg.AuthCookieDomain, err)
	}
	cfg, err = Load(env(map[string]string{"NODE_NAME": "n1", "AUTH_COOKIE_DOMAIN": "example.com"}))
	if err != nil || cfg.AuthCookieDomain != "example.com" {
		t.Fatalf("AUTH_COOKIE_DOMAIN = %q, %v; want example.com", cfg.AuthCookieDomain, err)
	}
	for _, v := range []string{"a b", "x;y", "http://x", "example.com/path", "a,b"} {
		_, err := Load(env(map[string]string{"NODE_NAME": "n1", "AUTH_COOKIE_DOMAIN": v}))
		if err == nil || !strings.Contains(err.Error(), "AUTH_COOKIE_DOMAIN") {
			t.Errorf("AUTH_COOKIE_DOMAIN=%q: error = %v, want AUTH_COOKIE_DOMAIN error", v, err)
		}
	}
}

func TestCORSOrigins(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"NODE_NAME":    "n1",
		"PUBLIC_URL":   "https://Mon1.example.com/ui",
		"CORS_ORIGINS": " https://status.example.com , http://localhost:4200/,https://mon1.example.com",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"https://mon1.example.com", "https://status.example.com", "http://localhost:4200"}
	if strings.Join(cfg.CORSOrigins, " ") != strings.Join(want, " ") {
		t.Errorf("CORSOrigins = %v, want %v", cfg.CORSOrigins, want)
	}

	for _, bad := range []string{"*", "example.com", "ftp://example.com", "https://example.com/app", "https://u:p@example.com", "https://example.com?x=1"} {
		if _, err := Load(env(map[string]string{"NODE_NAME": "n1", "CORS_ORIGINS": bad})); err == nil || !strings.Contains(err.Error(), "CORS_ORIGINS") {
			t.Errorf("CORS_ORIGINS=%q: want error, got %v", bad, err)
		}
	}
}

// TestClusterJoin covers CLUSTER_JOIN (FA-CLU-03): a comma-separated list of
// host:port seeds; empty entries are skipped, anything else must be host:port.
func TestClusterJoin(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    []string
		wantErr string
	}{
		{name: "unset", value: "", want: nil},
		{name: "two seeds with spaces", value: "a:1, b:2", want: []string{"a:1", "b:2"}},
		{name: "empty entries skipped", value: " ,, a:1 ,,", want: []string{"a:1"}},
		{name: "only separators", value: ",,", want: nil},
		{name: "no port", value: "nohostport", wantErr: "CLUSTER_JOIN"},
		{name: "one bad entry", value: "a:1,nohostport", wantErr: "CLUSTER_JOIN"},
		{name: "empty host", value: ":9443", wantErr: "CLUSTER_JOIN"},
		{name: "empty port", value: "voter1:", wantErr: "CLUSTER_JOIN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(env(map[string]string{"NODE_NAME": "n1", "CLUSTER_JOIN": tt.value}))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if strings.Join(cfg.ClusterJoin, " ") != strings.Join(tt.want, " ") || len(cfg.ClusterJoin) != len(tt.want) {
				t.Errorf("ClusterJoin = %q, want %q", cfg.ClusterJoin, tt.want)
			}
		})
	}
}

// TestJoinTokenSources checks that the join token comes from one source only.
func TestJoinTokenSources(t *testing.T) {
	cfg, err := Load(env(map[string]string{"NODE_NAME": "n1", "JOIN_TOKEN": "uc1.x"}))
	if err != nil || cfg.JoinToken != "uc1.x" || cfg.JoinTokenFile != "" {
		t.Errorf("JOIN_TOKEN only: token %q, file %q, err %v", cfg.JoinToken, cfg.JoinTokenFile, err)
	}

	cfg, err = Load(env(map[string]string{"NODE_NAME": "n1", "JOIN_TOKEN_FILE": "/run/secrets/join"}))
	if err != nil || cfg.JoinTokenFile != "/run/secrets/join" || cfg.JoinToken != "" {
		t.Errorf("JOIN_TOKEN_FILE only: token %q, file %q, err %v", cfg.JoinToken, cfg.JoinTokenFile, err)
	}

	_, err = Load(env(map[string]string{"NODE_NAME": "n1", "JOIN_TOKEN": "uc1.x", "JOIN_TOKEN_FILE": "/run/secrets/join"}))
	if err == nil || !strings.Contains(err.Error(), "JOIN_TOKEN") {
		t.Errorf("both sources: error = %v, want a JOIN_TOKEN error", err)
	}
}

// TestSelfCheck covers SELF_CHECK_TARGETS and SELF_HEALTH_FAIL_RATIO (ADR 0022):
// entries are trimmed, blank entries are skipped, invalid entries are named.
func TestSelfCheck(t *testing.T) {
	cfg, err := Load(env(map[string]string{"NODE_NAME": "n1"}))
	if err != nil || cfg.SelfCheckTargets != nil || cfg.SelfHealthFailRatio != 0.5 {
		t.Fatalf("defaults = %v, %v, %v; want no targets, ratio 0.5", cfg.SelfCheckTargets, cfg.SelfHealthFailRatio, err)
	}

	cfg, err = Load(env(map[string]string{
		"NODE_NAME":              "n1",
		"SELF_CHECK_TARGETS":     " 1.1.1.1:443 , ,https://example.com/ping?x=1,http://example.org ",
		"SELF_HEALTH_FAIL_RATIO": "1",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"1.1.1.1:443", "https://example.com/ping?x=1", "http://example.org"}
	if strings.Join(cfg.SelfCheckTargets, " ") != strings.Join(want, " ") {
		t.Errorf("SelfCheckTargets = %q, want %q", cfg.SelfCheckTargets, want)
	}
	if cfg.SelfHealthFailRatio != 1 {
		t.Errorf("SelfHealthFailRatio = %v, want 1", cfg.SelfHealthFailRatio)
	}

	invalidTargets := []string{"nohostport", "host:", "host:0", "host:70000", "ftp://example.com", "https://", "https://u:p@example.com"}
	for _, bad := range invalidTargets {
		_, err := Load(env(map[string]string{"NODE_NAME": "n1", "SELF_CHECK_TARGETS": "1.1.1.1:443," + bad}))
		if err == nil || !strings.Contains(err.Error(), "SELF_CHECK_TARGETS") || !strings.Contains(err.Error(), bad) {
			t.Errorf("SELF_CHECK_TARGETS entry %q: error = %v, want error naming the entry", bad, err)
		}
	}

	for _, bad := range []string{"0", "-0.1", "1.5", "abc", "NaN"} {
		_, err := Load(env(map[string]string{"NODE_NAME": "n1", "SELF_HEALTH_FAIL_RATIO": bad}))
		if err == nil || !strings.Contains(err.Error(), "SELF_HEALTH_FAIL_RATIO") {
			t.Errorf("SELF_HEALTH_FAIL_RATIO=%q: error = %v, want SELF_HEALTH_FAIL_RATIO error", bad, err)
		}
	}
}

// TestDegradedAfter covers DEGRADED_AFTER (ADR 0024): a duration of 0 or more,
// where 0 disables the degraded mode.
func TestDegradedAfter(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{name: "default", value: "", want: 15 * time.Second},
		{name: "custom", value: "30s", want: 30 * time.Second},
		{name: "disabled", value: "0", want: 0},
		{name: "not a duration", value: "soon", wantErr: true},
		{name: "negative", value: "-1s", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(env(map[string]string{"NODE_NAME": "n1", "DEGRADED_AFTER": tc.value}))
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "DEGRADED_AFTER") {
					t.Fatalf("error = %v, want DEGRADED_AFTER error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.DegradedAfter != tc.want {
				t.Errorf("DegradedAfter = %v, want %v", cfg.DegradedAfter, tc.want)
			}
		})
	}
}

// TestMetrics covers METRICS_ADDR (ADR 0025, section 7): off by default, a
// host:port with a valid port otherwise. METRICS_TOKEN and METRICS_TOKEN_FILE
// are exclusive.
func TestMetrics(t *testing.T) {
	cfg, err := Load(env(map[string]string{"NODE_NAME": "n1"}))
	if err != nil || cfg.MetricsAddr != "" || cfg.MetricsToken != "" || cfg.MetricsTokenFile != "" {
		t.Fatalf("defaults = %q, %q, %q, %v; want all empty", cfg.MetricsAddr, cfg.MetricsToken, cfg.MetricsTokenFile, err)
	}

	valid := []string{"127.0.0.1:9100", ":9100", "[::1]:9100"}
	for _, addr := range valid {
		if _, err := Load(env(map[string]string{"NODE_NAME": "n1", "METRICS_ADDR": addr})); err != nil {
			t.Errorf("METRICS_ADDR %q: unexpected error %v", addr, err)
		}
	}
	invalid := []string{"9100", "127.0.0.1", "host:", "host:0", "host:70000", "host:http"}
	for _, addr := range invalid {
		_, err := Load(env(map[string]string{"NODE_NAME": "n1", "METRICS_ADDR": addr}))
		if err == nil || !strings.Contains(err.Error(), "METRICS_ADDR") {
			t.Errorf("METRICS_ADDR %q: error = %v, want a METRICS_ADDR error", addr, err)
		}
	}

	_, err = Load(env(map[string]string{"NODE_NAME": "n1", "METRICS_TOKEN": "a", "METRICS_TOKEN_FILE": "/run/secrets/m"})) //nolint:gosec // G101: test value, not a credential
	if err == nil || !strings.Contains(err.Error(), "METRICS_TOKEN_FILE") {
		t.Errorf("both token sources: error = %v, want a METRICS_TOKEN error", err)
	}
}

func TestResolveMetricsToken(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		p := dir + "/" + name
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tests := []struct {
		name    string
		env     map[string]string
		file    func() string
		want    string
		wantErr bool
	}{
		{name: "none", env: map[string]string{"NODE_NAME": "n1"}, want: ""},
		{name: "env token trimmed", env: map[string]string{"NODE_NAME": "n1", "METRICS_TOKEN": "  s3cret \n"}, want: "s3cret"},
		{name: "file trimmed", env: map[string]string{"NODE_NAME": "n1", "METRICS_TOKEN_FILE": write("ok", "s3cret\n")}, want: "s3cret"},
		{name: "empty file", env: map[string]string{"NODE_NAME": "n1", "METRICS_TOKEN_FILE": write("empty", " \n")}, wantErr: true},
		{name: "missing file", env: map[string]string{"NODE_NAME": "n1", "METRICS_TOKEN_FILE": dir + "/missing"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(env(tt.env))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got, err := cfg.ResolveMetricsToken()
			if (err != nil) != tt.wantErr {
				t.Fatalf("ResolveMetricsToken error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("token = %q, want %q", got, tt.want)
			}
		})
	}
}

// ADR 0025: the thresholds of the system alerts have defaults and are validated.
func TestSystemAlertThresholds(t *testing.T) {
	cfg, err := Load(env(map[string]string{"NODE_NAME": "n1"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ReplicationLagAlert != 5*time.Minute || cfg.ClockSkewAlert != 2*time.Second ||
		cfg.DiskFreeAlert != 0.15 || cfg.WALSizeAlert != 512<<20 || cfg.ProbeOfflineAlert != 5*time.Minute {
		t.Errorf("defaults = %v %v %v %v %v", cfg.ReplicationLagAlert, cfg.ClockSkewAlert, cfg.DiskFreeAlert, cfg.WALSizeAlert, cfg.ProbeOfflineAlert)
	}

	cfg, err = Load(env(map[string]string{
		"NODE_NAME": "n1", "REPLICATION_LAG_ALERT": "90s", "CLOCK_SKEW_ALERT": "500ms",
		"DISK_FREE_ALERT": "0.5", "WAL_SIZE_ALERT": "1GiB", "PROBE_OFFLINE_ALERT": "2m",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ReplicationLagAlert != 90*time.Second || cfg.ClockSkewAlert != 500*time.Millisecond ||
		cfg.DiskFreeAlert != 0.5 || cfg.WALSizeAlert != 1<<30 || cfg.ProbeOfflineAlert != 2*time.Minute {
		t.Errorf("overrides = %v %v %v %v %v", cfg.ReplicationLagAlert, cfg.ClockSkewAlert, cfg.DiskFreeAlert, cfg.WALSizeAlert, cfg.ProbeOfflineAlert)
	}

	invalid := map[string]string{
		"REPLICATION_LAG_ALERT": "0s",
		"CLOCK_SKEW_ALERT":      "-1s",
		"DISK_FREE_ALERT":       "1",
		"WAL_SIZE_ALERT":        "512M",
		"PROBE_OFFLINE_ALERT":   "0s",
	}
	for key, value := range invalid {
		_, err := Load(env(map[string]string{"NODE_NAME": "n1", key: value}))
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s=%q: error = %v, want it to name the key", key, value, err)
		}
	}
}

func TestParseByteSize(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "512MiB", want: 512 << 20},
		{in: " 2 KiB ", want: 2 << 10},
		{in: "1GiB", want: 1 << 30},
		{in: "4096", want: 4096},
		{in: "0B", want: 0},
		{in: "", wantErr: true},
		{in: "MiB", wantErr: true},
		{in: "-1MiB", wantErr: true},
		{in: "10MB", want: 10_000_000},
		{in: "10M", wantErr: true},
		{in: "9999999999999GiB", wantErr: true},
	}
	for _, tt := range tests {
		got, err := parseByteSize(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseByteSize(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("parseByteSize(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

// genericOIDC is a complete AUTH_MODE=oidc environment with profile generic.
func genericOIDC() map[string]string {
	return map[string]string{
		"NODE_NAME":        "n1",
		"AUTH_MODE":        "oidc",
		"OIDC_ISSUER":      "https://idp.example.com/realms/ops",
		"OIDC_AUDIENCE":    "pulsequorum",
		"OIDC_CLIENT_IDS":  "spa",
		"OIDC_ROLES_CLAIM": "roles",
	}
}

// zitadelOIDC is a complete AUTH_MODE=oidc environment with profile zitadel.
func zitadelOIDC() map[string]string {
	return map[string]string{
		"NODE_NAME":               "n1",
		"AUTH_MODE":               "oidc",
		"OIDC_PROFILE":            "zitadel",
		"OIDC_ISSUER":             "https://zitadel.example.com",
		"OIDC_AUDIENCE":           "300000000000001",
		"OIDC_CLIENT_IDS":         "300000000000002",
		"ZITADEL_PLATFORM_ORG_ID": "300000000000003",
	}
}

// with returns a copy of base with the key/value pairs in set applied. An
// empty value unsets the variable.
func with(base map[string]string, set ...string) map[string]string {
	out := maps.Clone(base)
	for i := 0; i+1 < len(set); i += 2 {
		out[set[i]] = set[i+1]
	}
	return out
}

// expectErr loads m and checks the error: want "" means no error, otherwise
// the error must contain want.
func expectErr(t *testing.T, m map[string]string, want string) Config {
	t.Helper()
	cfg, err := Load(env(m))
	if want == "" {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return cfg
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want containing %q", err, want)
	}
	return cfg
}

func TestOIDCBaseEnvironments(t *testing.T) {
	cfg := expectErr(t, genericOIDC(), "")
	if cfg.AuthMode != AuthOIDC || cfg.AuthModeAlias || cfg.OIDC.Profile != ProfileGeneric || cfg.OIDC.Tenancy != TenancySingle {
		t.Errorf("generic: mode %q alias %v profile %q tenancy %q", cfg.AuthMode, cfg.AuthModeAlias, cfg.OIDC.Profile, cfg.OIDC.Tenancy)
	}
	cfg = expectErr(t, zitadelOIDC(), "")
	if cfg.AuthMode != AuthOIDC || cfg.AuthModeAlias || cfg.OIDC.Profile != ProfileZitadel || cfg.OIDC.Tenancy != TenancySingle || cfg.OIDC.PlatformOrgID != "300000000000003" {
		t.Errorf("zitadel: %+v", cfg.OIDC)
	}
}

func TestOIDCRequiredVariables(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{"generic issuer missing", with(genericOIDC(), "OIDC_ISSUER", ""), "OIDC_ISSUER"},
		{"generic audience missing", with(genericOIDC(), "OIDC_AUDIENCE", ""), "OIDC_AUDIENCE"},
		{"generic client ids missing", with(genericOIDC(), "OIDC_CLIENT_IDS", ""), "OIDC_CLIENT_IDS"},
		{"generic roles claim missing", with(genericOIDC(), "OIDC_ROLES_CLAIM", ""), "OIDC_ROLES_CLAIM"},
		{"zitadel issuer missing", with(zitadelOIDC(), "OIDC_ISSUER", ""), "OIDC_ISSUER"},
		{"zitadel audience missing", with(zitadelOIDC(), "OIDC_AUDIENCE", ""), "OIDC_AUDIENCE"},
		{"zitadel client ids missing", with(zitadelOIDC(), "OIDC_CLIENT_IDS", ""), "OIDC_CLIENT_IDS"},
		{"zitadel platform org missing", with(zitadelOIDC(), "ZITADEL_PLATFORM_ORG_ID", ""), "ZITADEL_PLATFORM_ORG_ID"},
		{"unknown profile", with(genericOIDC(), "OIDC_PROFILE", "keycloak"), "OIDC_PROFILE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { expectErr(t, tt.env, tt.wantErr) })
	}
}

func TestOIDCIssuer(t *testing.T) {
	tests := []struct {
		name      string
		issuer    string
		insecure  string
		wantErr   string
		wantValue string
	}{
		{"https", "https://idp.example.com/realms/ops", "", "", "https://idp.example.com/realms/ops"},
		{"trailing slash kept", "https://idp.example.com/", "", "", "https://idp.example.com/"},
		{"http with insecure flag", "http://localhost:8081/realms/dev", "true", "", "http://localhost:8081/realms/dev"},
		{"http without insecure flag", "http://idp.example.com", "", "OIDC_ISSUER", ""},
		{"http with insecure flag false", "http://idp.example.com", "false", "OIDC_ISSUER", ""},
		{"relative", "idp.example.com/realms/ops", "", "OIDC_ISSUER", ""},
		{"path only", "/realms/ops", "", "OIDC_ISSUER", ""},
		{"no host", "https:///realms/ops", "", "OIDC_ISSUER", ""},
		{"query", "https://idp.example.com/?tenant=x", "", "OIDC_ISSUER", ""},
		{"empty query marker", "https://idp.example.com/?", "", "OIDC_ISSUER", ""},
		{"fragment", "https://idp.example.com/#x", "", "OIDC_ISSUER", ""},
		{"ftp scheme", "ftp://idp.example.com", "", "OIDC_ISSUER", ""},
		{"insecure flag not a bool", "https://idp.example.com", "yes", "OIDC_INSECURE_HTTP", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := expectErr(t, with(genericOIDC(), "OIDC_ISSUER", tt.issuer, "OIDC_INSECURE_HTTP", tt.insecure), tt.wantErr)
			if tt.wantErr == "" && cfg.OIDC.Issuer != tt.wantValue {
				t.Errorf("Issuer = %q, want %q", cfg.OIDC.Issuer, tt.wantValue)
			}
		})
	}
}

func TestOIDCAlias(t *testing.T) {
	tests := []struct {
		name        string
		env         map[string]string
		wantErr     string
		wantAlias   bool
		wantProfile OIDCProfile
	}{
		{"alias without profile", with(zitadelOIDC(), "AUTH_MODE", "zitadel", "OIDC_PROFILE", ""), "", true, ProfileZitadel},
		{"alias with matching profile", with(zitadelOIDC(), "AUTH_MODE", "zitadel"), "", true, ProfileZitadel},
		{"alias with generic profile", with(zitadelOIDC(), "AUTH_MODE", "zitadel", "OIDC_PROFILE", "generic"), "OIDC_PROFILE", true, ProfileZitadel},
		{"alias with unknown profile", with(zitadelOIDC(), "AUTH_MODE", "zitadel", "OIDC_PROFILE", "foo"), "OIDC_PROFILE", true, ProfileZitadel},
		{"alias without platform org", with(zitadelOIDC(), "AUTH_MODE", "zitadel", "ZITADEL_PLATFORM_ORG_ID", ""), "ZITADEL_PLATFORM_ORG_ID", true, ProfileZitadel},
		{"explicit oidc zitadel", zitadelOIDC(), "", false, ProfileZitadel},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := expectErr(t, tt.env, tt.wantErr)
			if tt.wantErr != "" {
				return
			}
			if cfg.AuthMode != AuthOIDC || cfg.AuthModeAlias != tt.wantAlias || cfg.OIDC.Profile != tt.wantProfile {
				t.Errorf("mode %q alias %v profile %q, want oidc alias %v profile %q", cfg.AuthMode, cfg.AuthModeAlias, cfg.OIDC.Profile, tt.wantAlias, tt.wantProfile)
			}
		})
	}
}

func TestOIDCAlgorithms(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []string
		wantErr string
	}{
		{"default", "", []string{"RS256"}, ""},
		{"all supported", "RS256, ES256,EdDSA", []string{"RS256", "ES256", "EdDSA"}, ""},
		{"unsupported", "HS256", nil, "OIDC_ALGORITHMS"},
		{"none", "none", nil, "OIDC_ALGORITHMS"},
		{"duplicate", "ES256,ES256", nil, "OIDC_ALGORITHMS"},
		{"empty entry", "RS256,,ES256", nil, "OIDC_ALGORITHMS"},
		{"trailing comma", "RS256,", nil, "OIDC_ALGORITHMS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := expectErr(t, with(genericOIDC(), "OIDC_ALGORITHMS", tt.raw), tt.wantErr)
			if tt.wantErr == "" && !slices.Equal(cfg.OIDC.Algorithms, tt.want) {
				t.Errorf("Algorithms = %v, want %v", cfg.OIDC.Algorithms, tt.want)
			}
		})
	}
}

func TestOIDCMaxTokenTTL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr string
	}{
		{"default", "", time.Hour, ""},
		{"fifteen minutes", "15m", 15 * time.Minute, ""},
		{"twelve hours", "12h", 12 * time.Hour, ""},
		{"above twelve hours", "12h1s", 0, "OIDC_MAX_TOKEN_TTL"},
		{"zero", "0s", 0, "OIDC_MAX_TOKEN_TTL"},
		{"negative", "-1h", 0, "OIDC_MAX_TOKEN_TTL"},
		{"not a duration", "soon", 0, "OIDC_MAX_TOKEN_TTL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := expectErr(t, with(genericOIDC(), "OIDC_MAX_TOKEN_TTL", tt.raw), tt.wantErr)
			if tt.wantErr == "" && cfg.OIDC.MaxTokenTTL != tt.want {
				t.Errorf("MaxTokenTTL = %v, want %v", cfg.OIDC.MaxTokenTTL, tt.want)
			}
		})
	}
}

func TestOIDCScopes(t *testing.T) {
	zitadelDefaults := []string{
		"openid", "profile", "offline_access",
		"urn:zitadel:iam:user:resourceowner",
		"urn:zitadel:iam:org:project:id:300000000000001:aud",
		"urn:zitadel:iam:org:projects:roles",
	}
	tests := []struct {
		name    string
		env     map[string]string
		want    []string
		wantErr string
	}{
		{"generic default", genericOIDC(), []string{"openid", "profile", "offline_access"}, ""},
		{"zitadel default with project id", zitadelOIDC(), zitadelDefaults, ""},
		{"explicit space separated", with(genericOIDC(), "OIDC_SCOPES", "openid email"), []string{"openid", "email"}, ""},
		{"explicit comma separated", with(genericOIDC(), "OIDC_SCOPES", "openid,email"), []string{"openid", "email"}, ""},
		{"explicit without openid", with(genericOIDC(), "OIDC_SCOPES", "profile offline_access"), nil, "OIDC_SCOPES"},
		{"explicit only separators", with(genericOIDC(), "OIDC_SCOPES", ", ,"), nil, "OIDC_SCOPES"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := expectErr(t, tt.env, tt.wantErr)
			if tt.wantErr == "" && !slices.Equal(cfg.OIDC.Scopes, tt.want) {
				t.Errorf("Scopes = %v, want %v", cfg.OIDC.Scopes, tt.want)
			}
		})
	}
}

func TestOIDCClientIDsAndFlags(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		check   func(OIDCConfig) bool
		wantErr string
	}{
		{"client ids trimmed", with(genericOIDC(), "OIDC_CLIENT_IDS", " spa , cli "), func(c OIDCConfig) bool {
			return slices.Equal(c.ClientIDs, []string{"spa", "cli"})
		}, ""},
		{"duplicate client id", with(genericOIDC(), "OIDC_CLIENT_IDS", "spa,spa"), nil, "OIDC_CLIENT_IDS"},
		{"empty client id entry", with(genericOIDC(), "OIDC_CLIENT_IDS", "spa,,cli"), nil, "OIDC_CLIENT_IDS"},
		{"require azp default", genericOIDC(), func(c OIDCConfig) bool { return c.RequireAZP }, ""},
		{"require azp false", with(genericOIDC(), "OIDC_REQUIRE_AZP", "false"), func(c OIDCConfig) bool { return !c.RequireAZP }, ""},
		{"require azp not a bool", with(genericOIDC(), "OIDC_REQUIRE_AZP", "no"), nil, "OIDC_REQUIRE_AZP"},
		{"insecure http default off", genericOIDC(), func(c OIDCConfig) bool { return !c.InsecureHTTP }, ""},
		{"audience kept", genericOIDC(), func(c OIDCConfig) bool { return c.Audience == "pulsequorum" }, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := expectErr(t, tt.env, tt.wantErr)
			if tt.wantErr == "" && !tt.check(cfg.OIDC) {
				t.Errorf("unexpected OIDC config: %+v", cfg.OIDC)
			}
		})
	}
}

func TestOIDCRolesGeneric(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantMap map[string]string
		wantErr string
	}{
		{"claim dot path", with(genericOIDC(), "OIDC_ROLES_CLAIM", "realm_access.roles"), nil, ""},
		{"claim empty segment", with(genericOIDC(), "OIDC_ROLES_CLAIM", "realm_access..roles"), nil, "OIDC_ROLES_CLAIM"},
		{"claim leading dot", with(genericOIDC(), "OIDC_ROLES_CLAIM", ".roles"), nil, "OIDC_ROLES_CLAIM"},
		{"claim trailing dot", with(genericOIDC(), "OIDC_ROLES_CLAIM", "roles."), nil, "OIDC_ROLES_CLAIM"},
		{"role map", with(genericOIDC(), "OIDC_ROLE_MAP", "uptime-admins=tenant-admin, ops=tenant-editor,staff=tenant-viewer,platform=platform-admin"), map[string]string{
			"uptime-admins": "tenant-admin", "ops": "tenant-editor", "staff": "tenant-viewer", "platform": "platform-admin",
		}, ""},
		{"role map unknown role", with(genericOIDC(), "OIDC_ROLE_MAP", "ops=superuser"), nil, "OIDC_ROLE_MAP"},
		{"role map unknown role case", with(genericOIDC(), "OIDC_ROLE_MAP", "ops=Tenant-Admin"), nil, "OIDC_ROLE_MAP"},
		{"role map entry without equals", with(genericOIDC(), "OIDC_ROLE_MAP", "ops"), nil, "OIDC_ROLE_MAP"},
		{"role map empty key", with(genericOIDC(), "OIDC_ROLE_MAP", "=tenant-viewer"), nil, "OIDC_ROLE_MAP"},
		{"role map duplicate key", with(genericOIDC(), "OIDC_ROLE_MAP", "ops=tenant-editor,ops=tenant-viewer"), nil, "OIDC_ROLE_MAP"},
		{"role map empty entry", with(genericOIDC(), "OIDC_ROLE_MAP", "ops=tenant-editor,"), nil, "OIDC_ROLE_MAP"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := expectErr(t, tt.env, tt.wantErr)
			if tt.wantErr == "" && !maps.Equal(cfg.OIDC.RoleMap, tt.wantMap) {
				t.Errorf("RoleMap = %v, want %v", cfg.OIDC.RoleMap, tt.wantMap)
			}
		})
	}
}

func TestOIDCProfileSpecificVariables(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{"roles claim with zitadel", with(zitadelOIDC(), "OIDC_ROLES_CLAIM", "roles"), "OIDC_ROLES_CLAIM"},
		{"role map with zitadel", with(zitadelOIDC(), "OIDC_ROLE_MAP", "ops=tenant-editor"), "OIDC_ROLE_MAP"},
		{"tenancy with generic", with(genericOIDC(), "OIDC_TENANCY", "single"), "OIDC_TENANCY"},
		{"platform org with generic", with(genericOIDC(), "ZITADEL_PLATFORM_ORG_ID", "300000000000003"), "ZITADEL_PLATFORM_ORG_ID"},
		{"tenancy multi with zitadel", with(zitadelOIDC(), "OIDC_TENANCY", "multi"), ""},
		{"tenancy invalid with zitadel", with(zitadelOIDC(), "OIDC_TENANCY", "many"), "OIDC_TENANCY"},
		{"roles source with zitadel", with(zitadelOIDC(), "OIDC_ROLES_SOURCE", "access_token"), "OIDC_ROLES_SOURCE"},
		{"roles source userinfo with zitadel", with(zitadelOIDC(), "OIDC_ROLES_SOURCE", "userinfo"), "OIDC_ROLES_SOURCE"},
		{"default role with zitadel", with(zitadelOIDC(), "OIDC_DEFAULT_ROLE", "tenant-viewer"), "OIDC_DEFAULT_ROLE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { expectErr(t, tt.env, tt.wantErr) })
	}
	cfg := expectErr(t, with(zitadelOIDC(), "OIDC_TENANCY", "multi"), "")
	if cfg.OIDC.Tenancy != TenancyMulti || cfg.OIDC.RolesClaim != "" || cfg.OIDC.RoleMap != nil {
		t.Errorf("zitadel multi: %+v", cfg.OIDC)
	}
}

func TestOIDCRolesSource(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    string
		wantErr string
	}{
		{"default", genericOIDC(), RolesSourceAccessToken, ""},
		{"access token", with(genericOIDC(), "OIDC_ROLES_SOURCE", "access_token"), RolesSourceAccessToken, ""},
		{"userinfo", with(genericOIDC(), "OIDC_ROLES_SOURCE", "userinfo"), RolesSourceUserInfo, ""},
		{"unknown value", with(genericOIDC(), "OIDC_ROLES_SOURCE", "id_token"), "", "OIDC_ROLES_SOURCE"},
		{"case sensitive", with(genericOIDC(), "OIDC_ROLES_SOURCE", "UserInfo"), "", "OIDC_ROLES_SOURCE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := expectErr(t, tt.env, tt.wantErr)
			if tt.wantErr == "" && cfg.OIDC.RolesSource != tt.want {
				t.Errorf("RolesSource = %q, want %q", cfg.OIDC.RolesSource, tt.want)
			}
		})
	}
}

func TestOIDCDefaultRole(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    string
		wantErr string
	}{
		{"default empty", genericOIDC(), "", ""},
		{"tenant-admin", with(genericOIDC(), "OIDC_DEFAULT_ROLE", "tenant-admin"), "tenant-admin", ""},
		{"tenant-editor", with(genericOIDC(), "OIDC_DEFAULT_ROLE", "tenant-editor"), "tenant-editor", ""},
		{"tenant-viewer", with(genericOIDC(), "OIDC_DEFAULT_ROLE", "tenant-viewer"), "tenant-viewer", ""},
		{"platform-admin rejected", with(genericOIDC(), "OIDC_DEFAULT_ROLE", "platform-admin"), "", "platform-admin cannot be a default role"},
		{"unknown role", with(genericOIDC(), "OIDC_DEFAULT_ROLE", "superuser"), "", "OIDC_DEFAULT_ROLE"},
		{"case sensitive", with(genericOIDC(), "OIDC_DEFAULT_ROLE", "Tenant-Viewer"), "", "OIDC_DEFAULT_ROLE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := expectErr(t, tt.env, tt.wantErr)
			if tt.wantErr == "" && cfg.OIDC.DefaultRole != tt.want {
				t.Errorf("DefaultRole = %q, want %q", cfg.OIDC.DefaultRole, tt.want)
			}
		})
	}
}

func TestOIDCIgnoredInLocalMode(t *testing.T) {
	cfg := expectErr(t, map[string]string{
		"NODE_NAME":               "n1",
		"OIDC_ISSUER":             "not a url",
		"OIDC_PROFILE":            "keycloak",
		"OIDC_ALGORITHMS":         "HS256",
		"OIDC_TENANCY":            "many",
		"OIDC_MAX_TOKEN_TTL":      "forever",
		"OIDC_ROLES_SOURCE":       "id_token",
		"OIDC_DEFAULT_ROLE":       "platform-admin",
		"ZITADEL_PLATFORM_ORG_ID": "300000000000003",
	}, "")
	if cfg.AuthMode != AuthLocal || !reflect.DeepEqual(cfg.OIDC, OIDCConfig{}) || cfg.AuthModeAlias {
		t.Errorf("local mode: mode %q alias %v OIDC %+v, want zero OIDC", cfg.AuthMode, cfg.AuthModeAlias, cfg.OIDC)
	}
}

func TestLoadRejectsUnknownAuthMode(t *testing.T) {
	expectErr(t, map[string]string{"NODE_NAME": "n1", "AUTH_MODE": "ldap"}, "AUTH_MODE")
}

func TestOIDCAutoProvision(t *testing.T) {
	multi := func(extra ...string) map[string]string {
		return with(zitadelOIDC(), append([]string{"OIDC_TENANCY", "multi"}, extra...)...)
	}
	tests := []struct {
		name     string
		env      map[string]string
		wantErr  string
		wantOn   bool
		wantPlan string
		wantMax  int
	}{
		{name: "off by default", env: zitadelOIDC(), wantOn: false, wantPlan: "default", wantMax: 100},
		{name: "on with zitadel and multi", env: multi("OIDC_AUTO_PROVISION", "true"), wantOn: true, wantPlan: "default", wantMax: 100},
		{name: "plan and max", env: multi("OIDC_AUTO_PROVISION", "true", "OIDC_AUTO_PROVISION_PLAN", "starter", "OIDC_AUTO_PROVISION_MAX", "5"),
			wantOn: true, wantPlan: "starter", wantMax: 5},
		{name: "on with single tenancy", env: with(zitadelOIDC(), "OIDC_AUTO_PROVISION", "true"), wantErr: "OIDC_AUTO_PROVISION"},
		{name: "on with generic profile", env: with(genericOIDC(), "OIDC_AUTO_PROVISION", "true"), wantErr: "OIDC_AUTO_PROVISION"},
		{name: "invalid bool", env: multi("OIDC_AUTO_PROVISION", "yes"), wantErr: "OIDC_AUTO_PROVISION"},
		{name: "max zero", env: multi("OIDC_AUTO_PROVISION", "true", "OIDC_AUTO_PROVISION_MAX", "0"), wantErr: "OIDC_AUTO_PROVISION_MAX"},
		{name: "max not a number", env: multi("OIDC_AUTO_PROVISION", "true", "OIDC_AUTO_PROVISION_MAX", "many"), wantErr: "OIDC_AUTO_PROVISION_MAX"},
		{name: "invalid plan", env: multi("OIDC_AUTO_PROVISION", "true", "OIDC_AUTO_PROVISION_PLAN", "Not Valid"), wantErr: "OIDC_AUTO_PROVISION_PLAN"},
		{name: "system plan reserved", env: multi("OIDC_AUTO_PROVISION", "true", "OIDC_AUTO_PROVISION_PLAN", "system"), wantErr: "OIDC_AUTO_PROVISION_PLAN"},
		{name: "plan without enabling", env: multi("OIDC_AUTO_PROVISION_PLAN", "starter"), wantErr: "OIDC_AUTO_PROVISION_PLAN"},
		{name: "max without enabling", env: multi("OIDC_AUTO_PROVISION_MAX", "5"), wantErr: "OIDC_AUTO_PROVISION_MAX"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := expectErr(t, tt.env, tt.wantErr)
			if tt.wantErr != "" {
				return
			}
			if cfg.OIDC.AutoProvision != tt.wantOn || cfg.OIDC.AutoProvisionPlan != tt.wantPlan || cfg.OIDC.AutoProvisionMax != tt.wantMax {
				t.Errorf("auto provision = %t %q %d, want %t %q %d", cfg.OIDC.AutoProvision, cfg.OIDC.AutoProvisionPlan,
					cfg.OIDC.AutoProvisionMax, tt.wantOn, tt.wantPlan, tt.wantMax)
			}
		})
	}
}

func TestClusterSeeds(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    []string
		wantErr string
	}{
		{name: "unset", value: "", want: nil},
		{name: "host port and https", value: "voter1:9443, https://voter2:9443", want: []string{"voter1:9443", "voter2:9443"}},
		{name: "empty entries skipped", value: " ,, https://a:1 ,,", want: []string{"a:1"}},
		{name: "http rejected", value: "http://voter1:9443", wantErr: "CLUSTER_SEEDS"},
		{name: "other scheme rejected", value: "ftp://voter1:9443", wantErr: "CLUSTER_SEEDS"},
		{name: "empty port rejected", value: "voter1:", wantErr: "CLUSTER_SEEDS"},
		{name: "empty host rejected", value: ":9443", wantErr: "CLUSTER_SEEDS"},
		{name: "no port rejected", value: "voter1", wantErr: "CLUSTER_SEEDS"},
		{name: "path rejected", value: "https://voter1:9443/api", wantErr: "CLUSTER_SEEDS"},
		{name: "query rejected", value: "voter1:9443?x=1", wantErr: "CLUSTER_SEEDS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(env(map[string]string{"NODE_NAME": "n1", "CLUSTER_SEEDS": tt.value}))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(cfg.ClusterSeeds, tt.want) {
				t.Errorf("ClusterSeeds = %v, want %v", cfg.ClusterSeeds, tt.want)
			}
		})
	}
}

func TestClusterJoinTokenAlias(t *testing.T) {
	cfg, err := Load(env(map[string]string{"NODE_NAME": "n1", "CLUSTER_JOIN_TOKEN": "tok-alias"}))
	if err != nil || cfg.JoinToken != "tok-alias" {
		t.Fatalf("CLUSTER_JOIN_TOKEN: token = %q, err = %v; want tok-alias", cfg.JoinToken, err)
	}
	cfg, err = Load(env(map[string]string{
		"NODE_NAME":          "n1",
		"JOIN_TOKEN":         "tok-primary",
		"CLUSTER_JOIN_TOKEN": "tok-alias",
	}))
	if err != nil || cfg.JoinToken != "tok-primary" {
		t.Fatalf("JOIN_TOKEN precedence: token = %q, err = %v; want tok-primary", cfg.JoinToken, err)
	}
	_, err = Load(env(map[string]string{
		"NODE_NAME":          "n1",
		"CLUSTER_JOIN_TOKEN": "tok-alias",
		"JOIN_TOKEN_FILE":    "/run/secrets/join",
	}))
	if err == nil || !strings.Contains(err.Error(), "JOIN_TOKEN_FILE") {
		t.Fatalf("CLUSTER_JOIN_TOKEN + JOIN_TOKEN_FILE: err = %v, want JOIN_TOKEN_FILE error", err)
	}
}

func TestNodeRegion(t *testing.T) {
	maxRegion := strings.Repeat("a", 64)
	cfg, err := Load(env(map[string]string{"NODE_NAME": "n1", "NODE_REGION": " " + maxRegion + " "}))
	if err != nil || cfg.NodeRegion != maxRegion {
		t.Fatalf("64-byte region: region = %q, err = %v", cfg.NodeRegion, err)
	}
	_, err = Load(env(map[string]string{"NODE_NAME": "n1", "NODE_REGION": strings.Repeat("a", 65)}))
	if err == nil || !strings.Contains(err.Error(), "NODE_REGION") {
		t.Fatalf("65-byte region: err = %v, want NODE_REGION error", err)
	}
}

func TestNodeLabels(t *testing.T) {
	var thirtyTwo []string
	want32 := make(map[string]string, 32)
	for i := range 32 {
		k := strings.Repeat("k", 1) + string(rune('a'+i/10)) + string(rune('0'+i%10))
		thirtyTwo = append(thirtyTwo, k+"=v")
		want32[k] = "v"
	}
	thirtyThree := append(slices.Clone(thirtyTwo), "extra=v")

	tests := []struct {
		name    string
		value   string
		want    map[string]string
		wantErr string
	}{
		{name: "unset", value: "", want: nil},
		{name: "trimmed pairs", value: " tier = edge , dc = eu-central-1 , empty= ", want: map[string]string{
			"tier":  "edge",
			"dc":    "eu-central-1",
			"empty": "",
		}},
		{name: "32 labels allowed", value: strings.Join(thirtyTwo, ","), want: want32},
		{name: "33 labels rejected", value: strings.Join(thirtyThree, ","), wantErr: "NODE_LABELS"},
		{name: "missing equals", value: "tier", wantErr: "NODE_LABELS"},
		{name: "empty key", value: "=val", wantErr: "NODE_LABELS"},
		{name: "key too long", value: strings.Repeat("k", 65) + "=v", wantErr: "NODE_LABELS"},
		{name: "value too long", value: "k=" + strings.Repeat("v", 257), wantErr: "NODE_LABELS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(env(map[string]string{"NODE_NAME": "n1", "NODE_LABELS": tt.value}))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !maps.Equal(cfg.NodeLabels, tt.want) {
				t.Errorf("NodeLabels = %v, want %v", cfg.NodeLabels, tt.want)
			}
		})
	}
}

func TestProbeSettings(t *testing.T) {
	// Defaults
	cfg, err := Load(env(map[string]string{"NODE_NAME": "n1"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ProbeBufferMaxAge != 24*time.Hour ||
		cfg.ProbeBufferMaxBytes != 100<<20 ||
		cfg.ProbeAckRetain != 10*time.Minute ||
		cfg.ProbeSessionTimeout != 15*time.Second {
		t.Errorf("unexpected probe defaults: age=%v bytes=%d ack=%v session=%v",
			cfg.ProbeBufferMaxAge, cfg.ProbeBufferMaxBytes, cfg.ProbeAckRetain, cfg.ProbeSessionTimeout)
	}

	// Overrides including decimal MB/GB/KB and binary MiB
	cfg, err = Load(env(map[string]string{
		"NODE_NAME":              "n1",
		"PROBE_BUFFER_MAX_AGE":   "12h",
		"PROBE_BUFFER_MAX_BYTES": "100MB",
		"PROBE_ACK_RETAIN":       "0",
		"PROBE_SESSION_TIMEOUT":  "1s",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ProbeBufferMaxAge != 12*time.Hour ||
		cfg.ProbeBufferMaxBytes != 100_000_000 ||
		cfg.ProbeAckRetain != 0 ||
		cfg.ProbeSessionTimeout != time.Second {
		t.Errorf("unexpected probe overrides: age=%v bytes=%d ack=%v session=%v",
			cfg.ProbeBufferMaxAge, cfg.ProbeBufferMaxBytes, cfg.ProbeAckRetain, cfg.ProbeSessionTimeout)
	}

	// Decimal suffixes KB, MB, GB in parseByteSize
	for _, tc := range []struct {
		raw  string
		want int64
	}{
		{"2KB", 2_000},
		{"3MB", 3_000_000},
		{"1GB", 1_000_000_000},
		{"100MiB", 100 << 20},
	} {
		got, err := parseByteSize(tc.raw)
		if err != nil || got != tc.want {
			t.Errorf("parseByteSize(%q) = %d, %v; want %d", tc.raw, got, err, tc.want)
		}
	}

	// Invalid cases
	invalid := []struct {
		name    string
		key     string
		val     string
		wantErr string
	}{
		{"max age zero", "PROBE_BUFFER_MAX_AGE", "0", "PROBE_BUFFER_MAX_AGE"},
		{"max age negative", "PROBE_BUFFER_MAX_AGE", "-1s", "PROBE_BUFFER_MAX_AGE"},
		{"max age invalid", "PROBE_BUFFER_MAX_AGE", "soon", "PROBE_BUFFER_MAX_AGE"},
		{"max bytes zero", "PROBE_BUFFER_MAX_BYTES", "0", "PROBE_BUFFER_MAX_BYTES"},
		{"max bytes invalid", "PROBE_BUFFER_MAX_BYTES", "huge", "PROBE_BUFFER_MAX_BYTES"},
		{"ack retain negative", "PROBE_ACK_RETAIN", "-1s", "PROBE_ACK_RETAIN"},
		{"ack retain invalid", "PROBE_ACK_RETAIN", "never", "PROBE_ACK_RETAIN"},
		{"session timeout below 1s", "PROBE_SESSION_TIMEOUT", "500ms", "PROBE_SESSION_TIMEOUT"},
		{"session timeout zero", "PROBE_SESSION_TIMEOUT", "0", "PROBE_SESSION_TIMEOUT"},
		{"session timeout invalid", "PROBE_SESSION_TIMEOUT", "soon", "PROBE_SESSION_TIMEOUT"},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(env(map[string]string{"NODE_NAME": "n1", tt.key: tt.val}))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

// ADR 0044: the local policy of a probe is read from four variables.
func TestProbePolicySettings(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg, err := Load(env(map[string]string{"NODE_NAME": "n1"}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.ProbeAllowTargets != nil {
			t.Errorf("ProbeAllowTargets = %v, want nil", cfg.ProbeAllowTargets)
		}
		if cfg.ProbeAllowMethods != nil {
			t.Errorf("ProbeAllowMethods = %v, want nil", cfg.ProbeAllowMethods)
		}
		if cfg.ProbeAllowTypes != nil {
			t.Errorf("ProbeAllowTypes = %v, want nil", cfg.ProbeAllowTypes)
		}
		if cfg.ProbeResultDetail != "full" {
			t.Errorf("ProbeResultDetail = %q, want full", cfg.ProbeResultDetail)
		}
	})

	t.Run("valid values", func(t *testing.T) {
		cfg, err := Load(env(map[string]string{
			"NODE_NAME":                 "n1",
			"PROBE_ALLOW_TARGETS":       "10.0.0.0/8:443,[2001:db8::/48]:443",
			"PROBE_ALLOW_HTTP_METHODS":  "get,head",
			"PROBE_ALLOW_MONITOR_TYPES": "http,tcp",
			"PROBE_RESULT_DETAIL":       "minimal",
		}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := cfg.ProbeAllowTargets.String(); got != "10.0.0.0/8:443,[2001:db8::/48]:443" {
			t.Errorf("ProbeAllowTargets = %q", got)
		}
		if got := cfg.ProbeAllowMethods.String(); got != "GET,HEAD" {
			t.Errorf("ProbeAllowMethods = %q, want GET,HEAD", got)
		}
		if !slices.Equal(cfg.ProbeAllowTypes, []string{"http", "tcp"}) {
			t.Errorf("ProbeAllowTypes = %v", cfg.ProbeAllowTypes)
		}
		if cfg.ProbeResultDetail != "minimal" {
			t.Errorf("ProbeResultDetail = %q, want minimal", cfg.ProbeResultDetail)
		}
	})

	t.Run("any targets", func(t *testing.T) {
		cfg, err := Load(env(map[string]string{"NODE_NAME": "n1", "PROBE_ALLOW_TARGETS": "any"}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.ProbeAllowTargets == nil || !cfg.ProbeAllowTargets.Any() {
			t.Errorf("ProbeAllowTargets = %v, want any", cfg.ProbeAllowTargets)
		}
	})

	invalid := []struct {
		key string
		val string
	}{
		{"PROBE_ALLOW_TARGETS", "example.com"},
		{"PROBE_ALLOW_TARGETS", "10.0.0.5/16"},
		{"PROBE_ALLOW_HTTP_METHODS", "TRACE"},
		{"PROBE_ALLOW_MONITOR_TYPES", "HTTP!"},
		{"PROBE_RESULT_DETAIL", "verbose"},
	}
	for _, tt := range invalid {
		t.Run(tt.key+"="+tt.val, func(t *testing.T) {
			_, err := Load(env(map[string]string{"NODE_NAME": "n1", tt.key: tt.val}))
			if err == nil || !strings.Contains(err.Error(), tt.key) {
				t.Errorf("Load error = %v, want one naming %s", err, tt.key)
			}
		})
	}
}
