// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package alert

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/joinsider/uptimecluster/internal/cluster"
)

func testPayload(name string) cluster.AlertPayload {
	return cluster.AlertPayload{
		AlertID: "m1:1700000000000:DOWN", TenantID: "t1", MonitorID: "m1", MonitorName: name,
		MonitorType: "http", Event: "down", Status: "DOWN", PreviousStatus: "UP",
		SinceMs: 1_700_000_000_000, Cause: "connection refused", ErrorClass: "connect",
		IncidentID: "inc-1", Reminder: 2, Confirmed: false,
	}
}

// TestValidateTemplate covers FA-ALR-07: templates must be JSON and use known
// placeholders only.
func TestValidateTemplate(t *testing.T) {
	tests := []struct {
		name    string
		tpl     string
		wantErr string // empty = valid
	}{
		{"empty", "", ""},
		{"blank", " \n\t", ""},
		{"default", DefaultTemplate, ""},
		{"known placeholder", `{"name":"{{monitor.name}}"}`, ""},
		{"invalid json", `{"a":`, "not valid JSON"},
		{"plain text", `alert {{status}}`, "not valid JSON"},
		{"trailing data", `{"a":1} {"b":2}`, "trailing data"},
		{"trailing garbage", `{"a":1} x`, "not valid JSON"},
		{"unknown placeholder", `{"a":"{{nope}}"}`, "unknown placeholders: nope"},
		{"unknown placeholder in key", `{"{{nope}}":1}`, "unknown placeholders: nope"},
		{"unknown placeholders listed once", `{"a":"{{x}}","b":"{{x}} {{y}}"}`, "unknown placeholders: x, y"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateTemplate(tt.tpl)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("error = %v, want none", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestRenderTypedPlaceholders(t *testing.T) {
	out, err := Render(`{"reminder":"{{reminder}}","confirmed":"{{confirmed}}","since":"{{sinceMs}}"}`, testPayload("web"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("invalid JSON %s: %v", out, err)
	}
	if got["reminder"] != float64(2) || got["confirmed"] != false || got["since"] != float64(1_700_000_000_000) {
		t.Errorf("typed values = %#v", got)
	}
}

func TestRenderTextPlaceholders(t *testing.T) {
	tests := []struct {
		name string
		tpl  string
		want string // JSON of the result
	}{
		{"inside longer string", `{"msg":"{{monitor.name}} is {{status}}"}`, `{"msg":"web is DOWN"}`},
		{"in object key", `{"{{monitor.id}}":"x"}`, `{"m1":"x"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := Render(tt.tpl, testPayload("web"))
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != tt.want {
				t.Errorf("out = %s, want %s", out, tt.want)
			}
		})
	}
}

// TestRenderMonitorNameRoundTrip covers FA-ALR-07 and PF-27: whatever the
// monitor name contains, the webhook body is valid JSON with the exact name.
func TestRenderMonitorNameRoundTrip(t *testing.T) {
	names := []string{
		`plain`,
		`with "quotes"`,
		`back\slash`,
		"line one\nline two",
		`</script><b>&`,
		"tab\there",
		"unicode ü 🔴",
		`{{status}} literal`,
		`\"}, "injected": true, "x": "`,
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			out, err := Render(DefaultTemplate, testPayload(name))
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Monitor struct {
					Name string `json:"name"`
				} `json:"monitor"`
				Injected any `json:"injected"`
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("invalid JSON %s: %v", out, err)
			}
			if got.Monitor.Name != name || got.Injected != nil {
				t.Errorf("monitor name = %q (injected %v), want %q", got.Monitor.Name, got.Injected, name)
			}
		})
	}
}

func TestRenderEmptyTemplateUsesDefault(t *testing.T) {
	p := testPayload("web")
	fromEmpty, err := Render("  ", p)
	if err != nil {
		t.Fatal(err)
	}
	fromDefault, err := Render(DefaultTemplate, p)
	if err != nil {
		t.Fatal(err)
	}
	if string(fromEmpty) != string(fromDefault) || !json.Valid(fromEmpty) {
		t.Errorf("empty = %s, default = %s", fromEmpty, fromDefault)
	}
}

func TestRenderInvalidTemplate(t *testing.T) {
	if _, err := Render(`{"a":`, testPayload("web")); err == nil {
		t.Error("invalid template rendered")
	}
}

func TestText(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*cluster.AlertPayload)
		contains []string
		absent   []string
	}{
		{
			name:     "unconfirmed down",
			mutate:   func(_ *cluster.AlertPayload) {},
			contains: []string{"DOWN: web", "connection refused", "unconfirmed"},
		},
		{
			name: "confirmed down without reminder",
			mutate: func(p *cluster.AlertPayload) {
				p.Confirmed, p.Reminder = true, 0
			},
			contains: []string{"DOWN: web", "connection refused"},
			absent:   []string{"unconfirmed", "reminder"},
		},
		{
			name: "reminder note",
			mutate: func(p *cluster.AlertPayload) {
				p.Confirmed, p.Reminder = true, 3
			},
			contains: []string{"DOWN: web (still down, reminder 3)"},
			absent:   []string{"unconfirmed"},
		},
		{
			name: "up",
			mutate: func(p *cluster.AlertPayload) {
				p.Event, p.Status, p.Cause, p.Reminder, p.Confirmed = "up", "UP", "", 0, true
			},
			contains: []string{"UP: web"},
			absent:   []string{"DOWN", "connection refused", "unconfirmed"},
		},
		{
			name: "degraded",
			mutate: func(p *cluster.AlertPayload) {
				p.Event, p.Status, p.Reminder, p.Confirmed = "degraded", "DEGRADED", 0, true
			},
			contains: []string{"DEGRADED: web"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := testPayload("web")
			tt.mutate(&p)
			text := Text(p)
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
