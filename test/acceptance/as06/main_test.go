// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package main

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tokenFiles writes the token files used by the option tests into a fresh
// directory and returns their paths.
func tokenFiles(t *testing.T) (tokA, tokB, empty, missing string) {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	return write("a.tok", "  tok-a-secret\n"), write("b.tok", "tok-b-secret"),
		write("empty.tok", " \n"), filepath.Join(dir, "missing.tok")
}

func TestParseOptions(t *testing.T) {
	tokA, tokB, empty, missing := tokenFiles(t)
	tests := []struct {
		name    string
		args    []string
		wantErr bool
		check   func(t *testing.T, o options)
	}{
		{
			name: "defaults with two token files",
			args: []string{"-token-a-file", tokA, "-token-b-file", tokB},
			check: func(t *testing.T, o options) {
				if o.base != "http://127.0.0.1:8080" {
					t.Errorf("base = %q", o.base)
				}
				if o.tokenA != "tok-a-secret" || o.tokenB != "tok-b-secret" {
					t.Errorf("tokens not read and trimmed: %q %q", o.tokenA, o.tokenB)
				}
				if o.eventWait != 90*time.Second {
					t.Errorf("eventWait = %s", o.eventWait)
				}
			},
		},
		{
			name: "custom base and event wait",
			args: []string{"-base", "https://node.example.test:8443", "-event-wait", "120",
				"-token-a-file", tokA, "-token-b-file", tokB},
			check: func(t *testing.T, o options) {
				if o.base != "https://node.example.test:8443" || o.eventWait != 120*time.Second {
					t.Errorf("base %q, eventWait %s", o.base, o.eventWait)
				}
			},
		},
		{name: "token A flag missing", args: []string{"-token-b-file", tokB}, wantErr: true},
		{name: "token B flag missing", args: []string{"-token-a-file", tokA}, wantErr: true},
		{name: "token A file missing", args: []string{"-token-a-file", missing, "-token-b-file", tokB}, wantErr: true},
		{name: "token B file missing", args: []string{"-token-a-file", tokA, "-token-b-file", missing}, wantErr: true},
		{name: "token file empty", args: []string{"-token-a-file", empty, "-token-b-file", tokB}, wantErr: true},
		{name: "relative base URL", args: []string{"-base", "127.0.0.1:8080", "-token-a-file", tokA, "-token-b-file", tokB}, wantErr: true},
		{name: "event wait below range", args: []string{"-event-wait", "5", "-token-a-file", tokA, "-token-b-file", tokB}, wantErr: true},
		{name: "event wait above range", args: []string{"-event-wait", "601", "-token-a-file", tokA, "-token-b-file", tokB}, wantErr: true},
		{name: "positional argument", args: []string{"-token-a-file", tokA, "-token-b-file", tokB, "extra"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runParseCase(t, tt.args, tt.wantErr, tt.check)
		})
	}
}

// runParseCase parses args and checks the error outcome. On success the parsed
// options are passed to check, if it is set.
func runParseCase(t *testing.T, args []string, wantErr bool, check func(t *testing.T, o options)) {
	t.Helper()
	o, err := parseOptions(args, io.Discard)
	if (err != nil) != wantErr {
		t.Fatalf("err = %v, wantErr %v", err, wantErr)
	}
	if check != nil && err == nil {
		check(t, o)
	}
}

func TestRealMainUsageErrorsDoNotEchoTokens(t *testing.T) {
	tokA, _, _, missing := tokenFiles(t)
	var stdout, stderr bytes.Buffer
	code := realMain([]string{"-base", "not-a-url", "-token-a-file", tokA, "-token-b-file", missing}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit status = %d, want 2", code)
	}
	if strings.Contains(stderr.String(), "tok-a-secret") || strings.Contains(stdout.String(), "tok-a-secret") {
		t.Error("token printed on a usage error")
	}
}

// newTestScenario returns a scenario with fixed names for the judge tests.
func newTestScenario(t *testing.T) (*scenario, *report) {
	t.Helper()
	rep := &report{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := newScenario(options{base: "http://127.0.0.1:1", tokenA: "tok-a-secret", tokenB: "tok-b-secret"}, rep, log)
	s.suffix = "abcd1234"
	s.monitorID = "11111111-2222-3333-4444-555555555555"
	s.monitorName = "as06-http-abcd1234"
	s.channelID = "22222222-2222-3333-4444-555555555555"
	s.channelName = "as06-hook-abcd1234"
	s.maintName = "as06-maint-abcd1234"
	s.tokenName = "as06-token-abcd1234"
	s.tenantA = "tenant-acme-1"
	return s, rep
}

// judgeCase is one row of TestJudgeProbe.
type judgeCase struct {
	name     string
	p        probe
	status   int
	raw      string
	err      error
	wantPass bool
	wantText string
}

func TestJudgeProbe(t *testing.T) {
	notFound := probe{name: "read", denied: []int{404}}
	bulk := probe{name: "bulk", denied: []int{404, 422}}
	incidents := probe{name: "incidents", denied: []int{404}, emptyOK: true}
	const cleanBody = `{"title":"Not Found","detail":"monitor not found"}`

	tests := []judgeCase{
		{name: "404 without A data passes", p: notFound, status: 404, raw: cleanBody, wantPass: true},
		{name: "422 accepted for bulk", p: bulk, status: 422, raw: cleanBody, wantPass: true},
		{name: "404 leaking the monitor name fails", p: notFound, status: 404,
			raw: `{"detail":"as06-http-abcd1234 not found"}`, wantText: "leaks monitor name"},
		{name: "404 leaking the tenant ID fails", p: notFound, status: 404,
			raw: `{"detail":"tenant-acme-1"}`, wantText: "leaks tenant ID"},
		{name: "200 where a denial is required fails", p: notFound, status: 200, raw: `{"id":"x"}`,
			wantText: "request accepted"},
		{name: "incident list without items passes", p: incidents, status: 200, raw: `{"items":[]}`, wantPass: true},
		{name: "incident list with items fails", p: incidents, status: 200,
			raw: `{"items":[{"monitorName":"as06-http-abcd1234"}]}`, wantText: "leaks monitor name"},
		{name: "transport error fails", p: notFound, status: 0, err: errors.New("connection refused"),
			wantText: "no answer"},
		{name: "500 is not a denial", p: notFound, status: http.StatusInternalServerError, raw: cleanBody,
			wantText: "not a denial"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkJudgeCase(t, tt)
		})
	}
}

// checkJudgeCase runs judgeProbe for one case and checks the single row it adds.
func checkJudgeCase(t *testing.T, tt judgeCase) {
	s, rep := newTestScenario(t)
	s.judgeProbe(tt.p, tt.status, []byte(tt.raw), tt.err, s.aNeedles(false))
	if len(rep.steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(rep.steps))
	}
	got := rep.steps[0]
	if (got.Outcome == outcomePass) != tt.wantPass {
		t.Errorf("outcome = %s (measured %q), wantPass %v", got.Outcome, got.Measured, tt.wantPass)
	}
	if tt.wantText != "" && !strings.Contains(got.Measured, tt.wantText) {
		t.Errorf("measured = %q, want it to contain %q", got.Measured, tt.wantText)
	}
}

func TestTargetsUseReservedDomain(t *testing.T) {
	// The monitors and channels of the scenario must not reach a real host.
	for _, raw := range []string{monitorURL, channelURL} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		if !strings.HasSuffix(u.Hostname(), ".invalid") {
			t.Errorf("target %q is not in the reserved .invalid domain", raw)
		}
	}
}

func TestRedactRemovesTokensFromRows(t *testing.T) {
	s, rep := newTestScenario(t)
	s.row("row", "expected", "failed with tok-a-secret in text", false)
	if strings.Contains(rep.Markdown(), "tok-a-secret") {
		t.Error("token printed in the report")
	}
	if !strings.Contains(rep.Markdown(), "<redacted>") {
		t.Error("token not replaced")
	}
}
