// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import (
	"os/exec"
	"strings"
	"testing"
)

// forbiddenDeps lists packages the helper must not link: no SPA, no REST layer,
// no cluster state machine and no SQLite store. The helper only writes files.
var forbiddenDeps = []string{
	"github.com/joinsider/pulsequorum/internal/webui",
	"github.com/joinsider/pulsequorum/internal/api",
	"github.com/joinsider/pulsequorum/internal/cluster/raft",
	"github.com/joinsider/pulsequorum/internal/store/sqlite",
}

// isForbidden reports whether dep is bad or lies below it on a path boundary.
func isForbidden(dep, bad string) bool {
	return dep == bad || strings.HasPrefix(dep, bad+"/")
}

func TestWebconfigBinaryExcludesServerDeps(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not found in PATH")
	}
	// G204: the tool is the go binary from PATH; the arguments are constants of this test.
	out, err := exec.Command(goTool, "list", "-deps", "github.com/joinsider/pulsequorum/cmd/pulsequorum-webconfig").CombinedOutput() //nolint:gosec // G204: see above
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		for _, bad := range forbiddenDeps {
			if isForbidden(dep, bad) {
				t.Errorf("webconfig binary links forbidden package %s (matches %s)", dep, bad)
			}
		}
	}
}
