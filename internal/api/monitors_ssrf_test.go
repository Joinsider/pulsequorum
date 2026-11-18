// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package api

import (
	"net/http"
	"testing"

	"github.com/joinsider/uptimecluster/internal/api/gen"
	"github.com/joinsider/uptimecluster/internal/checker"
)

// TestSSRFPolicyOnRejectsBlockedTarget checks that a monitor whose host is
// blocked by the address policy is refused when it is saved (ADR 0030).
func TestSSRFPolicyOnRejectsBlockedTarget(t *testing.T) {
	e := newEnvTweaked(t, false, func(d *Deps) { d.SSRFPolicy = checker.BlockPrivate{} })

	res := e.do("POST", "/api/v1/monitors", editor, httpMonitor("x", "http://169.254.169.254/"))
	if res.code != http.StatusUnprocessableEntity {
		t.Fatalf("create = %d %s", res.code, res.body)
	}
	assertProblemField(t, decode[gen.Problem](t, res), "/config/url")

	if res := e.do("POST", "/api/v1/monitors", editor, httpMonitor("x", "https://example.com/")); res.code != http.StatusCreated {
		t.Errorf("public host: create = %d %s", res.code, res.body)
	}
}

// TestSSRFPolicyOffAcceptsBlockedTarget checks that without a policy the same
// host is accepted, while the notation rules still apply.
func TestSSRFPolicyOffAcceptsBlockedTarget(t *testing.T) {
	e := newEnv(t)

	if res := e.do("POST", "/api/v1/monitors", editor, httpMonitor("x", "http://169.254.169.254/")); res.code != http.StatusCreated {
		t.Errorf("policy off: create = %d %s", res.code, res.body)
	}

	res := e.do("POST", "/api/v1/monitors", editor, httpMonitor("x", "http://0x7f.1/"))
	if res.code != http.StatusUnprocessableEntity {
		t.Fatalf("notation: create = %d %s", res.code, res.body)
	}
	assertProblemField(t, decode[gen.Problem](t, res), "/config/url")
}

// assertProblemField fails unless the problem lists field.
func assertProblemField(t *testing.T, p gen.Problem, field string) {
	t.Helper()
	if p.Errors == nil {
		t.Fatalf("problem has no field errors, want %s", field)
	}
	for _, f := range *p.Errors {
		if f.Field == field {
			return
		}
	}
	t.Errorf("errors = %+v, want field %s", *p.Errors, field)
}
