// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package selfcheck

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/joinsider/uptimecluster/internal/checker"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func TestParseTargets(t *testing.T) {
	tests := []struct {
		name    string
		entries []string
		want    []Target
		wantErr string
	}{
		{name: "none", entries: nil, want: nil},
		{name: "blank entries skipped", entries: []string{" ", ""}, want: nil},
		{
			name:    "tcp target",
			entries: []string{"example.com:443"},
			want:    []Target{{Label: "example.com:443", Addr: "example.com:443"}},
		},
		{
			name:    "ipv6 tcp target",
			entries: []string{"[2606:4700::1111]:53"},
			want:    []Target{{Label: "[2606:4700::1111]:53", Addr: "[2606:4700::1111]:53"}},
		},
		{
			name:    "https target keeps URL, label has no path or query",
			entries: []string{"https://example.com/ping?token=x"},
			want:    []Target{{Label: "https://example.com", URL: "https://example.com/ping?token=x"}},
		},
		{
			name:    "http target with port",
			entries: []string{" http://10.0.0.1:8080/health "},
			want:    []Target{{Label: "http://10.0.0.1:8080", URL: "http://10.0.0.1:8080/health"}},
		},
		{name: "no port", entries: []string{"nohostport"}, wantErr: "nohostport"},
		{name: "empty host", entries: []string{":443"}, wantErr: ":443"},
		{name: "empty port", entries: []string{"host:"}, wantErr: "host:"},
		{name: "port zero", entries: []string{"host:0"}, wantErr: "host:0"},
		{name: "port too large", entries: []string{"host:70000"}, wantErr: "host:70000"},
		{name: "port not numeric", entries: []string{"host:http"}, wantErr: "host:http"},
		{name: "other scheme", entries: []string{"ftp://example.com"}, wantErr: "ftp://example.com"},
		{name: "https without host", entries: []string{"https://"}, wantErr: "https://"},
		{name: "user info", entries: []string{"https://u:p@example.com"}, wantErr: "https://u:p@example.com"}, //nolint:gosec // G101: deliberate test credential, userinfo in a URL must be rejected
		{name: "fragment", entries: []string{"https://example.com/#x"}, wantErr: "https://example.com/#x"},
		{
			name:    "all bad entries are reported",
			entries: []string{"ok.example:1", "bad1", "ftp://bad2"},
			wantErr: "bad2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseTargets(tt.entries)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d targets, want %d: %+v", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("target %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestParseTargetsReportsEveryBadEntry(t *testing.T) {
	_, err := ParseTargets([]string{"bad1", "bad2"})
	if err == nil || !strings.Contains(err.Error(), "bad1") || !strings.Contains(err.Error(), "bad2") {
		t.Fatalf("error = %v, want both entries named", err)
	}
}

func TestNewRequiresDialer(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New without Dialer returned no error")
	}
}

// closedAddr returns a loopback address with nothing listening on it.
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// openListener accepts and immediately closes connections on a loopback port.
func openListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().String()
}

func newProber(t *testing.T, targets ...Target) *Prober {
	t.Helper()
	p, err := New(Options{
		Targets: targets,
		Dialer:  &checker.Dialer{},
		Timeout: 2 * time.Second,
		Now:     func() time.Time { return time.UnixMilli(1_700_000_000_123) },
		Logger:  slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func tcpTarget(addr string) Target { return Target{Label: addr, Addr: addr} }

func TestRoundSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("method = %s, want HEAD", r.Method)
		}
		w.WriteHeader(http.StatusInternalServerError) // any response counts as reachable
	}))
	defer srv.Close()
	open := openListener(t)

	p := newProber(t, tcpTarget(open), Target{Label: srv.URL, URL: srv.URL + "/health"})
	p.round(context.Background())
	st := p.Status()
	if !st.Configured || st.Failed || st.AtMs != 1_700_000_000_123 {
		t.Fatalf("status = %+v, want configured, not failed, at 1700000000123", st)
	}
}

func TestRoundAllFailed(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srvURL := srv.URL
	srv.Close() // the port is closed now: connection refused

	p := newProber(t, tcpTarget(closedAddr(t)), Target{Label: srvURL, URL: srvURL})
	p.round(context.Background())
	if st := p.Status(); !st.Configured || !st.Failed || st.AtMs == 0 {
		t.Fatalf("status = %+v, want configured and failed", st)
	}
}

// One reachable target out of several is enough to clear the failure.
func TestRoundOneOKIsNotFailed(t *testing.T) {
	p := newProber(t, tcpTarget(closedAddr(t)), tcpTarget(closedAddr(t)), tcpTarget(openListener(t)))
	p.round(context.Background())
	if st := p.Status(); st.Failed {
		t.Fatalf("status = %+v, want not failed", st)
	}

	p = newProber(t, tcpTarget(closedAddr(t)), tcpTarget(closedAddr(t)))
	p.round(context.Background())
	if st := p.Status(); !st.Failed {
		t.Fatalf("status = %+v, want failed when all targets fail", st)
	}
}

// A round that recovers clears the failed state of the previous round.
func TestRoundRecovers(t *testing.T) {
	addr := closedAddr(t)
	p := newProber(t, tcpTarget(addr))
	p.round(context.Background())
	if !p.Status().Failed {
		t.Fatal("first round should fail")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("cannot reuse port: %v", err)
	}
	defer func() { _ = ln.Close() }()
	p.round(context.Background())
	if p.Status().Failed {
		t.Fatal("second round should recover")
	}
}

// The SSRF policy of the dialer applies to self-checks too.
func TestRoundRespectsDialerPolicy(t *testing.T) {
	p, err := New(Options{
		Targets: []Target{tcpTarget(openListener(t))},
		Dialer:  &checker.Dialer{Policy: checker.BlockPrivate{}},
		Logger:  slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	p.round(context.Background())
	if !p.Status().Failed {
		t.Fatal("loopback target must be blocked by BlockPrivate")
	}
}

func TestStatusBeforeFirstRound(t *testing.T) {
	p := newProber(t, tcpTarget(openListener(t)))
	if st := p.Status(); st != (Status{Configured: true}) {
		t.Fatalf("status before first round = %+v, want configured only", st)
	}

	none := newProber(t)
	if st := none.Status(); st != (Status{}) {
		t.Fatalf("status without targets = %+v, want zero", st)
	}
}

// Run without targets returns at once; with targets it probes until the
// context is cancelled, and Status can be read concurrently meanwhile.
func TestRunStopsOnCancel(t *testing.T) {
	none := newProber(t)
	none.Run(context.Background())

	p, err := New(Options{
		Targets:  []Target{tcpTarget(openListener(t))},
		Dialer:   &checker.Dialer{},
		Interval: time.Millisecond,
		Logger:   slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for p.Status().AtMs == 0 && time.Now().Before(deadline) {
		_ = p.Status()
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if p.Status().AtMs == 0 {
		t.Fatal("Run did not complete a round")
	}
}
