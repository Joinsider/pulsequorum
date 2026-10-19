// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package alert delivers notifications: Shoutrrr and webhook senders, the
// JSON-safe webhook templates, secret masking and the leader's outbox
// dispatcher (ADR 0014, FA-ALR-*).
package alert

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/joinsider/uptimecluster/internal/cluster"
)

// placeholder matches {{name}} with optional spaces.
var placeholder = regexp.MustCompile(`\{\{\s*([a-zA-Z][a-zA-Z0-9.]*)\s*\}\}`)

// Variables lists the placeholders a webhook template may use.
var Variables = []string{
	"alertId", "event", "status", "previousStatus", "since", "sinceMs", "cause", "errorClass",
	"incidentId", "reminder", "confirmed", "message", "monitor.id", "monitor.name", "monitor.type", "tenant.id",
}

func values(p cluster.AlertPayload) map[string]any {
	return map[string]any{
		"alertId":        p.AlertID,
		"event":          p.Event,
		"status":         p.Status,
		"previousStatus": p.PreviousStatus,
		"since":          time.UnixMilli(p.SinceMs).UTC().Format(time.RFC3339),
		"sinceMs":        p.SinceMs,
		"cause":          p.Cause,
		"errorClass":     p.ErrorClass,
		"incidentId":     p.IncidentID,
		"reminder":       p.Reminder,
		"confirmed":      p.Confirmed,
		"message":        Text(p),
		"monitor.id":     p.MonitorID,
		"monitor.name":   p.MonitorName,
		"monitor.type":   p.MonitorType,
		"tenant.id":      p.TenantID,
	}
}

// DefaultTemplate is used when a webhook has no template.
const DefaultTemplate = `{"alertId":"{{alertId}}","event":"{{event}}","status":"{{status}}",` +
	`"previousStatus":"{{previousStatus}}","since":"{{since}}","monitor":{"id":"{{monitor.id}}",` +
	`"name":"{{monitor.name}}","type":"{{monitor.type}}"},"cause":"{{cause}}","errorClass":"{{errorClass}}",` +
	`"incidentId":"{{incidentId}}","reminder":"{{reminder}}","confirmed":"{{confirmed}}","message":"{{message}}"}`

// ValidateTemplate checks that a template is JSON and uses known placeholders
// only (FA-ALR-07: invalid templates are rejected when saving).
func ValidateTemplate(tpl string) error {
	if strings.TrimSpace(tpl) == "" {
		return nil
	}
	tree, err := parse(tpl)
	if err != nil {
		return err
	}
	var unknown []string
	walkStrings(tree, func(s string) {
		for _, m := range placeholder.FindAllStringSubmatch(s, -1) {
			if !slices.Contains(Variables, m[1]) && !slices.Contains(unknown, m[1]) {
				unknown = append(unknown, m[1])
			}
		}
	})
	if len(unknown) > 0 {
		return fmt.Errorf("unknown placeholders: %s", strings.Join(unknown, ", "))
	}
	return nil
}

// Render fills the template. Placeholders only exist inside JSON strings; a
// string that is exactly one placeholder becomes the typed value, otherwise
// the value is inserted as text. The result is re-encoded, so it is valid JSON
// whatever the values contain (FA-ALR-07, PF-27).
func Render(tpl string, p cluster.AlertPayload) ([]byte, error) {
	if strings.TrimSpace(tpl) == "" {
		tpl = DefaultTemplate
	}
	tree, err := parse(tpl)
	if err != nil {
		return nil, err
	}
	vals := values(p)
	out := substitute(tree, vals)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func parse(tpl string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(tpl))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("template is not valid JSON: %w", err)
	}
	if dec.More() {
		return nil, errors.New("template is not valid JSON: trailing data")
	}
	return tree, nil
}

func walkStrings(v any, fn func(string)) {
	switch t := v.(type) {
	case string:
		fn(t)
	case []any:
		for _, e := range t {
			walkStrings(e, fn)
		}
	case map[string]any:
		for k, e := range t {
			fn(k)
			walkStrings(e, fn)
		}
	}
}

func substitute(v any, vals map[string]any) any {
	switch t := v.(type) {
	case string:
		if m := placeholder.FindStringSubmatch(t); m != nil && m[0] == t {
			if val, ok := vals[m[1]]; ok {
				return val
			}
		}
		return placeholder.ReplaceAllStringFunc(t, func(s string) string {
			val, ok := vals[placeholder.FindStringSubmatch(s)[1]]
			if !ok {
				return s
			}
			return fmt.Sprint(val)
		})
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = substitute(e, vals)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[substitute(k, vals).(string)] = substitute(e, vals)
		}
		return out
	}
	return v
}

// Text is the human-readable alert used by Shoutrrr and the {{message}} placeholder.
func Text(p cluster.AlertPayload) string {
	var b strings.Builder
	switch p.Event {
	case "down":
		b.WriteString("🔴 DOWN: ")
	case "up":
		b.WriteString("🟢 UP: ")
	case "degraded":
		b.WriteString("🟡 DEGRADED: ")
	default:
		b.WriteString(p.Status + ": ")
	}
	b.WriteString(p.MonitorName)
	if p.Reminder > 0 {
		fmt.Fprintf(&b, " (still down, reminder %d)", p.Reminder)
	}
	fmt.Fprintf(&b, "\nSince %s", time.UnixMilli(p.SinceMs).UTC().Format("2006-01-02 15:04:05 MST"))
	if p.Cause != "" {
		b.WriteString("\n" + p.Cause)
	}
	if !p.Confirmed {
		b.WriteString("\n⚠ unconfirmed – cluster degraded")
	}
	return b.String()
}
