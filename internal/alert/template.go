// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

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

	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/sysalert"
)

// placeholder matches {{name}} with optional spaces.
var placeholder = regexp.MustCompile(`\{\{\s*([a-zA-Z][a-zA-Z0-9.]*)\s*\}\}`)

// Variables lists the placeholders a webhook template may use.
var Variables = []string{
	"alertId", "event", "status", "previousStatus", "since", "sinceMs", "cause", "errorClass",
	"incidentId", "reminder", "confirmed", "message", "monitor.id", "monitor.name", "monitor.type", "tenant.id",
	"flapping", "changeCount", "channel.id", "channel.name", "channel.error",
	"verdict", "sender.nodeId", "sender.reachableVoters", "sender.totalVoters",
	"system.kind", "system.subject", "system.active", "system.level",
}

// flappingWindow names the window of the flapping rule (ADR 0023, FA-ALR-06).
const flappingWindow = "30 min"

func values(p cluster.AlertPayload) map[string]any {
	return map[string]any{
		"alertId":                p.AlertID,
		"event":                  p.Event,
		"status":                 p.Status,
		"previousStatus":         p.PreviousStatus,
		"since":                  time.UnixMilli(p.SinceMs).UTC().Format(time.RFC3339),
		"sinceMs":                p.SinceMs,
		"cause":                  p.Cause,
		"errorClass":             p.ErrorClass,
		"incidentId":             p.IncidentID,
		"reminder":               p.Reminder,
		"confirmed":              p.Confirmed,
		"message":                Text(p),
		"monitor.id":             p.MonitorID,
		"monitor.name":           p.MonitorName,
		"monitor.type":           p.MonitorType,
		"tenant.id":              p.TenantID,
		"flapping":               p.Flapping,
		"changeCount":            p.ChangeCount,
		"channel.id":             p.ChannelID,
		"channel.name":           p.ChannelName,
		"channel.error":          p.ChannelError,
		"verdict":                p.Verdict,
		"sender.nodeId":          senderOf(p).NodeID,
		"sender.reachableVoters": senderOf(p).ReachableVoters,
		"sender.totalVoters":     senderOf(p).TotalVoters,
		"system.kind":            systemOf(p).Kind,
		"system.subject":         systemOf(p).Subject,
		"system.active":          systemOf(p).Active,
		"system.level":           systemOf(p).Level,
	}
}

// systemOf returns the system alert of p, or the zero value for other events (ADR 0025).
func systemOf(p cluster.AlertPayload) cluster.SystemAlertInfo {
	if p.System == nil {
		return cluster.SystemAlertInfo{}
	}
	return *p.System
}

// senderOf returns the sender view of p, or the zero view if p has none.
func senderOf(p cluster.AlertPayload) cluster.AlertSender {
	if p.Sender == nil {
		return cluster.AlertSender{}
	}
	return *p.Sender
}

// DefaultTemplate is used when a webhook has no template.
const DefaultTemplate = `{"alertId":"{{alertId}}","event":"{{event}}","status":"{{status}}",` +
	`"previousStatus":"{{previousStatus}}","since":"{{since}}","monitor":{"id":"{{monitor.id}}",` +
	`"name":"{{monitor.name}}","type":"{{monitor.type}}"},"cause":"{{cause}}","errorClass":"{{errorClass}}",` +
	`"incidentId":"{{incidentId}}","reminder":"{{reminder}}","confirmed":"{{confirmed}}",` +
	`"verdict":"{{verdict}}","sender":{"nodeId":"{{sender.nodeId}}",` +
	`"reachableVoters":"{{sender.reachableVoters}}","totalVoters":"{{sender.totalVoters}}"},` +
	`"flapping":"{{flapping}}","changeCount":"{{changeCount}}",` +
	`"channel":{"id":"{{channel.id}}","name":"{{channel.name}}","error":"{{channel.error}}"},` +
	`"system":{"kind":"{{system.kind}}","subject":"{{system.subject}}","active":"{{system.active}}",` +
	`"level":"{{system.level}}"},` +
	`"message":"{{message}}"}`

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
	switch p.Event {
	case store.EventFlapping:
		return flappingText(p)
	case store.EventChannelFailed:
		return channelFailedText(p)
	case store.EventSystem:
		return systemText(p)
	}
	if s, ok := verdictSentence(p); ok {
		if p.Verdict == store.VerdictFalseAlarm {
			return s + "\nSince " + sinceText(p.SinceMs)
		}
		return s
	}
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
	// The title of an unconfirmed degraded alert carries the warning already.
	if !p.Confirmed && p.Sender == nil {
		b.WriteString("\n⚠ unconfirmed – cluster degraded")
	}
	if p.Sender != nil {
		b.WriteString("\n" + senderLine(p.Sender))
	}
	return b.String()
}

// senderLine is the view of the sending node (FA-ALR-11), e.g.
// "View: node-a · 3/3 voters · reported by node-a, node-b".
func senderLine(s *cluster.AlertSender) string {
	line := fmt.Sprintf("View: %s · %d/%d voters", s.NodeID, s.ReachableVoters, s.TotalVoters)
	if len(s.Reporters) > 0 {
		line += " · reported by " + strings.Join(s.Reporters, ", ")
	}
	return line
}

// isUnconfirmedDegraded reports whether p is an alert the degraded mode sent
// without quorum and not yet corrected (FA-ALR-10, ADR 0024).
func isUnconfirmedDegraded(p cluster.AlertPayload) bool {
	return p.Sender != nil && !p.Confirmed && p.Verdict == ""
}

// verdictSentence is the correction of an unconfirmed alert after the quorum
// returned (FA-ALR-10). ok is false for payloads without a known verdict.
func verdictSentence(p cluster.AlertPayload) (sentence string, ok bool) {
	switch p.Verdict {
	case store.VerdictFalseAlarm:
		sentence = fmt.Sprintf("False alarm: %s was not %s", p.MonitorName, p.Status)
		if p.Sender != nil {
			sentence += fmt.Sprintf(" (view of %s, %d/%d voters)", p.Sender.NodeID, p.Sender.ReachableVoters, p.Sender.TotalVoters)
		}
		return sentence, true
	case store.VerdictConfirmed:
		return fmt.Sprintf("Confirmed: %s was %s since %s", p.MonitorName, p.Status, sinceText(p.SinceMs)), true
	}
	return "", false
}

// flappingText announces the start (Flapping) or the end of a flapping period.
func flappingText(p cluster.AlertPayload) string {
	if p.Flapping {
		return fmt.Sprintf("⚠ %s is flapping: %d changes in %s, alerts suppressed\nSince %s",
			p.MonitorName, p.ChangeCount, flappingWindow, sinceText(p.SinceMs))
	}
	return fmt.Sprintf("🟢 %s stable again, status %s, %d changes\nSince %s",
		p.MonitorName, p.Status, p.ChangeCount, sinceText(p.SinceMs))
}

// channelFailedText tells the admin that another channel stopped working
// (FA-ALR-05). The error is already masked by the FSM and holds no secrets.
func channelFailedText(p cluster.AlertPayload) string {
	if p.ChannelError == "" {
		return fmt.Sprintf("⚠ Notification channel %s failed", p.ChannelName)
	}
	return fmt.Sprintf("⚠ Notification channel %s failed: %s", p.ChannelName, p.ChannelError)
}

// systemText renders a system alert (FA-OBS-01, ADR 0025): what happened and
// whether it started or ended. Only numbers and node IDs appear in it.
func systemText(p cluster.AlertPayload) string {
	s := p.System
	if s == nil {
		return "System alert"
	}
	var d map[string]float64
	_ = json.Unmarshal(s.Details, &d)
	switch {
	case s.Kind == sysalert.KindQuorum && s.Active:
		return fmt.Sprintf("Quorum lost: no leader, %d of %d voters reachable", int64(d["reachable"]), int64(d["total"]))
	case s.Kind == sysalert.KindQuorum:
		return fmt.Sprintf("Quorum restored after %d s", int64(d["gapSeconds"]))
	case s.Active && s.Level > 0:
		return fmt.Sprintf("System alert %s (%s), level %d", s.Kind, s.Subject, s.Level)
	case s.Active:
		return fmt.Sprintf("System alert %s (%s)", s.Kind, s.Subject)
	default:
		return fmt.Sprintf("System alert %s (%s) cleared", s.Kind, s.Subject)
	}
}

// title is the Shoutrrr title of a single alert.
func title(p cluster.AlertPayload) string {
	switch p.Event {
	case store.EventFlapping:
		if p.Flapping {
			return "Flapping: " + p.MonitorName
		}
		return "Stable: " + p.MonitorName
	case store.EventChannelFailed:
		return "Channel failed: " + p.ChannelName
	case store.EventSystem:
		return systemText(p)
	}
	if s, ok := verdictSentence(p); ok {
		return s
	}
	t := fmt.Sprintf("%s: %s", p.Status, p.MonitorName)
	if isUnconfirmedDegraded(p) {
		return fmt.Sprintf("⚠ unconfirmed – cluster degraded (%d/%d nodes) – %s", p.Sender.ReachableVoters, p.Sender.TotalVoters, t)
	}
	return t
}

// BundleTitle is the header and Shoutrrr title of a bundle of alerts (ADR 0023).
func BundleTitle(ps []cluster.AlertPayload) string {
	for _, p := range ps {
		if !isStatusEvent(p.Event) || p.Verdict != "" {
			return fmt.Sprintf("%d alerts", len(ps))
		}
	}
	return fmt.Sprintf("%d status changes", len(ps))
}

// BundleText renders several alerts of one Shoutrrr channel as one message: a
// header and one line per alert with monitor, status or event, and time.
func BundleText(ps []cluster.AlertPayload) string {
	var b strings.Builder
	b.WriteString(BundleTitle(ps))
	for _, p := range ps {
		b.WriteString("\n- " + bundleLine(p))
	}
	return b.String()
}

func bundleLine(p cluster.AlertPayload) string {
	switch p.Event {
	case store.EventFlapping:
		if p.Flapping {
			return fmt.Sprintf("%s: flapping, %d changes in %s, alerts suppressed", p.MonitorName, p.ChangeCount, flappingWindow)
		}
		return fmt.Sprintf("%s: stable again, status %s, %d changes", p.MonitorName, p.Status, p.ChangeCount)
	case store.EventChannelFailed:
		return fmt.Sprintf("channel %s failed: %s", p.ChannelName, p.ChannelError)
	case store.EventSystem:
		return systemText(p)
	}
	if s, ok := verdictSentence(p); ok {
		return s
	}
	line := fmt.Sprintf("%s: %s", p.MonitorName, strings.ToUpper(p.Event))
	if p.Reminder > 0 {
		line += fmt.Sprintf(" (reminder %d)", p.Reminder)
	}
	line += " at " + sinceText(p.SinceMs)
	if isUnconfirmedDegraded(p) {
		line += " (unconfirmed)"
	}
	return line
}

func isStatusEvent(event string) bool {
	switch event {
	case "down", "up", "degraded", "partial":
		return true
	}
	return false
}

func sinceText(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04:05 MST")
}
