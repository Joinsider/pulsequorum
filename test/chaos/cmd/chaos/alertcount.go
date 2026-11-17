// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package main

import (
	"strings"
	"time"

	"github.com/joinsider/uptimecluster/test/internal/fake"
)

// unconfirmedLineSuffix ends the line of an unconfirmed alert in a bundle
// (alert.BundleText).
const unconfirmedLineSuffix = " (unconfirmed)"

// isBundleTitle reports whether a message title is the header of a bundle
// (alert.BundleTitle): "N status changes" or "N alerts".
func isBundleTitle(title string) bool {
	return strings.HasSuffix(title, " status changes") || strings.HasSuffix(title, " alerts")
}

// bundleLines returns the entry lines of a bundle body (alert.BundleText),
// without their "- " marker.
func bundleLines(body string) []string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		if rest, ok := strings.CutPrefix(l, "- "); ok {
			out = append(out, rest)
		}
	}
	return out
}

// alertTitle splits the title of a single alert, "DOWN: name" or
// "<unconfirmed prefix>UP: name". ok is false for any other title.
func alertTitle(title string) (event, name string, unconfirmed, ok bool) {
	if rest, cut := strings.CutPrefix(title, as04UnconfirmedPrefix); cut {
		title, unconfirmed = rest, true
	}
	event, name, found := strings.Cut(title, ": ")
	if !found || (event != "DOWN" && event != "UP") {
		return "", "", false, false
	}
	return event, name, unconfirmed, true
}

// bundleLineIs reports whether one bundle entry line is the alert of monitor
// name for event. The line of an unconfirmed alert ends with " (unconfirmed)".
// An alert that lacks the suffix is confirmed.
func bundleLineIs(line, event, name string, unconfirmed bool) bool {
	if !strings.HasPrefix(line, name+": "+event+" at ") {
		return false
	}
	return strings.HasSuffix(line, unconfirmedLineSuffix) == unconfirmed
}

// notifyCount counts the alerts since the given time that match title. A
// single message matches by its title. A bundle matches once per entry line
// for the same monitor and event, so an alert that went out inside a bundle
// is still counted. It returns the count and the arrival times.
func notifyCount(rec *fake.Recorder, since time.Time, title string) (int, []time.Time) {
	event, name, unconfirmed, ok := alertTitle(title)
	var n int
	var at []time.Time
	for _, m := range rec.Messages() {
		if m.At.Before(since) {
			continue
		}
		if m.Title == title {
			n++
			at = append(at, m.At)
			continue
		}
		if !ok || !isBundleTitle(m.Title) {
			continue
		}
		for _, l := range bundleLines(m.Body) {
			if bundleLineIs(l, event, name, unconfirmed) {
				n++
				at = append(at, m.At)
			}
		}
	}
	return n, at
}
