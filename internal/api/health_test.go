// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/api/gen"
	"github.com/joinsider/pulsequorum/internal/degraded"
	"github.com/joinsider/pulsequorum/internal/store"
)

// fixedDegraded reports a fixed degraded state.
type fixedDegraded struct{ st degraded.State }

func (f fixedDegraded) State() degraded.State { return f.st }

// TestHealthReportsDegradedState covers the additive health fields of the
// degraded mode (ADR 0024): they are set only while degraded.
func TestHealthReportsDegradedState(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name          string
		state         degraded.State
		wantDegraded  bool
		wantPartition []string
	}{
		{name: "healthy", state: degraded.State{}},
		{
			name:          "degraded",
			state:         degraded.State{Degraded: true, Since: since, Partition: []string{"node-a", "node-b"}, Sender: "node-a"},
			wantDegraded:  true,
			wantPartition: []string{"node-a", "node-b"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnvTweaked(t, false, func(d *Deps) { d.Degraded = fixedDegraded{st: tc.state} })
			res := e.do("GET", "/api/v1/health", "", nil)
			if res.code != http.StatusOK {
				t.Fatalf("health = %d", res.code)
			}
			h := decode[gen.Health](t, res)
			if h.Degraded != tc.wantDegraded {
				t.Errorf("degraded = %v, want %v", h.Degraded, tc.wantDegraded)
			}
			if !tc.wantDegraded {
				if h.DegradedSince != nil || h.Partition != nil {
					t.Errorf("degradedSince %v partition %v set while healthy", h.DegradedSince, h.Partition)
				}
				return
			}
			if h.DegradedSince == nil || !h.DegradedSince.Equal(since) {
				t.Errorf("degradedSince = %v, want %v", h.DegradedSince, since)
			}
			if h.Partition == nil || !slices.Equal(*h.Partition, tc.wantPartition) {
				t.Errorf("partition = %v, want %v", h.Partition, tc.wantPartition)
			}
		})
	}
}

// TestToAPIIncidentReevaluated maps the FA-UPT-07 flag of an incident.
func TestToAPIIncidentReevaluated(t *testing.T) {
	t.Parallel()
	inc := store.Incident{ID: "0b8f3c52-9d1e-4f6a-8c27-5e4d1a9b3f60", MonitorID: "0b8f3c52-9d1e-4f6a-8c27-5e4d1a9b3f61", StartedMs: 1, Confirmed: true, Reevaluated: true}
	out := toAPIIncident(inc)
	if out.Reevaluated == nil || !*out.Reevaluated {
		t.Errorf("reevaluated = %v, want true", out.Reevaluated)
	}
}

// TestHealthReportsStale covers the optional stale flag (ADR 0036): it is
// omitted when no check is wired and carries the check result otherwise.
func TestHealthReportsStale(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		stale func() bool
		want  *bool
	}{
		{name: "not wired", stale: nil, want: nil},
		{name: "fresh", stale: func() bool { return false }, want: boolPtr(false)},
		{name: "stale", stale: func() bool { return true }, want: boolPtr(true)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnvTweaked(t, false, func(d *Deps) { d.Stale = tc.stale })
			res := e.do("GET", "/api/v1/health", "", nil)
			if res.code != http.StatusOK {
				t.Fatalf("health = %d", res.code)
			}
			h := decode[gen.Health](t, res)
			switch {
			case tc.want == nil && h.Stale != nil:
				t.Errorf("stale = %v, want omitted", *h.Stale)
			case tc.want != nil && (h.Stale == nil || *h.Stale != *tc.want):
				t.Errorf("stale = %v, want %v", h.Stale, *tc.want)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }
