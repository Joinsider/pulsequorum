// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package main

import (
	"strconv"
	"strings"
	"testing"
)

func TestReportMarkdown(t *testing.T) {
	tests := []struct {
		name     string
		steps    []bool
		contains []string
		verdict  string
	}{
		{
			name:    "all pass",
			steps:   []bool{true, true},
			verdict: "AS-06 overall: PASS",
			contains: []string{
				"| Step | Expected | Measured | Result |",
				"| step 1 | exp 1 | got 1 | PASS |",
				"| step 2 | exp 2 | got 2 | PASS |",
			},
		},
		{
			name:    "one failure makes the run fail",
			steps:   []bool{true, false},
			verdict: "AS-06 overall: FAIL",
			contains: []string{
				"| step 2 | exp 2 | got 2 | FAIL |",
			},
		},
		{
			name:    "empty report passes",
			steps:   nil,
			verdict: "AS-06 overall: PASS",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkReportCase(t, tt.steps, tt.contains, tt.verdict)
		})
	}
}

// checkReportCase adds one row per step to a report and checks the Markdown
// output and the failure state against the expectations.
func checkReportCase(t *testing.T, steps []bool, contains []string, verdict string) {
	var r report
	for i, pass := range steps {
		n := strconv.Itoa(i + 1)
		r.Add("step "+n, "exp "+n, "got "+n, pass)
	}
	out := r.Markdown()
	for _, want := range contains {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, verdict) {
		t.Errorf("verdict %q missing:\n%s", verdict, out)
	}
	if r.Failed() != strings.Contains(verdict, "FAIL") {
		t.Errorf("Failed() = %v disagrees with verdict %q", r.Failed(), verdict)
	}
}

func TestReportCellEscaping(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "plain", want: "plain"},
		{in: "a|b", want: `a\|b`},
		{in: "line one\nline two", want: "line one line two"},
		{in: "  spaced   out  ", want: "spaced out"},
	}
	for _, tt := range tests {
		if got := cell(tt.in); got != tt.want {
			t.Errorf("cell(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestReportRowKeepsOutcomeText(t *testing.T) {
	var r report
	r.Add("name with | pipe", "e", "m", false)
	out := r.Markdown()
	if !strings.Contains(out, `| name with \| pipe | e | m | FAIL |`) {
		t.Errorf("row not escaped:\n%s", out)
	}
}
