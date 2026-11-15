// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/joinsider/uptimecluster/test/chaos/harness"
	"github.com/joinsider/uptimecluster/test/internal/ucapi"
)

func TestParseOptionsDefaults(t *testing.T) {
	o, err := parseOptions(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.prefix != "ucchaos" || o.image != harness.DefaultImage || o.voters != 3 ||
		o.portBase != 18100 || o.fakeListenBase != 18950 || o.build != "auto" || o.scenarios != "all" {
		t.Errorf("defaults = %+v", o)
	}
	if o.interval != 20*time.Second {
		t.Errorf("interval = %s", o.interval)
	}
}

func TestParseOptionsRejects(t *testing.T) {
	tests := [][]string{
		{"-build", "sometimes"},
		{"-fake-listen-base", "80"},
		{"extra"},
		{"-no-such-flag"},
	}
	for _, args := range tests {
		if _, err := parseOptions(args, io.Discard); err == nil {
			t.Errorf("parseOptions(%v) accepted", args)
		}
	}
}

func TestSelectScenarios(t *testing.T) {
	all, err := selectScenarios("all")
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(all))
	for _, s := range all {
		ids = append(ids, s.ID)
	}
	if strings.Join(ids, ",") != "as02,selftest" {
		t.Errorf("all = %v", ids)
	}

	one, err := selectScenarios(" AS02 , as02")
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].ID != "as02" {
		t.Errorf("selection = %+v, want one unique as02", one)
	}
	if _, err := selectScenarios("nope"); err == nil {
		t.Error("unknown scenario accepted")
	}
	if _, err := selectScenarios(" , "); err == nil {
		t.Error("empty selection accepted")
	}
}

func TestRegisterRejectsDuplicates(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("duplicate registration did not panic")
		}
	}()
	register(Scenario{ID: "as02", Run: func(context.Context, *Env) error { return nil }})
}

func TestRealMainListsScenarios(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := realMain([]string{"-list"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "as02") || !strings.Contains(out.String(), "selftest") {
		t.Errorf("list = %q", out.String())
	}
}

func TestRealMainRejectsUnknownScenario(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := realMain([]string{"-scenarios", "bogus"}, &out, &errOut); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
}

func TestEnvURLs(t *testing.T) {
	cfg := harness.DefaultConfig()
	cfg.CallbackHost = "host.containers.internal"
	env := &Env{Cfg: cfg, NtfyPort: 18950, TargetPort: 18951}
	if got := env.NtfyURL("as02-ab"); got != "ntfy://host.containers.internal:18950/as02-ab?scheme=http" {
		t.Errorf("NtfyURL = %q", got)
	}
	if got := env.TargetURL("/x/y"); got != "http://host.containers.internal:18951/x/y" {
		t.Errorf("TargetURL = %q", got)
	}
}

func TestReportLabel(t *testing.T) {
	if got := (Scenario{ID: "as02", Label: "AS-02"}).reportLabel(); got != "AS-02" {
		t.Errorf("label = %q", got)
	}
	if got := (Scenario{ID: "xyz"}).reportLabel(); got != "XYZ" {
		t.Errorf("default label = %q", got)
	}
}

func TestPollUntil(t *testing.T) {
	n := 0
	ok, err := pollUntil(context.Background(), time.Second, time.Millisecond, func() (bool, error) {
		n++
		return n == 3, nil
	})
	if !ok || err != nil || n != 3 {
		t.Errorf("pollUntil = %v, %v after %d calls", ok, err, n)
	}

	ok, _ = pollUntil(context.Background(), 20*time.Millisecond, time.Millisecond, func() (bool, error) {
		return false, nil
	})
	if ok {
		t.Error("pollUntil succeeded without done")
	}
}

func TestMedianDuration(t *testing.T) {
	got := medianDuration([]time.Duration{300, 100, 200, 500, 400})
	if got != 300 {
		t.Errorf("median = %v, want 300", got)
	}
	if medianDuration(nil) != 0 {
		t.Error("median of nothing is not 0")
	}
}

func TestRandomSuffixDiffers(t *testing.T) {
	a, b := randomSuffix(), randomSuffix()
	if len(a) != 8 || a == b {
		t.Errorf("suffixes %q, %q", a, b)
	}
}

func TestDescribeLag(t *testing.T) {
	lag := int64(90000)
	got := describeLag(memberNode(&lag, "boom"))
	if !strings.Contains(got, "lag 1m30s") || !strings.Contains(got, "error: boom") {
		t.Errorf("describeLag = %q", got)
	}
}

// memberNode returns a member with the given replication figures.
func memberNode(lag *int64, errText string) ucapi.ClusterNode {
	return ucapi.ClusterNode{ID: "n3", ReplicationLagMs: lag, ReplicationError: errText}
}
