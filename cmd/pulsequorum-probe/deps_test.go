// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import (
	"os/exec"
	"strings"
	"testing"
)

// forbiddenDeps lists packages that the probe binary must not link (ADR 0043).
var forbiddenDeps = []string{
	"github.com/joinsider/pulsequorum/internal/webui",
	"github.com/joinsider/pulsequorum/internal/api",
	"github.com/joinsider/pulsequorum/internal/auth",
	"github.com/joinsider/pulsequorum/internal/alert",
	"github.com/nicholas-fedor/shoutrrr",
	"github.com/coreos/go-oidc/v3",
	"github.com/getkin/kin-openapi",
}

// isForbidden reports whether dep is bad or lies below it. Internal packages
// match on a path boundary (internal/api/gen counts, internal/apiclient does
// not); third-party modules match by prefix.
func isForbidden(dep, bad string) bool {
	if strings.HasPrefix(bad, "github.com/joinsider/") {
		return dep == bad || strings.HasPrefix(dep, bad+"/")
	}
	return strings.HasPrefix(dep, bad)
}

// TestProbeBinaryExcludesVoterDeps is the architecture test of ADR 0043: the
// probe binary must not pull in the web UI, the REST layer, authentication,
// alerting, Shoutrrr, OIDC or OpenAPI code.
func TestProbeBinaryExcludesVoterDeps(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not found in PATH")
	}
	// G204: the tool is the go binary from PATH; the arguments are constants of this test.
	out, err := exec.Command(goTool, "list", "-deps", "github.com/joinsider/pulsequorum/cmd/pulsequorum-probe").CombinedOutput() //nolint:gosec // G204: see above
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		for _, bad := range forbiddenDeps {
			if isForbidden(dep, bad) {
				t.Errorf("probe binary links forbidden package %s (matches %s)", dep, bad)
			}
		}
	}
}
