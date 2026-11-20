// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package admincli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joinsider/pulsequorum/internal/config"
)

// TestReadNewPasswordFromFile covers --password-file: the trailing line break
// goes, everything else stays.
func TestReadNewPasswordFromFile(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"trailing newline is stripped", "correct horse battery\n", "correct horse battery"},
		{"trailing CRLF is stripped", "correct horse battery\r\n", "correct horse battery"},
		{"inner spaces are kept", "with  two   inner spaces\n", "with  two   inner spaces"},
		{"trailing space is kept", "trailing space \n", "trailing space "},
		{"no line break at all", "no-newline-password", "no-newline-password"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "pw")
			if err := os.WriteFile(file, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := readNewPassword(Env{Stdin: strings.NewReader("ignored\n")}, file)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("password = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadNewPasswordMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := readNewPassword(Env{}, missing); err == nil {
		t.Fatal("missing password file: no error")
	}
}

// TestReadNewPasswordFromStdin covers a non-terminal stdin (pipes, scripts):
// only the first line is the password, and a CRLF line break is stripped.
func TestReadNewPasswordFromStdin(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"first line only", "first line\nsecond line\n", "first line"},
		{"CRLF is stripped", "first line\r\nsecond line\r\n", "first line"},
		{"inner spaces are kept", "two  spaces inside\n", "two  spaces inside"},
		{"no line break", "only-line", "only-line"},
		{"empty input", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readNewPassword(Env{Stdin: strings.NewReader(tc.input)}, "")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("password = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResetPasswordNeedsLocalAuth: with another AUTH_MODE the command fails
// before it opens or changes anything in the data directory.
func TestResetPasswordNeedsLocalAuth(t *testing.T) {
	dir := t.TempDir()
	before := dirEntries(t, dir)
	var stdout, stderr bytes.Buffer
	env := Env{Getenv: func(string) string { return "" }, Stdin: strings.NewReader("x"), Stdout: &stdout, Stderr: &stderr}
	cfg := config.Config{AuthMode: config.AuthZitadel, DataDir: dir}

	err := runResetPassword(context.Background(), cfg, []string{"--username", "admin"}, env)
	if err == nil || !strings.Contains(err.Error(), "AUTH_MODE=local") {
		t.Fatalf("err = %v, want the AUTH_MODE=local hint", err)
	}
	if after := dirEntries(t, dir); len(after) != len(before) {
		t.Errorf("data directory changed: %v -> %v", before, after)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", stdout.String())
	}
}

// The configuration check of Run rejects AUTH_MODE=zitadel before the command
// starts, so the operator sees exit 2 with the configuration error.
func TestResetPasswordZitadelConfigRejectedByRun(t *testing.T) {
	dir := t.TempDir()
	vars := map[string]string{"DATA_DIR": dir, "NODE_NAME": "node1", "AUTH_MODE": "zitadel"}
	var stdout, stderr bytes.Buffer
	env := Env{Getenv: func(k string) string { return vars[k] }, Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr}
	if code := Run(context.Background(), []string{"reset-password", "--username", "admin"}, env); code != ExitUsage {
		t.Fatalf("exit %d, want %d; stderr %q", code, ExitUsage, stderr.String())
	}
	if !strings.Contains(stderr.String(), "invalid configuration") {
		t.Errorf("stderr lacks the configuration error: %q", stderr.String())
	}
}

// -h prints the usage and exits 0 without touching the data directory.
func TestResetPasswordHelp(t *testing.T) {
	dir := t.TempDir()
	before := dirEntries(t, dir)
	code, out, errOut := runCLI(t, dir, "reset-password", "-h")
	if code != ExitOK {
		t.Fatalf("exit %d, want %d; stderr %q", code, ExitOK, errOut)
	}
	if !strings.Contains(errOut, "--username") || !strings.Contains(errOut, "--password-file") {
		t.Errorf("usage lacks the flags: %q", errOut)
	}
	if out != "" {
		t.Errorf("stdout = %q, want nothing", out)
	}
	if after := dirEntries(t, dir); len(after) != len(before) {
		t.Errorf("help changed the data directory: %v -> %v", before, after)
	}
}
