// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package alert

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/store"
)

func flappingPayload(flapping bool) cluster.AlertPayload {
	p := testPayload("web")
	p.Event, p.Status, p.Cause, p.Reminder, p.Confirmed = store.EventFlapping, "DOWN", "", 0, true
	p.Flapping, p.ChangeCount = flapping, 6
	if !flapping {
		p.Status, p.ChangeCount = "UP", 7
	}
	return p
}

func channelFailedPayload() cluster.AlertPayload {
	return cluster.AlertPayload{
		AlertID: "ch:failed", TenantID: "t1", Event: store.EventChannelFailed, SinceMs: 1_700_000_000_000,
		ChannelID: "c2", ChannelName: "ops mail", ChannelError: "webhook answered 401 Unauthorized",
	}
}

// TestTextNewEvents checks the text of flapping and channel failure alerts.
func TestTextNewEvents(t *testing.T) {
	tests := []struct {
		name     string
		p        cluster.AlertPayload
		contains []string
		absent   []string
	}{
		{
			name:     "flapping start",
			p:        flappingPayload(true),
			contains: []string{"web is flapping: 6 changes in 30 min, alerts suppressed"},
			absent:   []string{"unconfirmed"},
		},
		{
			name:     "flapping end",
			p:        flappingPayload(false),
			contains: []string{"web stable again, status UP, 7 changes"},
			absent:   []string{"flapping:", "unconfirmed"},
		},
		{
			name:     "channel failed",
			p:        channelFailedPayload(),
			contains: []string{"Notification channel ops mail failed: webhook answered 401 Unauthorized"},
			absent:   []string{"unconfirmed", "https://"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text := Text(tt.p)
			for _, s := range tt.contains {
				if !strings.Contains(text, s) {
					t.Errorf("Text = %q, missing %q", text, s)
				}
			}
			for _, s := range tt.absent {
				if strings.Contains(text, s) {
					t.Errorf("Text = %q, must not contain %q", text, s)
				}
			}
		})
	}
}

// TestDefaultTemplateNewEvents checks that the default webhook JSON carries the
// new fields and stays valid JSON.
func TestDefaultTemplateNewEvents(t *testing.T) {
	out, err := Render(DefaultTemplate, flappingPayload(true))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Event       string `json:"event"`
		Flapping    bool   `json:"flapping"`
		ChangeCount int    `json:"changeCount"`
		Message     string `json:"message"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if got.Event != store.EventFlapping || !got.Flapping || got.ChangeCount != 6 ||
		!strings.Contains(got.Message, "is flapping") {
		t.Errorf("rendered = %+v", got)
	}

	out, err = Render(DefaultTemplate, channelFailedPayload())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"name":"ops mail"`) {
		t.Errorf("channel fields missing: %s", out)
	}
}

// TestValidateNewPlaceholders checks that the new placeholders are accepted.
func TestValidateNewPlaceholders(t *testing.T) {
	if err := ValidateTemplate(`{"n":"{{changeCount}}","f":"{{flapping}}","c":"{{channel.name}}","e":"{{channel.error}}"}`); err != nil {
		t.Errorf("ValidateTemplate = %v, want nil", err)
	}
}

// TestBundleTextRendering checks the header and one line per alert of a bundle.
func TestBundleTextRendering(t *testing.T) {
	down := testPayload("api")
	down.Reminder = 0
	up := testPayload("db")
	up.Event, up.Status, up.Cause, up.Reminder, up.Confirmed = "up", "UP", "", 0, true

	ps := []cluster.AlertPayload{down, up}
	if got := BundleTitle(ps); got != "2 status changes" {
		t.Errorf("BundleTitle = %q, want %q", got, "2 status changes")
	}
	text := BundleText(ps)
	for _, want := range []string{"2 status changes", "- api: DOWN at", "- db: UP at"} {
		if !strings.Contains(text, want) {
			t.Errorf("BundleText = %q, missing %q", text, want)
		}
	}
	if strings.Contains(text, "unconfirmed") {
		t.Errorf("BundleText = %q, must not carry the per-alert warning", text)
	}

	mixed := []cluster.AlertPayload{down, flappingPayload(true), channelFailedPayload()}
	if got := BundleTitle(mixed); got != "3 alerts" {
		t.Errorf("BundleTitle mixed = %q, want %q", got, "3 alerts")
	}
	text = BundleText(mixed)
	for _, want := range []string{"- web: flapping, 6 changes", "- channel ops mail failed:"} {
		if !strings.Contains(text, want) {
			t.Errorf("BundleText mixed = %q, missing %q", text, want)
		}
	}
}
