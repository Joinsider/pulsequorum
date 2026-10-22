// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package main

import (
	"strings"
	"testing"
	"time"
)

func TestUnexpectedDownVerdict(t *testing.T) {
	start := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	monitors := []monitorRef{{ID: "flipped", Interval: 60, TargetIndex: 1}, {ID: "quiet", Interval: 60, TargetIndex: 2}}
	tests := []struct {
		name   string
		events []stateEvent
		want   string
	}{
		{"no events", nil, "PASS"},
		{"DOWN of the flipped monitor is expected", []stateEvent{{MonitorID: "flipped", Status: "DOWN"}}, "PASS"},
		{"DOWN of a quiet monitor fails", []stateEvent{{MonitorID: "quiet", Status: "DOWN"}}, "FAIL"},
		{"PENDING alone does not fail the DOWN verdict", []stateEvent{{MonitorID: "quiet", Status: "PENDING"}}, "PASS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recorder{streamConnections: 1, stateEvents: tt.events}
			rec.markFlipped("flipped")
			rep := buildReport(measureOptions{duration: time.Minute}, monitors, rec, start, start.Add(time.Minute))
			if got := verdictFor(rep, "no unexpected DOWN"); got != tt.want {
				t.Errorf("verdict = %s, want %s", got, tt.want)
			}
			md := renderMarkdown(rep)
			if !strings.Contains(md, "- no unexpected DOWN: "+tt.want) {
				t.Errorf("markdown lacks the verdict line %q", "- no unexpected DOWN: "+tt.want)
			}
		})
	}
}

func TestUnexpectedVerdictWithoutStreamIsNoData(t *testing.T) {
	start := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	rec := &recorder{}
	rep := buildReport(measureOptions{duration: time.Minute}, []monitorRef{{ID: "m", Interval: 60, TargetIndex: -1}}, rec, start, start)
	if got := verdictFor(rep, "no unexpected DOWN"); got != "NO DATA" {
		t.Errorf("verdict = %s, want NO DATA", got)
	}
}

func TestCPUStatsSkipSamplesWithoutValue(t *testing.T) {
	start := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	rec := &recorder{resources: []resourceSample{
		{At: start, HasCPU: false, MemoryBytes: 100},
		{At: start, HasCPU: true, CPUPercent: 40, MemoryBytes: 200},
		{At: start, HasCPU: true, CPUPercent: 60, MemoryBytes: 150},
	}}
	rs := resourceStats(measureOptions{container: "uc"}, rec)
	if rs.Samples != 3 || rs.CPUSamples != 2 || rs.MeanCPUPercent != 50 || rs.MaxCPUPercent != 60 || rs.MaxMemoryBytes != 200 {
		t.Errorf("resource stats = %+v", rs)
	}
}
