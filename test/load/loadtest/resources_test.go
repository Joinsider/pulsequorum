// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package main

import (
	"math"
	"testing"
	"time"
)

// Sample strings as printed by the podman shim and by docker. The podman
// object is cut down to the fields the parser uses; the array sample keeps
// its original fields with the elided net_io/block_io values replaced.
const (
	podmanObject1 = `{"AvgCPU":54.02,"ContainerID":"0aa71745208f","Name":"ucload","CPU":54.02,"CPUNano":5149249000,"CPUSystemNano":641847,"SystemNano":1792691263898244859,"MemUsage":32776192,"MemLimit":1073741824,"MemPerc":3.05}`
	// 2 s more CPU time over 4 s of engine time: 50 % of one core.
	podmanObject2 = `{"AvgCPU":40.10,"ContainerID":"0aa71745208f","Name":"ucload","CPU":40.10,"CPUNano":7149249000,"CPUSystemNano":641900,"SystemNano":1792691267898244859,"MemUsage":32776192,"MemLimit":1073741824,"MemPerc":3.05}`
	podmanArray1  = `[{"id":"0aa71745208f","name":"ucload","cpu_time":"1m53.007223s","cpu_percent":"10.42%","avg_cpu":"10.42%","mem_usage":"56.14MB / 1.074GB","mem_percent":"5.23%","net_io":"0B / 0B","block_io":"0B / 0B","pids":"12"}]`
	// 2 s more CPU time (1m55.007223s) over 4 s of local time: 50 %.
	podmanArray2 = `[{"id":"0aa71745208f","name":"ucload","cpu_time":"1m55.007223s","cpu_percent":"10.42%","avg_cpu":"10.42%","mem_usage":"56.14MB / 1.074GB","mem_percent":"5.23%","net_io":"0B / 0B","block_io":"0B / 0B","pids":"12"}]`
	dockerLine   = `{"CPUPerc":"12.34%","MemUsage":"85.2MB / 1.074GiB"}`
)

func TestParseDockerStats(t *testing.T) {
	tests := []struct {
		name       string
		out        string
		wantCPU    float64
		wantHasCPU bool // reported percentage used (no cumulative time)
		wantTime   time.Duration
		wantTimeOK bool
		wantMem    uint64
		wantErr    bool
	}{
		{name: "docker format", out: dockerLine, wantCPU: 12.34, wantHasCPU: true, wantMem: 85_200_000},
		{name: "docker binary units", out: `{"CPUPerc":"0.00%","MemUsage":"10MiB / 1GiB"}`, wantCPU: 0, wantHasCPU: true, wantMem: 10 << 20},
		{name: "docker CPU string field", out: `{"CPU":"12.34%","MemUsage":"85.2MB / 1.074GB"}`, wantCPU: 12.34, wantHasCPU: true, wantMem: 85_200_000},
		{name: "CPUPerc wins over CPU", out: `{"CPUPerc":"1.5%","CPU":"9%","MemUsage":"1kB / 2kB"}`, wantCPU: 1.5, wantHasCPU: true, wantMem: 1000},
		{name: "podman object numbers", out: podmanObject1, wantCPU: 54.02, wantHasCPU: true, wantTime: 5149249000, wantTimeOK: true, wantMem: 32776192},
		{name: "podman array strings", out: podmanArray1, wantCPU: 10.42, wantHasCPU: true, wantTime: 113007223 * time.Microsecond, wantTimeOK: true, wantMem: 56_140_000},
		{name: "podman array with cumulative time only", out: `[{"cpu_time":"850ms","mem_usage":"1.5GB / 2GB"}]`, wantTime: 850 * time.Millisecond, wantTimeOK: true, wantMem: 1_500_000_000},
		{name: "missing cpu", out: `{"MemUsage":"85.2MB / 1.074GB"}`, wantErr: true},
		{name: "missing memory", out: `{"CPUPerc":"1%"}`, wantErr: true},
		{name: "unknown unit", out: `{"CPUPerc":"1%","MemUsage":"5XB / 1GB"}`, wantErr: true},
		{name: "bad cpu_time", out: `[{"cpu_time":"soon","mem_usage":"1MB / 1GB"}]`, wantErr: true},
		{name: "empty array", out: `[]`, wantErr: true},
		{name: "not json", out: `CPU 12%`, wantErr: true},
		{name: "empty", out: ``, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := parseDockerStats([]byte(tt.out))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if r.memoryBytes != tt.wantMem {
				t.Errorf("memory = %d, want %d", r.memoryBytes, tt.wantMem)
			}
			if r.hasReportedCPU != tt.wantHasCPU || (tt.wantHasCPU && r.reportedCPU != tt.wantCPU) {
				t.Errorf("reported CPU = %v (%v), want %v (%v)", r.reportedCPU, r.hasReportedCPU, tt.wantCPU, tt.wantHasCPU)
			}
			if r.hasCPUTime != tt.wantTimeOK || r.cpuTime != tt.wantTime {
				t.Errorf("cpu time = %v (%v), want %v (%v)", r.cpuTime, r.hasCPUTime, tt.wantTime, tt.wantTimeOK)
			}
		})
	}
}

func TestParseDockerStatsEngineTime(t *testing.T) {
	r, err := parseDockerStats([]byte(podmanObject1))
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Unix(0, 1792691263898244859); !r.engineTime.Equal(want) {
		t.Errorf("engine time = %v, want %v", r.engineTime, want)
	}
	r, err = parseDockerStats([]byte(podmanArray1))
	if err != nil {
		t.Fatal(err)
	}
	if !r.engineTime.IsZero() {
		t.Errorf("array shape has no engine clock, got %v", r.engineTime)
	}
}

func TestDockerSampleCPUFromCumulativeTime(t *testing.T) {
	t0 := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

	t.Run("podman object uses CPUNano over SystemNano", func(t *testing.T) {
		var m cpuMeter
		first, err := dockerSample([]byte(podmanObject1), &m, t0)
		if err != nil {
			t.Fatal(err)
		}
		if first.HasCPU {
			t.Errorf("first reading must not yield CPU, got %v", first.CPUPercent)
		}
		if first.MemoryBytes != 32776192 {
			t.Errorf("memory = %d", first.MemoryBytes)
		}
		second, err := dockerSample([]byte(podmanObject2), &m, t0.Add(5*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if !second.HasCPU || math.Abs(second.CPUPercent-50) > 1e-9 {
			t.Errorf("CPU = %v (has %v), want 50 %%; the printed 40.10 must not be used", second.CPUPercent, second.HasCPU)
		}
	})

	t.Run("array uses cpu_time over local time", func(t *testing.T) {
		var m cpuMeter
		if _, err := dockerSample([]byte(podmanArray1), &m, t0); err != nil {
			t.Fatal(err)
		}
		s, err := dockerSample([]byte(podmanArray2), &m, t0.Add(4*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if !s.HasCPU || math.Abs(s.CPUPercent-50) > 1e-9 {
			t.Errorf("CPU = %v (has %v), want 50 %%", s.CPUPercent, s.HasCPU)
		}
		if s.MemoryBytes != 56_140_000 {
			t.Errorf("memory = %d, want 56140000", s.MemoryBytes)
		}
	})

	t.Run("docker percentage is used without cumulative time", func(t *testing.T) {
		var m cpuMeter
		s, err := dockerSample([]byte(dockerLine), &m, t0)
		if err != nil {
			t.Fatal(err)
		}
		if !s.HasCPU || s.CPUPercent != 12.34 {
			t.Errorf("CPU = %v (has %v), want 12.34", s.CPUPercent, s.HasCPU)
		}
	})
}

func TestCPUMeter(t *testing.T) {
	t0 := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		cpu     []time.Duration
		at      []time.Duration
		wantPct []float64 // one entry per reading; NaN means no value
	}{
		{
			name:    "one core busy for 4 s of 4 s",
			cpu:     []time.Duration{10 * time.Second, 14 * time.Second},
			at:      []time.Duration{0, 4 * time.Second},
			wantPct: []float64{math.NaN(), 100},
		},
		{
			name:    "counter reset yields no value",
			cpu:     []time.Duration{10 * time.Second, 2 * time.Second, 3 * time.Second},
			at:      []time.Duration{0, 1 * time.Second, 2 * time.Second},
			wantPct: []float64{math.NaN(), math.NaN(), 100},
		},
		{
			name:    "zero wall time yields no value",
			cpu:     []time.Duration{0, time.Second},
			at:      []time.Duration{time.Second, time.Second},
			wantPct: []float64{math.NaN(), math.NaN()},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var m cpuMeter
			for i := range tt.cpu {
				pct, ok := m.update(tt.cpu[i], t0.Add(tt.at[i]))
				want := tt.wantPct[i]
				if math.IsNaN(want) {
					if ok {
						t.Errorf("reading %d: got %v, want no value", i, pct)
					}
					continue
				}
				if !ok || math.Abs(pct-want) > 1e-9 {
					t.Errorf("reading %d: got %v (ok %v), want %v", i, pct, ok, want)
				}
			}
		})
	}
}

func TestParseMemUsage(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want uint64
	}{
		{"number bytes", `32776192`, 32776192},
		{"decimal units", `"56.14MB / 1.074GB"`, 56_140_000},
		{"binary units", `"56MiB / 1GiB"`, 56 << 20},
		{"no limit part", `"1.5GB"`, 1_500_000_000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMemUsage([]byte(tt.raw))
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestParsePSOutput(t *testing.T) {
	cpu, mem, err := parsePSOutput([]byte("  3.5  20480\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cpu != 3.5 || mem != 20480*1024 {
		t.Errorf("got cpu=%v mem=%d", cpu, mem)
	}
	if _, _, err := parsePSOutput([]byte("garbage")); err == nil {
		t.Error("want error for malformed output")
	}
}
