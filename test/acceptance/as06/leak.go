// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import "strings"

// needle is a value of tenant A that must not show up in an answer to B.
type needle struct {
	label string
	value string
}

// findLeaks returns the labels of the needles that occur in body. Empty
// values are skipped, so an unset ID cannot match every body.
func findLeaks(body string, needles []needle) []string {
	var found []string
	for _, n := range needles {
		if n.value != "" && strings.Contains(body, n.value) {
			found = append(found, n.label)
		}
	}
	return found
}

// scrub replaces every non-empty secret in msg with "<redacted>".
func scrub(msg string, secrets ...string) string {
	for _, s := range secrets {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, "<redacted>")
		}
	}
	return msg
}
