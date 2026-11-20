// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package harness

import (
	"regexp"
)

// stalePatterns returns the regular expressions that match the object names
// this prefix creates: voters, sidecars, data volumes and the network. Other
// prefixes, even ones that start with the same text, do not match.
func stalePatterns(prefix string) (containers, volumes *regexp.Regexp, network string) {
	p := regexp.QuoteMeta(prefix)
	containers = regexp.MustCompile(`^` + p + `-v[0-9]+(-netem)?$`)
	volumes = regexp.MustCompile(`^` + p + `-v[0-9]+-data$`)
	return containers, volumes, prefix + "-net"
}

// matchNames returns the names that match re, in input order.
func matchNames(names []string, re *regexp.Regexp) []string {
	var out []string
	for _, n := range names {
		if re.MatchString(n) {
			out = append(out, n)
		}
	}
	return out
}
