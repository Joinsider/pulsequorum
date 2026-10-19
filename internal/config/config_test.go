// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package config

import (
	"log/slog"
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
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"NODE_NAME":        "probe-1",
		"NODE_ROLE":        "probe",
		"PUBLIC_URL":       "https://mon1.example.com/",
		"LOG_LEVEL":        "debug",
		"LOG_FORMAT":       "text",
		"SHUTDOWN_TIMEOUT": "3s",
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
