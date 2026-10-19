// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package checker

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoCheckerBypassesDialer enforces FA-CHK-04: outgoing connections of
// checkers go through Dialer only, so the SSRF policy cannot be bypassed.
func TestNoCheckerBypassesDialer(t *testing.T) {
	forbidden := []string{
		"net.Dial(", "net.DialTimeout(", "net.Dialer{", "net.DialTCP(", "net.DialUDP(",
		"http.DefaultClient", "http.DefaultTransport", "http.Get(", "http.Post(", "http.Head(",
		"tls.Dial(", "tls.DialWithDialer(",
	}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") || path == "dialer.go" {
			return err
		}
		src, err := os.ReadFile(path) //nolint:gosec // G304: walking this package's own sources
		if err != nil {
			return err
		}
		for _, f := range forbidden {
			if strings.Contains(string(src), f) {
				t.Errorf("%s uses %s; open connections through checker.Dialer", path, f)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
