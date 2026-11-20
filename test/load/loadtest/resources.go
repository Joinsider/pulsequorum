// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// resourceSample is one reading of CPU and memory of the voter process.
// CPUPercent is in per-core units: 100 means one full core. HasCPU is false
// when no CPU value could be computed, e.g. the first reading of a cumulative
// CPU counter, which only yields an interval from the second reading on.
type resourceSample struct {
	At          time.Time `json:"at"`
	HasCPU      bool      `json:"hasCpu"`
	CPUPercent  float64   `json:"cpuPercent"`
	MemoryBytes uint64    `json:"memoryBytes"`
}

// sampleFunc reads one resourceSample.
type sampleFunc func(ctx context.Context) (resourceSample, error)

// dockerSampler reads `docker stats --no-stream` of a container. Podman's
// docker shim prints different JSON shapes, see parseDockerStats.
func dockerSampler(container string) sampleFunc {
	var meter cpuMeter
	return func(ctx context.Context) (resourceSample, error) {
		out, err := exec.CommandContext(ctx, "docker", "stats", "--no-stream", "--format", "{{json .}}", container).Output()
		if err != nil {
			return resourceSample{}, commandError("docker stats", err)
		}
		s, err := dockerSample(out, &meter, time.Now())
		if err != nil {
			return resourceSample{}, fmt.Errorf("parse docker stats output %q: %w", preview(out), err)
		}
		return s, nil
	}
}

// dockerSample converts the output of one `docker stats` call into a
// resourceSample. The meter keeps the cumulative CPU time between calls.
func dockerSample(out []byte, meter *cpuMeter, now time.Time) (resourceSample, error) {
	r, err := parseDockerStats(out)
	if err != nil {
		return resourceSample{}, err
	}
	s := resourceSample{At: now, MemoryBytes: r.memoryBytes}
	switch {
	case r.hasCPUTime:
		// The engine's own clock, when reported, belongs to the same reading
		// as the CPU time; otherwise the local clock is used.
		at := now
		if !r.engineTime.IsZero() {
			at = r.engineTime
		}
		s.CPUPercent, s.HasCPU = meter.update(r.cpuTime, at)
	case r.hasReportedCPU:
		s.CPUPercent, s.HasCPU = r.reportedCPU, true
	}
	return s, nil
}

// psSampler reads %CPU and RSS of a process with ps(1).
func psSampler(pid int) sampleFunc {
	return func(ctx context.Context) (resourceSample, error) {
		out, err := exec.CommandContext(ctx, "ps", "-o", "%cpu=,rss=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return resourceSample{}, commandError("ps", err)
		}
		cpu, mem, err := parsePSOutput(out)
		if err != nil {
			return resourceSample{}, fmt.Errorf("parse ps output %q: %w", preview(out), err)
		}
		return resourceSample{At: time.Now(), HasCPU: true, CPUPercent: cpu, MemoryBytes: mem}, nil
	}
}

// cpuMeter turns cumulative CPU time readings into percent of one core over
// the wall time between two readings.
type cpuMeter struct {
	ok   bool
	last time.Duration
	at   time.Time
}

// update returns the CPU percent since the previous reading. The first
// reading and a reading after a counter reset (the value went down) give
// ok false.
func (m *cpuMeter) update(cpu time.Duration, at time.Time) (percent float64, ok bool) {
	prev, prevAt, had := m.last, m.at, m.ok
	m.last, m.at, m.ok = cpu, at, true
	if !had {
		return 0, false
	}
	wall := at.Sub(prevAt)
	delta := cpu - prev
	if wall <= 0 || delta < 0 {
		return 0, false
	}
	return float64(delta) / float64(wall) * 100, true
}

func commandError(what string, err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return fmt.Errorf("%s: %w: %s", what, err, strings.TrimSpace(preview(ee.Stderr)))
	}
	return fmt.Errorf("%s: %w", what, err)
}

func firstLine(b []byte) []byte {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if line := bytes.TrimSpace(sc.Bytes()); len(line) > 0 {
			return line
		}
	}
	return nil
}

// preview returns at most 300 bytes of raw command output for diagnostics.
func preview(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

// dockerReading is the part of one container stats entry the tool uses.
type dockerReading struct {
	memoryBytes    uint64
	reportedCPU    float64 // percent as printed by the engine
	hasReportedCPU bool
	cpuTime        time.Duration // cumulative CPU time of the container
	hasCPUTime     bool
	engineTime     time.Time // engine clock of the reading, zero if not reported
}

// statsEntry is one JSON object of stats output, fields kept raw because
// the engines use numbers and strings for the same meaning.
type statsEntry map[string]json.RawMessage

// field returns the first present, non-null value of the given keys.
func (e statsEntry) field(keys ...string) (json.RawMessage, bool) {
	for _, k := range keys {
		if raw, ok := e[k]; ok && string(raw) != "null" {
			return raw, true
		}
	}
	return nil, false
}

// parseDockerStats parses the output of `docker stats --no-stream` with a
// `{{json .}}` or `json` format. Accepted shapes:
//
//   - Docker: one object per line, strings: {"CPUPerc":"12.34%","MemUsage":"85.2MB / 1.074GiB"}
//   - Podman object (`{{json .}}`): numbers, cumulative CPUNano and SystemNano:
//     {"CPU":54.02,"CPUNano":5149249000,"SystemNano":1792691263898244859,"MemUsage":32776192}
//   - Podman array (`json`): one array with strings, cumulative cpu_time:
//     [{"cpu_time":"1m53.007223s","cpu_percent":"10.42%","mem_usage":"56.14MB / 1.074GB"}]
//
// The cumulative CPU time is preferred over the printed percentage, which
// podman reports as an average since container start.
func parseDockerStats(out []byte) (dockerReading, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return dockerReading{}, errors.New("empty output")
	}
	var e statsEntry
	if trimmed[0] == '[' {
		var list []statsEntry
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return dockerReading{}, fmt.Errorf("not JSON: %w", err)
		}
		if len(list) == 0 {
			return dockerReading{}, errors.New("empty array")
		}
		e = list[0]
	} else if err := json.Unmarshal(firstLine(trimmed), &e); err != nil {
		return dockerReading{}, fmt.Errorf("not JSON: %w", err)
	}
	return readDockerEntry(e)
}

func readDockerEntry(e statsEntry) (dockerReading, error) {
	var r dockerReading

	raw, ok := e.field("MemUsage", "mem_usage")
	if !ok {
		return r, errors.New("no MemUsage or mem_usage field")
	}
	mem, err := parseMemUsage(raw)
	if err != nil {
		return r, fmt.Errorf("memory: %w", err)
	}
	r.memoryBytes = mem

	if raw, ok := e.field("CPUPerc", "CPU", "cpu_percent"); ok {
		v, err := strconv.ParseFloat(strings.TrimSuffix(rawText(raw), "%"), 64)
		if err != nil {
			return r, fmt.Errorf("CPU percent %s: %w", raw, err)
		}
		r.reportedCPU, r.hasReportedCPU = v, true
	}

	if raw, ok := e.field("CPUNano"); ok {
		ns, err := strconv.ParseInt(rawText(raw), 10, 64)
		if err != nil {
			return r, fmt.Errorf("CPUNano %s: %w", raw, err)
		}
		r.cpuTime, r.hasCPUTime = time.Duration(ns), true
	} else if raw, ok := e.field("cpu_time"); ok {
		d, err := time.ParseDuration(rawText(raw))
		if err != nil {
			return r, fmt.Errorf("cpu_time %s: %w", raw, err)
		}
		r.cpuTime, r.hasCPUTime = d, true
	}

	if raw, ok := e.field("SystemNano"); ok {
		ns, err := strconv.ParseInt(rawText(raw), 10, 64)
		if err != nil {
			return r, fmt.Errorf("SystemNano %s: %w", raw, err)
		}
		r.engineTime = time.Unix(0, ns)
	}

	if !r.hasReportedCPU && !r.hasCPUTime {
		return r, errors.New("no CPU field (CPUPerc, CPU, cpu_percent, CPUNano or cpu_time)")
	}
	return r, nil
}

// rawText returns the content of a JSON string or the token of a JSON number.
func rawText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(string(raw))
}

// parseMemUsage accepts a byte count (number) or "used / limit" (string) and
// returns the used part in bytes.
func parseMemUsage(raw json.RawMessage) (uint64, error) {
	text := rawText(raw)
	if n, err := strconv.ParseFloat(text, 64); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("negative size %q", text)
		}
		return uint64(math.Round(n)), nil
	}
	used, _, _ := strings.Cut(text, "/")
	return parseSize(used)
}

var sizeUnits = map[string]float64{
	"B":   1,
	"kB":  1e3,
	"KB":  1e3,
	"KiB": 1 << 10,
	"MB":  1e6,
	"MiB": 1 << 20,
	"GB":  1e9,
	"GiB": 1 << 30,
	"TB":  1e12,
	"TiB": 1 << 40,
}

// parseSize converts "85.2MB" or "1.074GiB" to bytes.
func parseSize(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	i := strings.IndexFunc(s, unicode.IsLetter)
	if i <= 0 {
		return 0, fmt.Errorf("no number and unit in %q", s)
	}
	num, err := strconv.ParseFloat(strings.TrimSpace(s[:i]), 64)
	if err != nil {
		return 0, err
	}
	mult, ok := sizeUnits[strings.TrimSpace(s[i:])]
	if !ok {
		return 0, fmt.Errorf("unknown unit in %q", s)
	}
	return uint64(math.Round(num * mult)), nil
}

// parsePSOutput parses `ps -o %cpu=,rss=` output: CPU percent and RSS in KiB.
func parsePSOutput(b []byte) (cpuPercent float64, memoryBytes uint64, err error) {
	fields := strings.Fields(string(b))
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("want 2 fields, got %d", len(fields))
	}
	cpuPercent, err = strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, 0, err
	}
	rssKiB, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return 0, 0, err
	}
	return cpuPercent, uint64(rssKiB * 1024), nil
}
