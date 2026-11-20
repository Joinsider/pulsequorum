// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package alert

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/store"
)

// degradedPayload is an unconfirmed DOWN alert of a degraded sender.
func degradedPayload() cluster.AlertPayload {
	p := testPayload("web")
	p.Confirmed, p.Reminder = false, 0
	p.AlertID = "degraded:node-a:m1:1700000000000:DOWN"
	p.Sender = &cluster.AlertSender{NodeID: "node-a", ReachableVoters: 1, TotalVoters: 3}
	return p
}

// TestTextSenderLine checks the view line of the sending node (FA-ALR-11).
func TestTextSenderLine(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*cluster.AlertPayload)
		contains []string
		absent   []string
	}{
		{
			name: "sender without reporters",
			mutate: func(p *cluster.AlertPayload) {
				p.Confirmed = true
				p.Sender = &cluster.AlertSender{NodeID: "node-a", ReachableVoters: 2, TotalVoters: 3, Confirmed: true}
			},
			contains: []string{"\nView: node-a · 2/3 voters"},
			absent:   []string{"reported by", "unconfirmed"},
		},
		{
			name: "sender with reporters",
			mutate: func(p *cluster.AlertPayload) {
				p.Confirmed = true
				p.Sender = &cluster.AlertSender{NodeID: "node-a", ReachableVoters: 3, TotalVoters: 3,
					Reporters: []string{"node-a", "node-b"}, Confirmed: true}
			},
			contains: []string{"\nView: node-a · 3/3 voters · reported by node-a, node-b"},
		},
		{
			name: "unconfirmed sender keeps the warning and the line",
			mutate: func(p *cluster.AlertPayload) {
				p.Sender = &cluster.AlertSender{NodeID: "node-a", ReachableVoters: 1, TotalVoters: 3}
			},
			contains: []string{"View: node-a · 1/3 voters"},
			absent:   []string{"unconfirmed"},
		},
		{
			name:   "no sender, no line",
			mutate: func(p *cluster.AlertPayload) { p.Sender, p.Confirmed = nil, true },
			absent: []string{"View:"},
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

// TestTitleUnconfirmedPrefix checks the title of an unconfirmed degraded alert.
func TestTitleUnconfirmedPrefix(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*cluster.AlertPayload)
		want   string
	}{
		{
			name:   "unconfirmed degraded",
			mutate: func(*cluster.AlertPayload) {},
			want:   "⚠ unconfirmed – cluster degraded (1/3 nodes) – DOWN: web",
		},
		{
			name:   "confirmed with sender",
			mutate: func(p *cluster.AlertPayload) { p.Confirmed = true },
			want:   "DOWN: web",
		},
		{
			name:   "unconfirmed without sender keeps the plain title",
			mutate: func(p *cluster.AlertPayload) { p.Sender = nil },
			want:   "DOWN: web",
		},
		{
			name: "correction is not prefixed",
			mutate: func(p *cluster.AlertPayload) {
				p.Verdict = store.VerdictConfirmed
			},
			want: "Confirmed: web was DOWN since " + sinceText(testPayload("web").SinceMs),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := degradedPayload()
			tt.mutate(&p)
			if got := title(p); got != tt.want {
				t.Errorf("title = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestVerdictTexts checks the correction texts (FA-ALR-10).
func TestVerdictTexts(t *testing.T) {
	falseAlarm := degradedPayload()
	falseAlarm.Verdict = store.VerdictFalseAlarm
	falseAlarm.Sender = &cluster.AlertSender{NodeID: "node-c", ReachableVoters: 1, TotalVoters: 3}

	confirmed := degradedPayload()
	confirmed.Verdict = store.VerdictConfirmed
	confirmed.Confirmed = true

	t.Run("false alarm", func(t *testing.T) {
		want := "False alarm: web was not DOWN (view of node-c, 1/3 voters)"
		if got := title(falseAlarm); got != want {
			t.Errorf("title = %q, want %q", got, want)
		}
		text := Text(falseAlarm)
		if !strings.HasPrefix(text, want+"\nSince ") {
			t.Errorf("Text = %q, want the sentence and the since line", text)
		}
		if strings.Contains(text, "unconfirmed") || strings.Contains(text, "View:") {
			t.Errorf("Text = %q, must not carry the unconfirmed warning or the view line", text)
		}
	})

	t.Run("false alarm without sender", func(t *testing.T) {
		p := falseAlarm
		p.Sender = nil
		want := "False alarm: web was not DOWN"
		if got := title(p); got != want {
			t.Errorf("title = %q, want %q", got, want)
		}
	})

	t.Run("confirmed", func(t *testing.T) {
		want := "Confirmed: web was DOWN since " + sinceText(confirmed.SinceMs)
		if got := Text(confirmed); got != want {
			t.Errorf("Text = %q, want %q", got, want)
		}
	})

	t.Run("unknown verdict keeps the status text", func(t *testing.T) {
		p := degradedPayload()
		p.Verdict = store.VerdictUnverifiable
		if got := Text(p); !strings.HasPrefix(got, "🔴 DOWN: web") || !strings.Contains(got, "View: node-a") {
			t.Errorf("Text = %q, want the status text with the view line", got)
		}
	})
}

// TestBundleVerdictsAndUnconfirmed checks that bundles list corrections as
// sentences and mark unconfirmed lines.
func TestBundleVerdictsAndUnconfirmed(t *testing.T) {
	falseAlarm := degradedPayload()
	falseAlarm.Verdict = store.VerdictFalseAlarm
	confirmed := degradedPayload()
	confirmed.Verdict = store.VerdictConfirmed
	confirmed.Confirmed = true

	ps := []cluster.AlertPayload{degradedPayload(), falseAlarm, confirmed}
	if got := BundleTitle(ps); got != "3 alerts" {
		t.Errorf("BundleTitle = %q, want %q", got, "3 alerts")
	}
	text := BundleText(ps)
	for _, want := range []string{
		"- web: DOWN at " + sinceText(degradedPayload().SinceMs) + " (unconfirmed)",
		"- False alarm: web was not DOWN (view of node-a, 1/3 voters)",
		"- Confirmed: web was DOWN since " + sinceText(confirmed.SinceMs),
	} {
		if !strings.Contains(text, want) {
			t.Errorf("BundleText = %q, missing %q", text, want)
		}
	}
}

// TestDefaultTemplateSenderAndVerdict checks that the default webhook JSON
// carries the sender view and the verdict, also for plain alerts.
func TestDefaultTemplateSenderAndVerdict(t *testing.T) {
	p := degradedPayload()
	p.Verdict = store.VerdictFalseAlarm
	out, err := Render(DefaultTemplate, p)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Verdict string `json:"verdict"`
		Sender  struct {
			NodeID          string `json:"nodeId"`
			ReachableVoters int    `json:"reachableVoters"`
			TotalVoters     int    `json:"totalVoters"`
		} `json:"sender"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if got.Verdict != store.VerdictFalseAlarm || got.Sender.NodeID != "node-a" ||
		got.Sender.ReachableVoters != 1 || got.Sender.TotalVoters != 3 {
		t.Errorf("rendered = %+v", got)
	}

	plain, err := Render(DefaultTemplate, testPayload("web"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(plain, &got); err != nil {
		t.Fatalf("plain alert not valid JSON: %v\n%s", err, plain)
	}
	if got.Sender.NodeID != "" || got.Verdict != "" {
		t.Errorf("plain alert rendered sender %+v, verdict %q", got.Sender, got.Verdict)
	}
}

// TestValidateSenderPlaceholders checks that the new placeholders are accepted.
func TestValidateSenderPlaceholders(t *testing.T) {
	tpl := `{"v":"{{verdict}}","n":"{{sender.nodeId}}","r":"{{sender.reachableVoters}}","t":"{{sender.totalVoters}}"}`
	if err := ValidateTemplate(tpl); err != nil {
		t.Errorf("ValidateTemplate = %v, want nil", err)
	}
	if err := ValidateTemplate(`{"x":"{{sender.reporters}}"}`); err == nil {
		t.Error("unknown placeholder sender.reporters accepted")
	}
}
