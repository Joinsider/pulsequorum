// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mapEnv(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

func TestRunWritesConfigAndHeaders(t *testing.T) {
	dir := t.TempDir()
	headers := filepath.Join(dir, "headers.conf")
	// An existing file is replaced, not appended to.
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := mapEnv(map[string]string{
		"FRONTEND_NODES":    "https://n1.example.com",
		"FRONTEND_TLS_CERT": "/c.pem",
		"FRONTEND_TLS_KEY":  "/k.pem",
	})

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--web-root", dir, "--headers", headers}, env, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, stderr.String())
	}

	cfgBytes, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Nodes []string `json:"nodes"`
		Auth  struct {
			Mode string `json:"mode"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(cfgBytes, &cfg); err != nil {
		t.Fatalf("config.json is not JSON: %v\n%s", err, cfgBytes)
	}
	if len(cfg.Nodes) != 1 || cfg.Nodes[0] != "https://n1.example.com" || cfg.Auth.Mode != "local" {
		t.Errorf("config.json = %s", cfgBytes)
	}

	hdrBytes, err := os.ReadFile(headers)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`add_header Content-Security-Policy "`, `add_header Strict-Transport-Security "max-age=31536000" always;`} {
		if !strings.Contains(string(hdrBytes), want) {
			t.Errorf("headers file does not contain %q:\n%s", want, hdrBytes)
		}
	}

	for _, p := range []string{filepath.Join(dir, "config.json"), headers} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %o, want 644", p, perm)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestRunInvalidConfigWritesNothing(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{name: "missing nodes", env: map[string]string{}},
		{name: "invalid node", env: map[string]string{"FRONTEND_NODES": "https://n1.example.com/path"}},
		{name: "oidc without client", env: map[string]string{"FRONTEND_NODES": "https://n1.example.com", "AUTH_MODE": "oidc", "OIDC_ISSUER": "https://idp.example.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			headers := filepath.Join(dir, "headers.conf")
			var stdout, stderr bytes.Buffer
			code := run([]string{"--web-root", dir, "--headers", headers}, mapEnv(tt.env), &stdout, &stderr)
			if code != 1 {
				t.Fatalf("exit code = %d, want 1", code)
			}
			if !strings.HasPrefix(stderr.String(), "invalid configuration:") {
				t.Errorf("stderr = %q", stderr.String())
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("files written for invalid config: %v", entries)
			}
		})
	}
}

func TestRunWriteErrorExitsOne(t *testing.T) {
	env := mapEnv(map[string]string{"FRONTEND_NODES": "https://n1.example.com"})
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--web-root", missing, "--headers", filepath.Join(dir, "h.conf")}, env, &stdout, &stderr); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}

func TestRunUsageErrors(t *testing.T) {
	env := mapEnv(map[string]string{"FRONTEND_NODES": "https://n1.example.com"})
	tests := []struct {
		name string
		args []string
	}{
		{name: "no flags", args: nil},
		{name: "missing headers", args: []string{"--web-root", t.TempDir()}},
		{name: "missing web root", args: []string{"--headers", filepath.Join(t.TempDir(), "h.conf")}},
		{name: "unknown flag", args: []string{"--bogus"}},
		{name: "positional argument", args: []string{"extra"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tt.args, env, &stdout, &stderr); code != 2 {
				t.Fatalf("exit code = %d, want 2 (stderr: %s)", code, stderr.String())
			}
		})
	}
}

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, mapEnv(nil), &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.HasPrefix(stdout.String(), "pulsequorum-webconfig ") {
		t.Errorf("version output = %q", stdout.String())
	}
}
