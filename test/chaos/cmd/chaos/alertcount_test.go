// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/joinsider/uptimecluster/internal/alert"
	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/test/internal/fake"
)

// postNtfy delivers one message to the recorder the way shoutrrr does: the
// title in the Title header, the text as body.
func postNtfy(rec *fake.Recorder, title, body string) {
	req := httptest.NewRequest(http.MethodPost, "/topic", strings.NewReader(body))
	req.Header.Set("Title", title)
	rec.ServeHTTP(httptest.NewRecorder(), req)
}

// TestNotifyCountBundleLines checks that alerts inside a real bundle (built by
// alert.BundleTitle and alert.BundleText) count per monitor and event, and that
// unconfirmed lines are told apart from confirmed ones.
func TestNotifyCountBundleLines(t *testing.T) {
	const since = 1_800_000_000_000
	unconfirmed := &cluster.AlertSender{ReachableVoters: 1, TotalVoters: 3}
	ps := []cluster.AlertPayload{
		{MonitorName: "m1", Event: store.EventDown, Status: "DOWN", SinceMs: since},
		{MonitorName: "m2", Event: store.EventUp, Status: "UP", SinceMs: since},
		{MonitorName: "m3", Event: store.EventDown, Status: "DOWN", SinceMs: since, Sender: unconfirmed},
	}
	rec := fake.NewRecorder()
	start := time.Now().Add(-time.Second)
	postNtfy(rec, alert.BundleTitle(ps), alert.BundleText(ps))
	postNtfy(rec, "DOWN: m4", "single body")
	postNtfy(rec, as04UnconfirmedPrefix+"DOWN: m5", "single body")

	cases := []struct {
		title string
		want  int
	}{
		{"DOWN: m1", 1},                         // confirmed DOWN in a bundle
		{"UP: m2", 1},                           // UP in a bundle
		{"DOWN: m3", 0},                         // unconfirmed line is not confirmed
		{as04UnconfirmedPrefix + "DOWN: m3", 1}, // unconfirmed line in a bundle
		{"UP: m1", 0},                           // wrong event
		{"DOWN: m2", 0},                         // wrong event
		{"DOWN: m", 0},                          // a name is matched whole
		{"DOWN: m4", 1},                         // single message by title
		{as04UnconfirmedPrefix + "DOWN: m5", 1}, // single unconfirmed message
		{as04UnconfirmedPrefix + "DOWN: m4", 0}, // confirmed single is not unconfirmed
	}
	for _, c := range cases {
		got, at := notifyCount(rec, start, c.title)
		if got != c.want || len(at) != got {
			t.Errorf("notifyCount(%q) = %d (times %d), want %d", c.title, got, len(at), c.want)
		}
	}
	if n, _ := notifyCount(rec, time.Now().Add(time.Hour), "DOWN: m1"); n != 0 {
		t.Errorf("notifyCount before the window = %d, want 0", n)
	}
}

// TestBundleLinesParsesRealBody checks the entry lines of a real bundle body.
func TestBundleLinesParsesRealBody(t *testing.T) {
	ps := []cluster.AlertPayload{
		{MonitorName: "m1", Event: store.EventDown, Status: "DOWN", SinceMs: 1_800_000_000_000},
		{MonitorName: "m2", Event: store.EventUp, Status: "UP", SinceMs: 1_800_000_000_000},
	}
	lines := bundleLines(alert.BundleText(ps))
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "m1: DOWN at ") || !strings.HasPrefix(lines[1], "m2: UP at ") {
		t.Fatalf("bundleLines = %q", lines)
	}
	if !bundleLineIs(lines[0], "DOWN", "m1", false) || bundleLineIs(lines[0], "UP", "m1", false) {
		t.Errorf("line %q is matched wrongly", lines[0])
	}
}
