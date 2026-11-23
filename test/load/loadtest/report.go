// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Thresholds from NFA-PERF-01 to NFA-PERF-04 (docs/pflichtenheft.md).
const (
	// cpuMeanLimit is in per-core units (docker and ps report 100 % per
	// full core): 50 % of 2 vCPUs is 100 %.
	cpuMeanLimit     = 100.0
	memoryLimitBytes = 512 << 20
	apiP95LimitMs    = 200.0
	statusMaxLimitMs = 2000.0

	reqNFAPerf0102       = "NFA-PERF-01/02"
	critCPUMeanLimit     = "CPU mean < 100 % (50 % of 2 vCPUs)"
	critNoUnexpectedDown = "no unexpected DOWN"
	measuredNoSamples    = "no samples"
	resultNoData         = "NO DATA"
)

// apiSample is one API timing. Group is "monitors_list" (complete listing
// with all pages) or "uptime".
type apiSample struct {
	Group      string    `json:"group"`
	At         time.Time `json:"at"`
	DurationMs float64   `json:"durationMs"`
	Count      int       `json:"count,omitempty"`
	Range      string    `json:"range,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// statusSample is one flip of a target and the matching state event.
// LatencyMs = received - event.ts (commit to client); FlipToEventMs is
// informational and depends on the check interval.
type statusSample struct {
	MonitorID     string    `json:"monitorId"`
	TargetIndex   int       `json:"targetIndex"`
	Direction     string    `json:"direction"`
	FlippedAt     time.Time `json:"flippedAt"`
	EventTS       time.Time `json:"eventTs,omitzero"`
	ReceivedAt    time.Time `json:"receivedAt,omitzero"`
	LatencyMs     float64   `json:"latencyMs"`
	FlipToEventMs float64   `json:"flipToEventMs"`
	Timeout       bool      `json:"timeout,omitempty"`
	// LostDuringReconnect marks a wait during which the stream was down, so
	// the event may have been sent while nothing was connected.
	LostDuringReconnect bool   `json:"lostDuringReconnect,omitempty"`
	Error               string `json:"error,omitempty"`
}

type groupStats struct {
	Group  string `json:"group"`
	Errors int    `json:"errors"`
	distribution
}

type statusSummary struct {
	Flips       int          `json:"flips"`
	Timeouts    int          `json:"timeouts"`
	Lost        int          `json:"lostDuringReconnect"`
	Errors      int          `json:"errors"`
	Connections int          `json:"streamConnections"`
	Reconnects  int          `json:"streamReconnects"`
	Latency     distribution `json:"latency"`
	FlipToEvent distribution `json:"flipToEvent"`
}

type resourceSummary struct {
	Source         string   `json:"source"`
	Samples        int      `json:"samples"`
	CPUSamples     int      `json:"cpuSamples"`
	Errors         int      `json:"errors"`
	MeanCPUPercent float64  `json:"meanCpuPercent"`
	MaxCPUPercent  float64  `json:"maxCpuPercent"`
	MaxMemoryBytes uint64   `json:"maxMemoryBytes"`
	ErrorSamples   []string `json:"errorSamples,omitempty"`
}

type verdict struct {
	Requirement string `json:"requirement"`
	Criterion   string `json:"criterion"`
	Measured    string `json:"measured"`
	Result      string `json:"result"` // PASS, FAIL or NO DATA
}

type report struct {
	StartedAt               time.Time         `json:"startedAt"`
	EndedAt                 time.Time         `json:"endedAt"`
	Parameters              map[string]string `json:"parameters"`
	Monitors                int               `json:"monitors"`
	MonitorsWithTarget      int               `json:"monitorsWithTarget"`
	IntervalSeconds         int               `json:"intervalSeconds"`
	ExpectedChecksPerMinute float64           `json:"expectedChecksPerMinute"`
	API                     []groupStats      `json:"api"`
	Status                  statusSummary     `json:"status"`
	Unexpected              unexpectedSummary `json:"unexpectedStatus"`
	Resources               resourceSummary   `json:"resources"`
	Verdicts                []verdict         `json:"verdicts"`
	APISamples              []apiSample       `json:"apiSamples"`
	StatusSamples           []statusSample    `json:"statusSamples"`
	ResourceSamples         []resourceSample  `json:"resourceSamples"`
}

func buildReport(opts measureOptions, monitors []monitorRef, rec *recorder, startedAt, endedAt time.Time) report {
	rec.mu.Lock()
	defer rec.mu.Unlock()

	r := report{
		StartedAt: startedAt,
		EndedAt:   endedAt,
		Parameters: map[string]string{
			"base":             opts.base,
			"user":             opts.user,
			"duration":         opts.duration.String(),
			"targetsControl":   opts.targetsControl,
			"apiInterval":      opts.apiInterval.String(),
			"resourceInterval": opts.resInterval.String(),
			"statusInterval":   opts.statusInterval.String(),
			"statusTimeout":    opts.statusTimeout.String(),
		},
		Monitors:        len(monitors),
		IntervalSeconds: monitors[0].Interval,
		APISamples:      rec.api,
		StatusSamples:   rec.status,
		ResourceSamples: rec.resources,
	}
	if opts.container != "" {
		r.Parameters["container"] = opts.container
	}
	if opts.pid != 0 {
		r.Parameters["pid"] = fmt.Sprint(opts.pid)
	}
	for _, m := range monitors {
		if m.TargetIndex >= 0 {
			r.MonitorsWithTarget++
		}
	}
	if r.IntervalSeconds > 0 {
		r.ExpectedChecksPerMinute = float64(r.Monitors) * 60 / float64(r.IntervalSeconds)
	}

	r.API = apiStats(rec.api)
	r.Status = statusStats(rec.status, rec.streamConnections, rec.streamReconnects)
	r.Unexpected = unexpectedChanges(rec.stateEvents, rec.flipped)
	r.Resources = resourceStats(opts, rec)
	r.Verdicts = verdicts(r)
	return r
}

func apiStats(samples []apiSample) []groupStats {
	var out []groupStats
	for _, group := range []string{"monitors_list", "uptime"} {
		var vals []float64
		g := groupStats{Group: group}
		for _, s := range samples {
			if s.Group != group {
				continue
			}
			if s.Error != "" {
				g.Errors++
				continue
			}
			vals = append(vals, s.DurationMs)
		}
		g.distribution = summarize(vals)
		out = append(out, g)
	}
	return out
}

func statusStats(samples []statusSample, connections, reconnects int) statusSummary {
	s := statusSummary{Flips: len(samples), Connections: connections, Reconnects: reconnects}
	var lat, flip []float64
	for _, x := range samples {
		switch {
		case x.LostDuringReconnect:
			s.Lost++
		case x.Timeout:
			s.Timeouts++
		case x.Error != "":
			s.Errors++
		default:
			lat = append(lat, x.LatencyMs)
			flip = append(flip, x.FlipToEventMs)
		}
	}
	s.Latency = summarize(lat)
	s.FlipToEvent = summarize(flip)
	return s
}

func resourceStats(opts measureOptions, rec *recorder) resourceSummary {
	rs := resourceSummary{Samples: len(rec.resources), Errors: rec.resErrN, ErrorSamples: rec.resErrs}
	switch {
	case opts.container != "":
		rs.Source = "docker stats " + opts.container
	case opts.pid != 0:
		rs.Source = fmt.Sprintf("ps -p %d", opts.pid)
	default:
		rs.Source = "none"
	}
	var sum float64
	for _, s := range rec.resources {
		rs.MaxMemoryBytes = max(rs.MaxMemoryBytes, s.MemoryBytes)
		if !s.HasCPU {
			continue
		}
		rs.CPUSamples++
		sum += s.CPUPercent
		rs.MaxCPUPercent = max(rs.MaxCPUPercent, s.CPUPercent)
	}
	if rs.CPUSamples > 0 {
		rs.MeanCPUPercent = sum / float64(rs.CPUSamples)
	}
	return rs
}

func verdicts(r report) []verdict {
	var out []verdict
	if r.Resources.Samples == 0 {
		out = append(out, verdict{reqNFAPerf0102, critCPUMeanLimit, measuredNoSamples, resultNoData})
		out = append(out, verdict{reqNFAPerf0102, "memory < 512 MiB", measuredNoSamples, resultNoData})
	} else {
		cpu := verdict{reqNFAPerf0102, critCPUMeanLimit, "no CPU interval yet", resultNoData}
		if r.Resources.CPUSamples > 0 {
			cpu = verdict{reqNFAPerf0102, critCPUMeanLimit,
				fmt.Sprintf("%.1f %% (max %.1f %%, %d samples)", r.Resources.MeanCPUPercent, r.Resources.MaxCPUPercent, r.Resources.CPUSamples),
				passIf(r.Resources.MeanCPUPercent < cpuMeanLimit)}
		}
		out = append(out, cpu)
		out = append(out, verdict{reqNFAPerf0102, "memory < 512 MiB",
			fmt.Sprintf("%.1f MiB max", float64(r.Resources.MaxMemoryBytes)/(1<<20)),
			passIf(r.Resources.MaxMemoryBytes < memoryLimitBytes)})
	}
	for _, g := range r.API {
		label := "monitor list (complete listing)"
		if g.Group == "uptime" {
			label = "uptime"
		}
		if g.Count == 0 {
			out = append(out, verdict{"NFA-PERF-03", "p95 < 200 ms, " + label, measuredNoSamples, resultNoData})
			continue
		}
		out = append(out, verdict{"NFA-PERF-03", "p95 < 200 ms, " + label,
			fmt.Sprintf("p95 %.1f ms", g.P95), passIf(g.P95 < apiP95LimitMs)})
	}
	st := r.Status
	if st.Latency.Count == 0 {
		out = append(out, verdict{"NFA-PERF-04", "status change visible <= 2 s after commit", measuredNoSamples, resultNoData})
	} else {
		ok := st.Latency.Max <= statusMaxLimitMs && st.Timeouts == 0
		out = append(out, verdict{"NFA-PERF-04", "status change visible <= 2 s after commit (all samples, no timeouts)",
			fmt.Sprintf("max %.0f ms, p95 %.0f ms, timeouts %d, lost during reconnect %d", st.Latency.Max, st.Latency.P95, st.Timeouts, st.Lost),
			passIf(ok)})
	}
	if r.Status.Connections == 0 {
		out = append(out, verdict{"-", critNoUnexpectedDown, "stream never connected", resultNoData})
	} else {
		out = append(out, verdict{"-", critNoUnexpectedDown,
			fmt.Sprintf("%d unexpected DOWN, %d PENDING on %d monitors", r.Unexpected.Down, r.Unexpected.Pending, r.Unexpected.Monitors),
			passIf(r.Unexpected.Down == 0)})
	}
	return out
}

func passIf(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

func renderMarkdown(r report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Pulsequorum load test report\n\n")
	fmt.Fprintf(&b, "- Start: %s\n", r.StartedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "- End: %s\n", r.EndedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "- Elapsed: %s\n\n", r.EndedAt.Sub(r.StartedAt).Round(time.Second))

	b.WriteString("## Parameters\n\n| Parameter | Value |\n|---|---|\n")
	keys := make([]string, 0, len(r.Parameters))
	for k := range r.Parameters {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "| %s | %s |\n", k, r.Parameters[k])
	}

	b.WriteString("\n## Monitors and expected load\n\n")
	fmt.Fprintf(&b, "- Monitors: %d (%d with target farm URL)\n", r.Monitors, r.MonitorsWithTarget)
	fmt.Fprintf(&b, "- Check interval: %d s\n", r.IntervalSeconds)
	fmt.Fprintf(&b, "- Expected checks per minute: %.0f (heartbeats are not counted, this is the nominal rate)\n\n", r.ExpectedChecksPerMinute)

	b.WriteString("## API latency\n\n| Endpoint | Count | Errors | p50 ms | p95 ms | p99 ms | max ms | mean ms |\n|---|---|---|---|---|---|---|---|\n")
	for _, g := range r.API {
		name := "GET /api/v1/monitors?limit=500 (complete listing)"
		if g.Group == "uptime" {
			name = "GET /api/v1/monitors/{id}/uptime (random monitor, random range)"
		}
		fmt.Fprintf(&b, "| %s | %d | %d | %.1f | %.1f | %.1f | %.1f | %.1f |\n",
			name, g.Count, g.Errors, g.P50, g.P95, g.P99, g.Max, g.Mean)
	}

	st := r.Status
	b.WriteString("\n## Status-change latency\n\n")
	fmt.Fprintf(&b, "Flips: %d, timeouts: %d, lost during reconnect: %d, errors: %d\n\n", st.Flips, st.Timeouts, st.Lost, st.Errors)
	fmt.Fprintf(&b, "Stream connections: %d, reconnects: %d (each reconnect logs in again and requests a new ticket)\n\n", st.Connections, st.Reconnects)
	b.WriteString("| Metric | Count | p50 ms | p95 ms | p99 ms | max ms |\n|---|---|---|---|---|---|\n")
	fmt.Fprintf(&b, "| received_at - event.ts (commit to client) | %d | %.0f | %.0f | %.0f | %.0f |\n",
		st.Latency.Count, st.Latency.P50, st.Latency.P95, st.Latency.P99, st.Latency.Max)
	fmt.Fprintf(&b, "| flip to event (informational, depends on check interval) | %d | %.0f | %.0f | %.0f | %.0f |\n",
		st.FlipToEvent.Count, st.FlipToEvent.P50, st.FlipToEvent.P95, st.FlipToEvent.P99, st.FlipToEvent.Max)

	u := r.Unexpected
	b.WriteString("\n## Unexpected status changes\n\n")
	b.WriteString("DOWN and PENDING state events of monitors the tool did not flip (false alarms).\n\n")
	fmt.Fprintf(&b, "- DOWN: %d, PENDING: %d, monitors affected: %d\n", u.Down, u.Pending, u.Monitors)
	for _, e := range u.First {
		fmt.Fprintf(&b, "- first: %s %s at event.ts %s (received %s)\n", e.Status, e.MonitorID,
			e.EventTS.Format(time.RFC3339Nano), e.ReceivedAt.Format(time.RFC3339Nano))
	}
	fmt.Fprintf(&b, "- no unexpected DOWN: %s\n", verdictFor(r, critNoUnexpectedDown))

	rs := r.Resources
	b.WriteString("\n## Resource usage\n\n")
	fmt.Fprintf(&b, "- Source: %s\n- Samples: %d (with CPU value: %d), errors: %d\n", rs.Source, rs.Samples, rs.CPUSamples, rs.Errors)
	fmt.Fprintf(&b, "- CPU mean %.1f %%, max %.1f %% (per-core units: 100 %% = one full core)\n", rs.MeanCPUPercent, rs.MaxCPUPercent)
	fmt.Fprintf(&b, "- Memory max %.1f MiB\n", float64(rs.MaxMemoryBytes)/(1<<20))
	for _, e := range rs.ErrorSamples {
		fmt.Fprintf(&b, "- Raw sample error: %s\n", e)
	}

	b.WriteString("\n## Verdicts\n\n| Requirement | Criterion | Measured | Result |\n|---|---|---|---|\n")
	for _, v := range r.Verdicts {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", v.Requirement, v.Criterion, v.Measured, v.Result)
	}

	b.WriteString("\n## Notes\n\n")
	b.WriteString("- CPU is taken from docker stats (or ps) in per-core units; the limit of 50 % of 2 vCPUs therefore becomes 100 %. For docker stats with a cumulative CPU counter, the percentage is computed per interval from the counter deltas; the printed percentage is only used without a counter.\n")
	b.WriteString("- A status wait during which the stream was down is reported as lost during reconnect, not as a timeout.\n")
	b.WriteString("- Status latency uses event.ts (set by the voter when the change is committed) and the local receive time; both clocks must be synchronized, so run the load generator on the voter host or with NTP.\n")
	b.WriteString("- Status cycles run every -status-interval for one random monitor each; down and up flips are measured separately.\n")
	return b.String()
}

// verdictFor returns the result of the verdict with the given criterion, or NO DATA.
func verdictFor(r report, criterion string) string {
	for _, v := range r.Verdicts {
		if v.Criterion == criterion {
			return v.Result
		}
	}
	return resultNoData
}
