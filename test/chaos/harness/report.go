// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package harness

import (
	"fmt"
	"strings"
)

// Outcomes of one step.
const (
	OutcomePass = "PASS"
	OutcomeFail = "FAIL"
	OutcomeInfo = "INFO"
)

// Step is one row of a scenario report.
type Step struct {
	Name     string
	Expected string
	Measured string
	Outcome  string
}

// Report collects the steps of one scenario in order.
type Report struct {
	Scenario string
	Title    string
	steps    []Step
}

// NewReport starts the report of a scenario.
func NewReport(scenario, title string) *Report {
	return &Report{Scenario: scenario, Title: title}
}

// Check appends a step that passes or fails.
func (r *Report) Check(name, expected, measured string, pass bool) {
	out := OutcomeFail
	if pass {
		out = OutcomePass
	}
	r.steps = append(r.steps, Step{Name: name, Expected: expected, Measured: measured, Outcome: out})
}

// Info appends an informational step that does not affect the verdict.
func (r *Report) Info(name, expected, measured string) {
	r.steps = append(r.steps, Step{Name: name, Expected: expected, Measured: measured, Outcome: OutcomeInfo})
}

// Steps returns a copy of the steps.
func (r *Report) Steps() []Step {
	return append([]Step(nil), r.steps...)
}

// Failed reports whether any step failed.
func (r *Report) Failed() bool {
	for _, s := range r.steps {
		if s.Outcome == OutcomeFail {
			return true
		}
	}
	return false
}

// Markdown renders the scenario as a heading, a table and a verdict line.
func (r *Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %s: %s\n\n", r.Scenario, r.Title)
	b.WriteString("| Step | Expected | Measured | Result |\n")
	b.WriteString("|------|----------|----------|--------|\n")
	for _, s := range r.steps {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n",
			cell(s.Name), cell(s.Expected), cell(s.Measured), s.Outcome)
	}
	verdict := OutcomePass
	if r.Failed() {
		verdict = OutcomeFail
	}
	fmt.Fprintf(&b, "\n%s overall: %s\n", r.Scenario, verdict)
	return b.String()
}

// Document renders several scenario reports under one heading with an
// overall verdict.
func Document(title string, reports ...*Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", title)
	failed := false
	for _, r := range reports {
		b.WriteString(r.Markdown())
		b.WriteString("\n")
		failed = failed || r.Failed()
	}
	verdict := OutcomePass
	if failed {
		verdict = OutcomeFail
	}
	fmt.Fprintf(&b, "Overall: %s\n", verdict)
	return b.String()
}

// cell makes a value safe for a Markdown table cell.
func cell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.Join(strings.Fields(s), " ")
}
