// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package config

import (
	"log/slog"
	"os"
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
		{"zitadel not yet supported", map[string]string{"AUTH_MODE": "zitadel"}, "not supported yet"},
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
	for _, tt := range []struct {
		setting string
		mode    AuthMode
		want    bool
	}{{"auto", AuthLocal, false}, {"auto", AuthZitadel, true}, {"on", AuthLocal, true}, {"off", AuthZitadel, false}} {
		if got := (Config{SSRFProtection: tt.setting, AuthMode: tt.mode}).SSRFProtectionEnabled(); got != tt.want {
			t.Errorf("%s/%s = %v, want %v", tt.setting, tt.mode, got, tt.want)
		}
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
		cfg.DiskFreeAlert != 0.15 || cfg.WALSizeAlert != 512<<20 {
		t.Errorf("defaults = %v %v %v %v", cfg.ReplicationLagAlert, cfg.ClockSkewAlert, cfg.DiskFreeAlert, cfg.WALSizeAlert)
	}

	cfg, err = Load(env(map[string]string{
		"NODE_NAME": "n1", "REPLICATION_LAG_ALERT": "90s", "CLOCK_SKEW_ALERT": "500ms",
		"DISK_FREE_ALERT": "0.5", "WAL_SIZE_ALERT": "1GiB",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ReplicationLagAlert != 90*time.Second || cfg.ClockSkewAlert != 500*time.Millisecond ||
		cfg.DiskFreeAlert != 0.5 || cfg.WALSizeAlert != 1<<30 {
		t.Errorf("overrides = %v %v %v %v", cfg.ReplicationLagAlert, cfg.ClockSkewAlert, cfg.DiskFreeAlert, cfg.WALSizeAlert)
	}

	invalid := map[string]string{
		"REPLICATION_LAG_ALERT": "0s",
		"CLOCK_SKEW_ALERT":      "-1s",
		"DISK_FREE_ALERT":       "1",
		"WAL_SIZE_ALERT":        "512MB",
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
		{in: "10MB", wantErr: true},
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
