// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import (
	"fmt"
	"strings"
)

// Outcome of one step.
const (
	outcomePass = "PASS"
	outcomeFail = "FAIL"
	outcomeInfo = "INFO"
)

// step is one row of the result table.
type step struct {
	Name     string
	Expected string
	Measured string
	Outcome  string
}

// report collects the steps in order and renders them as a Markdown table.
type report struct {
	steps []step
}

// Add appends a step. pass selects PASS or FAIL.
func (r *report) Add(name, expected, measured string, pass bool) {
	out := outcomeFail
	if pass {
		out = outcomePass
	}
	r.steps = append(r.steps, step{Name: name, Expected: expected, Measured: measured, Outcome: out})
}

// AddInfo appends an informational step that does not affect the result.
func (r *report) AddInfo(name, expected, measured string) {
	r.steps = append(r.steps, step{Name: name, Expected: expected, Measured: measured, Outcome: outcomeInfo})
}

// Failed reports whether any step failed.
func (r *report) Failed() bool {
	for _, s := range r.steps {
		if s.Outcome == outcomeFail {
			return true
		}
	}
	return false
}

// Markdown renders the table and an overall verdict line.
func (r *report) Markdown() string {
	var b strings.Builder
	b.WriteString("| Step | Expected | Measured | Result |\n")
	b.WriteString("|------|----------|----------|--------|\n")
	for _, s := range r.steps {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n",
			cell(s.Name), cell(s.Expected), cell(s.Measured), s.Outcome)
	}
	verdict := "PASS"
	if r.Failed() {
		verdict = "FAIL"
	}
	fmt.Fprintf(&b, "\nAS-01 overall: %s\n", verdict)
	return b.String()
}

// cell makes a value safe for a Markdown table cell.
func cell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.Join(strings.Fields(s), " ")
}
