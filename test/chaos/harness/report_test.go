// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package harness

import (
	"strings"
	"testing"
)

func TestReportMarkdownAndVerdict(t *testing.T) {
	r := NewReport("AS-02", "Join two voters")
	r.Check("Voters listed", "3 voters", "3 voters", true)
	r.Info("Join duration", "informational", "42 s")
	r.Check("Gap", "<= 40 s", "55 s", false)

	md := r.Markdown()
	for _, want := range []string{
		"## AS-02: Join two voters",
		"| Step | Expected | Measured | Result |",
		"| Voters listed | 3 voters | 3 voters | PASS |",
		"| Join duration | informational | 42 s | INFO |",
		"| Gap | <= 40 s | 55 s | FAIL |",
		"AS-02 overall: FAIL",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
	if !r.Failed() {
		t.Error("Failed() = false with a failed step")
	}
}

func TestReportEscapesCells(t *testing.T) {
	r := NewReport("S", "t")
	r.Check("a|b", "x\ny", "z", true)
	md := r.Markdown()
	if !strings.Contains(md, `| a\|b | x y | z | PASS |`) {
		t.Errorf("cell not escaped:\n%s", md)
	}
}

func TestDocumentOverallVerdict(t *testing.T) {
	ok := NewReport("AS-02", "one")
	ok.Check("s", "e", "m", true)
	bad := NewReport("SELFTEST", "two")
	bad.Check("s", "e", "m", false)

	doc := Document("Chaos run", ok, bad)
	if !strings.HasPrefix(doc, "# Chaos run\n") || !strings.HasSuffix(strings.TrimSpace(doc), "Overall: FAIL") {
		t.Errorf("document:\n%s", doc)
	}
	if !strings.Contains(Document("x", ok), "Overall: PASS") {
		t.Error("all-pass document not PASS")
	}
}
