// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package alert

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/store"
)

// ADR 0025, section 5 and 7: the text of a system alert names what happened, and
// the outcome of each attempt is labelled for the delivery metrics.
func TestSystemText(t *testing.T) {
	tests := []struct {
		name    string
		system  *cluster.SystemAlertInfo
		want    string
		wantAll string
	}{
		{
			name:   "quorum lost",
			system: &cluster.SystemAlertInfo{Kind: "quorum", Subject: "cluster", Active: true, Details: json.RawMessage(`{"reachable":1,"total":3}`)},
			want:   "Quorum lost: no leader, 1 of 3 voters reachable",
		},
		{
			name:   "quorum restored",
			system: &cluster.SystemAlertInfo{Kind: "quorum", Subject: "cluster", Details: json.RawMessage(`{"gapSeconds":754}`)},
			want:   "Quorum restored after 754 s",
		},
		{
			name:   "certificate stage",
			system: &cluster.SystemAlertInfo{Kind: "cert_expiry", Subject: "ca", Active: true, Level: 7},
			want:   "System alert cert_expiry (ca), level 7",
		},
		{
			name:   "raised without level",
			system: &cluster.SystemAlertInfo{Kind: "voter_offline", Subject: "n2", Active: true},
			want:   "System alert voter_offline (n2)",
		},
		{
			name:   "cleared",
			system: &cluster.SystemAlertInfo{Kind: "disk_low", Subject: "n2"},
			want:   "System alert disk_low (n2) cleared",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := cluster.AlertPayload{Event: store.EventSystem, System: tt.system}
			if got := Text(p); got != tt.want {
				t.Errorf("Text = %q, want %q", got, tt.want)
			}
			if got := title(p); got != tt.want {
				t.Errorf("title = %q, want %q", got, tt.want)
			}
			if got := bundleLine(p); got != tt.want {
				t.Errorf("bundleLine = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOutcomeOf(t *testing.T) {
	tests := []struct {
		res  result
		want string
	}{
		{result{delivered: true}, OutcomeSent},
		{result{retryAfter: time.Minute}, OutcomeThrottled},
		{result{msg: "webhook answered 500"}, OutcomeFailed},
		{result{permanent: true, msg: "invalid"}, OutcomeFailed},
	}
	for _, tt := range tests {
		if got := outcomeOf(tt.res); got != tt.want {
			t.Errorf("outcomeOf(%+v) = %q, want %q", tt.res, got, tt.want)
		}
	}
}

// The local dispatcher reports every attempt with the channel kind, so the
// throttled attempt and the later success both count.
func TestLocalOnOutcomeReportsEachAttempt(t *testing.T) {
	type obs struct{ kind, outcome string }
	var seen []obs
	l := newDoneLog()
	clk := newClock()
	s := &recordingSender{err: &DeliveryError{RetryAfter: time.Second, Msg: "webhook answered 429 Too Many Requests"}}
	d := NewLocalDispatcher(LocalDispatcherOptions{
		Sender: s, Now: clk.now, Jitter: func() float64 { return 0.5 },
		OnOutcome: func(kind, outcome string) { seen = append(seen, obs{kind, outcome}) },
	})
	d.Enqueue(localDel("a0", "ch1", store.ChannelWebhook, l))
	d.deliverDue(context.Background(), clk.now())
	s.setErr(nil)
	clk.t = clk.t.Add(31 * time.Second) // the backoff of the first retry
	d.deliverDue(context.Background(), clk.now())

	want := []obs{{store.ChannelWebhook, OutcomeThrottled}, {store.ChannelWebhook, OutcomeSent}}
	if fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Errorf("OnOutcome = %v, want %v", seen, want)
	}
}

// TestDefaultTemplateCarriesSystemAlert: a webhook without its own template
// receives kind, subject, active and level of a system alert (ADR 0025).
func TestDefaultTemplateCarriesSystemAlert(t *testing.T) {
	p := cluster.AlertPayload{AlertID: "sys:voter_offline:n2:1:active", Event: store.EventSystem, TenantID: store.SystemTenantID,
		Confirmed: true, System: &cluster.SystemAlertInfo{Kind: "voter_offline", Subject: "n2", Active: true, Level: 0}}
	body, err := Render(DefaultTemplate, p)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		System struct {
			Kind, Subject string
			Active        bool
			Level         int
		} `json:"system"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, body)
	}
	if got.System.Kind != "voter_offline" || got.System.Subject != "n2" || !got.System.Active || got.System.Level != 0 {
		t.Errorf("system = %+v", got.System)
	}
}
