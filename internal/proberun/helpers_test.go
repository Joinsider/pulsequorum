// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package proberun

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joinsider/pulsequorum/internal/checker/icmpcheck"
	"github.com/joinsider/pulsequorum/internal/config"
	"github.com/joinsider/pulsequorum/internal/store/sqlite"
)

func TestReadJoinToken(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("  secret-token\n\t"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		cfg     config.Config
		want    string
		wantErr string
	}{
		{name: "inline token", cfg: config.Config{JoinToken: "inline-token"}, want: "inline-token"},
		{name: "no token configured", cfg: config.Config{}, want: ""},
		{name: "token file is trimmed", cfg: config.Config{JoinTokenFile: tokenFile}, want: "secret-token"},
		{
			name:    "missing token file",
			cfg:     config.Config{JoinTokenFile: filepath.Join(dir, "missing")},
			wantErr: "JOIN_TOKEN_FILE",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadJoinToken(tc.cfg)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ReadJoinToken error = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadJoinToken: %v", err)
			}
			if got != tc.want {
				t.Errorf("ReadJoinToken = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDBFileSizes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	metrics, err := sqlite.OpenMetrics(ctx, dir, sqlite.MetricsOptions{
		ReadConns:    1,
		CacheSizeKiB: sqlite.ProbeMetricsCacheKiB,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metrics.Close() })

	db := metrics.DB()
	sizes := DBFileSizes(db)

	info, err := os.Stat(db.Path())
	if err != nil {
		t.Fatalf("stat metrics database: %v", err)
	}
	if sizes.Size != info.Size() {
		t.Errorf("DBFileSizes().Size = %d, want %d (size of %s)", sizes.Size, info.Size(), db.Path())
	}
	if sizes.Size <= 0 {
		t.Errorf("DBFileSizes().Size = %d, want a migrated database larger than 0", sizes.Size)
	}
	if sizes.WAL < 0 {
		t.Errorf("DBFileSizes().WAL = %d, want >= 0", sizes.WAL)
	}
}

func TestDBFileSizesMissingFileCountsAsZero(t *testing.T) {
	ctx := context.Background()
	metrics, err := sqlite.OpenMetrics(ctx, t.TempDir(), sqlite.MetricsOptions{ReadConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metrics.Close() })

	db := metrics.DB()
	if err := os.Remove(db.Path()); err != nil {
		t.Fatalf("remove metrics database: %v", err)
	}
	if sizes := DBFileSizes(db); sizes.Size != 0 {
		t.Errorf("DBFileSizes().Size = %d after the file vanished, want 0", sizes.Size)
	}
}

func TestProberunHelpersAndEarlyErrors(t *testing.T) {
	if got := policyString(""); got != "unrestricted" {
		t.Fatalf("policyString(\"\") = %q, want unrestricted", got)
	}
	if got := policyString("GET,HEAD"); got != "GET,HEAD" {
		t.Fatalf("policyString(\"GET,HEAD\") = %q", got)
	}

	rec := httptest.NewRecorder()
	writeProbeJSON(rec, http.StatusOK, map[string]string{"status": "ok"})
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("writeProbeJSON response = %d %v", rec.Code, rec.Header())
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mode, caps := detectProbeCapabilities(context.Background(), config.Config{ICMPMode: icmpcheck.ModeOff}, log)
	if mode != icmpcheck.ModeOff || len(caps) == 0 {
		t.Fatalf("detectProbeCapabilities(off) = (%q, %v)", mode, caps)
	}

	// Run fails early when DataDir is a regular file.
	fileAsDir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(fileAsDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), config.Config{DataDir: fileAsDir}, log); err == nil {
		t.Fatal("expected error when DataDir is a file")
	}

	// Run fails early when JoinTokenFile does not exist.
	if err := Run(context.Background(), config.Config{
		DataDir:       t.TempDir(),
		ICMPMode:      icmpcheck.ModeOff,
		JoinTokenFile: filepath.Join(t.TempDir(), "missing-token"),
	}, log); err == nil {
		t.Fatal("expected error when JoinTokenFile is missing")
	}

	// Run fails early when MetricsTokenFile does not exist.
	if err := Run(context.Background(), config.Config{
		DataDir:          t.TempDir(),
		ICMPMode:         icmpcheck.ModeOff,
		MetricsTokenFile: filepath.Join(t.TempDir(), "missing-metrics-token"),
	}, log); err == nil {
		t.Fatal("expected error when MetricsTokenFile is missing")
	}

	// Run fails early when no probe certificate or join token exists.
	if err := Run(context.Background(), config.Config{
		DataDir:  t.TempDir(),
		ICMPMode: icmpcheck.ModeOff,
	}, log); err == nil {
		t.Fatal("expected error when probe has no certificate and no token")
	}
}
