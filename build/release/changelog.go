// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import (
	"fmt"
	"regexp"
	"strings"
)

// Commit is the subset of a git commit needed to build a changelog.
type Commit struct {
	Hash    string
	Subject string
	Body    string
}

// conventionalPattern splits "type(scope)!: description".
var conventionalPattern = regexp.MustCompile(`^([A-Za-z]+)(?:\(([^()]*)\))?(!)?: (.+)$`)

// Indexes into groupHeadings, in output order.
const (
	groupBreaking = iota
	groupFeatures
	groupBugFixes
	groupSecurity
	groupPerformance
	groupRefactoring
	groupOther
	numGroups
)

var groupHeadings = [numGroups]string{
	groupBreaking:    "Breaking changes",
	groupFeatures:    "Features",
	groupBugFixes:    "Bug fixes",
	groupSecurity:    "Security",
	groupPerformance: "Performance",
	groupRefactoring: "Refactoring",
	groupOther:       "Other changes",
}

// changelogEntry is one rendered line of the changelog.
type changelogEntry struct {
	scope       string
	description string
	hash        string
}

func (e changelogEntry) line() string {
	var b strings.Builder
	b.WriteString("- ")
	if e.scope != "" {
		fmt.Fprintf(&b, "**%s:** ", e.scope)
	}
	b.WriteString(e.description)
	if e.hash != "" {
		fmt.Fprintf(&b, " (%s)", e.hash)
	}
	return b.String()
}

// Changelog renders commits as Markdown grouped by Conventional Commits type.
// Commits of type docs, test, chore, ci, style and build, and merge commits
// ("Merge " prefix), are left out unless they are breaking changes, which are
// always listed under "Breaking changes". Input order is kept within a group.
// It returns "No user-facing changes.\n" if nothing remains.
func Changelog(commits []Commit) string {
	var groups [numGroups][]changelogEntry
	for _, c := range commits {
		idx, entry, ok := classifyCommit(c)
		if !ok {
			continue
		}
		groups[idx] = append(groups[idx], entry)
	}

	var b strings.Builder
	for idx, entries := range groups {
		if len(entries) == 0 {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "### %s\n\n", groupHeadings[idx])
		for _, e := range entries {
			b.WriteString(e.line())
			b.WriteString("\n")
		}
	}
	if b.Len() == 0 {
		return "No user-facing changes.\n"
	}
	return b.String()
}

// classifyCommit returns the group index and entry for c. ok is false if the
// commit is excluded from the changelog.
func classifyCommit(c Commit) (idx int, entry changelogEntry, ok bool) {
	subject := strings.TrimSpace(c.Subject)
	if strings.HasPrefix(subject, "Merge ") {
		return 0, changelogEntry{}, false
	}
	entry.hash = shortHash(c.Hash)
	breaking := hasBreakingFooter(c.Body)

	m := conventionalPattern.FindStringSubmatch(subject)
	if m == nil {
		entry.description = subject
		if breaking {
			return groupBreaking, entry, true
		}
		return groupOther, entry, true
	}

	typ := strings.ToLower(m[1])
	entry.scope = m[2]
	entry.description = strings.TrimSpace(m[4])
	if m[3] == "!" {
		breaking = true
	}
	if breaking {
		return groupBreaking, entry, true
	}

	switch typ {
	case "feat":
		return groupFeatures, entry, true
	case "fix":
		if strings.EqualFold(entry.scope, "security") {
			return groupSecurity, entry, true
		}
		return groupBugFixes, entry, true
	case "sec", "security":
		return groupSecurity, entry, true
	case "perf":
		return groupPerformance, entry, true
	case "refactor":
		return groupRefactoring, entry, true
	case "docs", "test", "chore", "ci", "style", "build":
		return 0, changelogEntry{}, false
	default:
		// Unknown types keep their full subject so the type stays visible.
		entry.scope = ""
		entry.description = subject
		return groupOther, entry, true
	}
}

// hasBreakingFooter reports whether a body line starts with a BREAKING CHANGE footer.
func hasBreakingFooter(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "BREAKING CHANGE:") || strings.HasPrefix(line, "BREAKING-CHANGE:") {
			return true
		}
	}
	return false
}

// shortHash returns the first 7 characters of a commit hash.
func shortHash(hash string) string {
	if len(hash) > 7 {
		return hash[:7]
	}
	return hash
}
