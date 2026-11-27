// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func frontendEnv(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

func TestRunFrontendHeaders(t *testing.T) {
	env := frontendEnv(map[string]string{"FRONTEND_NODES": "https://n1.example.com,https://n2.example.com"})
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  []string
	}{
		{
			name:     "netlify is the default",
			args:     []string{"headers"},
			wantCode: 0,
			wantOut:  []string{"/*\n", "  Content-Security-Policy: ", "https://n1.example.com", "/config.json\n  Cache-Control: no-store\n"},
		},
		{
			name:     "netlify explicit",
			args:     []string{"headers", "--format", "netlify"},
			wantCode: 0,
			wantOut:  []string{"/*\n", "  X-Frame-Options: DENY\n"},
		},
		{
			name:     "nginx",
			args:     []string{"headers", "--format=nginx"},
			wantCode: 0,
			wantOut:  []string{`add_header Content-Security-Policy "`, `add_header X-Frame-Options "DENY" always;`, "wss://n2.example.com"},
		},
		{
			name:     "unknown format",
			args:     []string{"headers", "--format", "apache"},
			wantCode: 2,
		},
		{
			name:     "unexpected argument",
			args:     []string{"headers", "extra"},
			wantCode: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runFrontend(context.Background(), tt.args, env, &stdout, &stderr)
			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", code, tt.wantCode, stderr.String())
			}
			for _, want := range tt.wantOut {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("stdout does not contain %q:\n%s", want, stdout.String())
				}
			}
		})
	}
}

func TestRunFrontendInvalidConfig(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		args []string
	}{
		{name: "missing nodes serve", env: map[string]string{}, args: nil},
		{name: "missing nodes headers", env: map[string]string{}, args: []string{"headers"}},
		{name: "invalid node", env: map[string]string{"FRONTEND_NODES": "http://[bad"}, args: nil},
		{name: "oidc without issuer", env: map[string]string{"FRONTEND_NODES": "https://n1.example.com", "AUTH_MODE": "oidc"}, args: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runFrontend(context.Background(), tt.args, frontendEnv(tt.env), &stdout, &stderr)
			if code != 2 {
				t.Fatalf("exit code = %d, want 2", code)
			}
			if !strings.HasPrefix(stderr.String(), "invalid configuration:\n") {
				t.Errorf("stderr = %q, want prefix %q", stderr.String(), "invalid configuration:\n")
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
		})
	}
}

func TestRunFrontendHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runFrontend(context.Background(), []string{"--help"}, frontendEnv(nil), &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	for _, want := range []string{"FRONTEND_NODES", "AUTH_MODE", "OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_SCOPES", "FRONTEND_LISTEN", "FRONTEND_TLS_CERT", "FRONTEND_TLS_KEY", "FRONTEND_HSTS", "headers"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("help does not mention %s", want)
		}
	}
}
