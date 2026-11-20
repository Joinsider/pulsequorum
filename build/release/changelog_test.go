// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import "testing"

const noChanges = "No user-facing changes.\n"

func TestChangelog(t *testing.T) {
	tests := []struct {
		name    string
		commits []Commit
		want    string
	}{
		{
			name:    "no commits",
			commits: nil,
			want:    noChanges,
		},
		{
			name: "only excluded types and merges",
			commits: []Commit{
				{Hash: "1111111aaa", Subject: "docs: update readme"},
				{Hash: "2222222bbb", Subject: "test(api): cover login"},
				{Hash: "3333333ccc", Subject: "chore(deps): bump x"},
				{Hash: "4444444ddd", Subject: "ci: pin runner"},
				{Hash: "5555555eee", Subject: "style: gofmt"},
				{Hash: "6666666fff", Subject: "build: tweak flags"},
				{Hash: "7777777aaa", Subject: "Merge branch 'main' into feat/x", Body: "BREAKING CHANGE: ignored for merges"},
			},
			want: noChanges,
		},
		{
			name:    "empty input list of commits renders the placeholder",
			commits: []Commit{},
			want:    noChanges,
		},
		{
			name:    "feature with scope",
			commits: []Commit{{Hash: "abcdef1234", Subject: "feat(api): add x"}},
			want:    "### Features\n\n- **api:** add x (abcdef1)\n",
		},
		{
			name:    "feature without scope and hash",
			commits: []Commit{{Subject: "feat: add TCP checker"}},
			want:    "### Features\n\n- add TCP checker\n",
		},
		{
			name:    "type is matched case-insensitively",
			commits: []Commit{{Hash: "abcdef1234", Subject: "FEAT: shout"}},
			want:    "### Features\n\n- shout (abcdef1)\n",
		},
		{
			name:    "fix without security scope is a bug fix",
			commits: []Commit{{Hash: "abcdef1234", Subject: "fix(store): nil check"}},
			want:    "### Bug fixes\n\n- **store:** nil check (abcdef1)\n",
		},
		{
			name:    "fix(security) goes to Security",
			commits: []Commit{{Hash: "abcdef1234", Subject: "fix(security): escape header"}},
			want:    "### Security\n\n- **security:** escape header (abcdef1)\n",
		},
		{
			name:    "sec type goes to Security",
			commits: []Commit{{Hash: "abcdef1234", Subject: "sec: rotate key"}},
			want:    "### Security\n\n- rotate key (abcdef1)\n",
		},
		{
			name:    "security type goes to Security",
			commits: []Commit{{Hash: "abcdef1234", Subject: "security(auth): tighten cookie"}},
			want:    "### Security\n\n- **auth:** tighten cookie (abcdef1)\n",
		},
		{
			name:    "perf goes to Performance",
			commits: []Commit{{Hash: "abcdef1234", Subject: "perf(store): batch writes"}},
			want:    "### Performance\n\n- **store:** batch writes (abcdef1)\n",
		},
		{
			name:    "refactor goes to Refactoring",
			commits: []Commit{{Hash: "abcdef1234", Subject: "refactor: split scheduler"}},
			want:    "### Refactoring\n\n- split scheduler (abcdef1)\n",
		},
		{
			name:    "non-conventional subject goes to Other changes",
			commits: []Commit{{Hash: "abcdef1234", Subject: "Update vendored notices"}},
			want:    "### Other changes\n\n- Update vendored notices (abcdef1)\n",
		},
		{
			name:    "unknown conventional type keeps its full subject under Other changes",
			commits: []Commit{{Hash: "abcdef1234", Subject: "wip(api): half done"}},
			want:    "### Other changes\n\n- wip(api): half done (abcdef1)\n",
		},
		{
			name:    "bang marks breaking",
			commits: []Commit{{Hash: "abcdef1234", Subject: "feat(checker)!: drop legacy ping"}},
			want:    "### Breaking changes\n\n- **checker:** drop legacy ping (abcdef1)\n",
		},
		{
			name:    "bang on fix(security) is breaking, not Security",
			commits: []Commit{{Hash: "abcdef1234", Subject: "fix(security)!: drop weak cipher"}},
			want:    "### Breaking changes\n\n- **security:** drop weak cipher (abcdef1)\n",
		},
		{
			name:    "bang on an excluded type is still listed as breaking",
			commits: []Commit{{Hash: "abcdef1234", Subject: "chore!: drop Go 1.22"}},
			want:    "### Breaking changes\n\n- drop Go 1.22 (abcdef1)\n",
		},
		{
			name: "BREAKING CHANGE footer marks breaking",
			commits: []Commit{{
				Hash:    "abcdef1234",
				Subject: "feat(api): new tokens",
				Body:    "Explains why.\n\nBREAKING CHANGE: tokens are now opaque",
			}},
			want: "### Breaking changes\n\n- **api:** new tokens (abcdef1)\n",
		},
		{
			name: "BREAKING-CHANGE footer with leading spaces marks breaking",
			commits: []Commit{{
				Hash:    "abcdef1234",
				Subject: "fix: old path",
				Body:    "  BREAKING-CHANGE: path removed",
			}},
			want: "### Breaking changes\n\n- old path (abcdef1)\n",
		},
		{
			name: "BREAKING CHANGE on non-conventional subject",
			commits: []Commit{{
				Hash:    "abcdef1234",
				Subject: "Rework auth",
				Body:    "BREAKING CHANGE: sessions expire",
			}},
			want: "### Breaking changes\n\n- Rework auth (abcdef1)\n",
		},
		{
			name: "breaking commit appears only under Breaking changes",
			commits: []Commit{{
				Hash:    "abcdef1234",
				Subject: "fix(api)!: nil handling",
				Body:    "BREAKING CHANGE: strict",
			}},
			want: "### Breaking changes\n\n- **api:** nil handling (abcdef1)\n",
		},
		{
			name: "BREAKING CHANGE mentioned mid-line does not count",
			commits: []Commit{{
				Hash:    "abcdef1234",
				Subject: "fix: something",
				Body:    "This is not a BREAKING CHANGE: just prose",
			}},
			want: "### Bug fixes\n\n- something (abcdef1)\n",
		},
		{
			name: "short hash is kept whole",
			commits: []Commit{{
				Hash:    "abc",
				Subject: "feat: x",
			}},
			want: "### Features\n\n- x (abc)\n",
		},
		{
			name: "groups in order with input order kept inside a group",
			commits: []Commit{
				{Hash: "abcdef1234", Subject: "fix(api): handle nil body"},
				{Hash: "1111111aaa", Subject: "feat: add TCP checker"},
				{Hash: "2222222bbb", Subject: "feat(checker)!: drop legacy ping"},
				{Hash: "3333333ccc", Subject: "docs: update readme"},
				{Hash: "4444444ddd", Subject: "fix(security): escape tenant header"},
				{Hash: "5555555eee", Subject: "perf(store): batch writes"},
				{Hash: "6666666fff", Subject: "refactor: split scheduler"},
				{Hash: "7777777aaa", Subject: "Update vendored notices"},
				{Hash: "8888888bbb", Subject: "Merge branch 'main'"},
				{Hash: "9999999ccc", Subject: "chore(deps): bump x"},
				{Hash: "aaaaaaaddd", Subject: "sec: rotate key"},
				{Hash: "bbbbbbbeee", Subject: "fix(api): x", Body: "BREAKING CHANGE: removed y"},
				{Hash: "cccccccfff", Subject: "feat(api): a"},
			},
			want: "### Breaking changes\n\n" +
				"- **checker:** drop legacy ping (2222222)\n" +
				"- **api:** x (bbbbbbb)\n" +
				"\n" +
				"### Features\n\n" +
				"- add TCP checker (1111111)\n" +
				"- **api:** a (ccccccc)\n" +
				"\n" +
				"### Bug fixes\n\n" +
				"- **api:** handle nil body (abcdef1)\n" +
				"\n" +
				"### Security\n\n" +
				"- **security:** escape tenant header (4444444)\n" +
				"- rotate key (aaaaaaa)\n" +
				"\n" +
				"### Performance\n\n" +
				"- **store:** batch writes (5555555)\n" +
				"\n" +
				"### Refactoring\n\n" +
				"- split scheduler (6666666)\n" +
				"\n" +
				"### Other changes\n\n" +
				"- Update vendored notices (7777777)\n",
		},
		{
			name: "empty groups are omitted",
			commits: []Commit{
				{Hash: "abcdef1234", Subject: "perf: faster"},
				{Hash: "1111111aaa", Subject: "docs: nope"},
			},
			want: "### Performance\n\n- faster (abcdef1)\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Changelog(tt.commits); got != tt.want {
				t.Errorf("Changelog() =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}
