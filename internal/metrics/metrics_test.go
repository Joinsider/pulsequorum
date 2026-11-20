// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package metrics

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func fakeSources() Sources {
	return Sources{
		RaftState: func() string { return "Leader" },
		RaftIndexes: func() (uint64, uint64, time.Duration) {
			return 42, 43, 150 * time.Millisecond
		},
		ReplicationLag: func() map[string]time.Duration {
			return map[string]time.Duration{"voter-2": 2 * time.Second, "voter-3": -1}
		},
		ReplicationDropped: func() map[string]uint64 { return map[string]uint64{"voter-2": 7} },
		DBSizes: func() map[string]struct{ Size, WAL int64 } {
			return map[string]struct{ Size, WAL int64 }{
				"state":   {Size: 1000, WAL: 10},
				"metrics": {Size: 2000, WAL: 20},
			}
		},
		DiskFree:      func() (float64, error) { return 0.5, nil },
		OutboxPending: func(context.Context) (int, error) { return 3, nil },
		Degraded:      func() bool { return false },
		SystemAlertsActive: func(context.Context) (map[string]int, error) {
			return map[string]int{"voter_offline": 1}, nil
		},
		ClockOffsets: func() map[string]time.Duration {
			return map[string]time.Duration{"voter-2": -250 * time.Millisecond}
		},
		Version: "1.2.3",
	}
}

func scrape(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func TestMetricNamesAreExported(t *testing.T) {
	m := New(fakeSources())
	m.CheckStarted("http")
	m.CheckFinished("http", "up", 40*time.Millisecond)
	m.AlertDelivered("system", "sent")
	body := scrape(t, m.Handler())

	want := []string{
		`pulsequorum_checks_total{result="up",type="http"} 1`,
		`pulsequorum_check_duration_seconds_bucket{type="http",le="0.05"} 1`,
		`pulsequorum_check_duration_seconds_count{type="http"} 1`,
		`pulsequorum_checks_running 0`,
		`pulsequorum_alert_deliveries_total{kind="system",outcome="sent"} 1`,
		`pulsequorum_raft_state{state="Leader"} 1`,
		`pulsequorum_raft_state{state="Follower"} 0`,
		`pulsequorum_raft_applied_index 42`,
		`pulsequorum_raft_commit_index 43`,
		`pulsequorum_raft_last_contact_seconds 0.15`,
		`pulsequorum_replication_lag_seconds{peer="voter-2"} 2`,
		`pulsequorum_replication_lag_seconds{peer="voter-3"} -1`,
		`pulsequorum_replication_dropped_total{peer="voter-2"} 7`,
		`pulsequorum_db_size_bytes{db="state"} 1000`,
		`pulsequorum_db_wal_bytes{db="metrics"} 20`,
		`pulsequorum_disk_free_ratio 0.5`,
		`pulsequorum_outbox_pending 3`,
		`pulsequorum_degraded 0`,
		`pulsequorum_system_alerts_active{kind="voter_offline"} 1`,
		`pulsequorum_clock_offset_seconds{peer="voter-2"} -0.25`,
		`pulsequorum_build_info{version="1.2.3"} 1`,
	}
	for _, line := range want {
		if !hasLine(body, line) {
			t.Errorf("missing line %q in:\n%s", line, body)
		}
	}
}

func TestNilSourcesAreNotExported(t *testing.T) {
	body := scrape(t, New(Sources{}).Handler())
	for _, name := range []string{
		"pulsequorum_raft_", "pulsequorum_replication_", "pulsequorum_db_",
		"pulsequorum_disk_free_ratio", "pulsequorum_outbox_pending", "pulsequorum_degraded",
		"pulsequorum_system_alerts_active", "pulsequorum_clock_offset_seconds",
	} {
		if strings.Contains(body, name) {
			t.Errorf("%s exported without its source:\n%s", name, body)
		}
	}
	if !hasLine(body, `pulsequorum_build_info{version="unknown"} 1`) {
		t.Errorf("build_info without version missing:\n%s", body)
	}
}

func TestFailingSourceIsSkipped(t *testing.T) {
	src := fakeSources()
	src.DiskFree = func() (float64, error) { return 0, errors.New("statfs failed") }
	src.OutboxPending = func(context.Context) (int, error) { return 0, errors.New("db closed") }
	body := scrape(t, New(src).Handler())
	if strings.Contains(body, "pulsequorum_disk_free_ratio") || strings.Contains(body, "pulsequorum_outbox_pending") {
		t.Errorf("failed sources still exported:\n%s", body)
	}
}

func TestContextSourcesGetDeadline(t *testing.T) {
	src := fakeSources()
	var deadlines []bool
	src.OutboxPending = func(ctx context.Context) (int, error) {
		_, ok := ctx.Deadline()
		deadlines = append(deadlines, ok)
		return 0, nil
	}
	src.SystemAlertsActive = func(ctx context.Context) (map[string]int, error) {
		_, ok := ctx.Deadline()
		deadlines = append(deadlines, ok)
		return nil, nil
	}
	scrape(t, New(src).Handler())
	if len(deadlines) != 2 || !deadlines[0] || !deadlines[1] {
		t.Errorf("context sources got deadlines %v, want both with a deadline", deadlines)
	}
}

func TestNoTenantOrMonitorLabels(t *testing.T) {
	m := New(fakeSources())
	m.CheckStarted("http")
	m.CheckFinished("http", "down", time.Millisecond)
	m.AlertDelivered("sys", "sent")
	body := scrape(t, m.Handler())

	allowed := map[string]bool{
		"type": true, "result": true, "kind": true, "outcome": true,
		"state": true, "peer": true, "db": true, "version": true, "le": true,
	}
	labelRe := regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="`)
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		open := strings.IndexByte(line, '{')
		if open < 0 {
			continue
		}
		end := strings.LastIndexByte(line, '}')
		for _, mm := range labelRe.FindAllStringSubmatch(line[open:end], -1) {
			if !allowed[mm[1]] {
				t.Errorf("unexpected label %q in %q", mm[1], line)
			}
		}
	}
}

func TestCheckObserverCounts(t *testing.T) {
	m := New(Sources{})
	m.CheckStarted("tcp")
	if body := scrape(t, m.Handler()); !hasLine(body, "pulsequorum_checks_running 1") {
		t.Errorf("running gauge not 1 during check:\n%s", body)
	}
	m.CheckFinished("tcp", "degraded", 2*time.Second)
	body := scrape(t, m.Handler())
	for _, line := range []string{
		`pulsequorum_checks_total{result="degraded",type="tcp"} 1`,
		`pulsequorum_checks_running 0`,
		`pulsequorum_check_duration_seconds_bucket{type="tcp",le="2.5"} 1`,
	} {
		if !hasLine(body, line) {
			t.Errorf("missing %q in:\n%s", line, body)
		}
	}
}

func TestRegistriesAreIndependent(t *testing.T) {
	a := New(Sources{Version: "a"})
	b := New(Sources{Version: "b"})
	a.CheckFinished("http", "up", time.Millisecond)
	if body := scrape(t, b.Handler()); strings.Contains(body, `type="http"`) {
		t.Errorf("registry b sees metrics of a:\n%s", body)
	}
}

func TestServeRequiresToken(t *testing.T) {
	tests := []struct {
		name, token, auth, path string
		want                    int
	}{
		{"no token configured", "", "", "/metrics", http.StatusOK},
		{"missing header", "s3cret", "", "/metrics", http.StatusUnauthorized},
		{"wrong token", "s3cret", "Bearer nope", "/metrics", http.StatusUnauthorized},
		{"right token", "s3cret", "Bearer s3cret", "/metrics", http.StatusOK},
		{"other path is not served", "", "", "/api/v1/health", http.StatusNotFound},
		{"POST is refused", "", "", "/metrics", http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			m := New(Sources{})
			go func() { done <- m.serve(ctx, ln, tt.token, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
			defer func() {
				cancel()
				if err := <-done; err != nil {
					t.Errorf("serve returned %v after shutdown", err)
				}
			}()

			method := http.MethodGet
			if tt.want == http.StatusMethodNotAllowed {
				method = http.MethodPost
			}
			req, _ := http.NewRequest(method, "http://"+ln.Addr().String()+tt.path, nil)
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tt.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}
}

func TestServeAddressInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if err := New(Sources{}).Serve(context.Background(), ln.Addr().String(), "", nil); err == nil {
		t.Error("Serve on a busy address returned nil")
	}
}

func hasLine(body, line string) bool {
	for _, l := range strings.Split(body, "\n") {
		if l == line {
			return true
		}
	}
	return false
}
