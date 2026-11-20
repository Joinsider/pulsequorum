// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import (
	"strings"
	"testing"
)

func TestReportMarkdown(t *testing.T) {
	tests := []struct {
		name     string
		steps    func(r *report)
		contains []string
		failed   bool
	}{
		{
			name: "all pass",
			steps: func(r *report) {
				r.Add("Admin", "201", "user admin", true)
				r.AddInfo("UP notification", "informational", "not received")
			},
			contains: []string{"| Admin | 201 | user admin | PASS |", "| UP notification | informational | not received | INFO |", "overall: PASS"},
			failed:   false,
		},
		{
			name: "one failure",
			steps: func(r *report) {
				r.Add("DOWN", "within 70 s", "after 95 s", false)
			},
			contains: []string{"| DOWN | within 70 s | after 95 s | FAIL |", "overall: FAIL"},
			failed:   true,
		},
		{
			name: "pipes and newlines are escaped",
			steps: func(r *report) {
				r.Add("Cause", "a|b", "line one\nline two", true)
			},
			contains: []string{`| Cause | a\|b | line one line two | PASS |`},
			failed:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &report{}
			tt.steps(r)
			md := r.Markdown()
			for _, want := range tt.contains {
				if !strings.Contains(md, want) {
					t.Errorf("markdown lacks %q:\n%s", want, md)
				}
			}
			if r.Failed() != tt.failed {
				t.Errorf("Failed() = %v, want %v", r.Failed(), tt.failed)
			}
		})
	}
}
