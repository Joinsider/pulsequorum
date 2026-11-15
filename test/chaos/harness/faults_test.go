// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package harness

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

// scripted answers runtime calls with a handler, so tests can set exact
// answers and record every call in order.
type scripted struct {
	calls   []string
	handler func(args []string) (string, error)
}

func (s *scripted) exec(_ context.Context, _ string, args []string) ([]byte, []byte, error) {
	s.calls = append(s.calls, strings.Join(args, " "))
	out, err := s.handler(args)
	var stderr []byte
	if err != nil {
		stderr = []byte(err.Error())
	}
	return []byte(out), stderr, err
}

// clusterWithScript returns a cluster of three voters whose runtime answers
// through handler. The handler is called for every runtime call.
func clusterWithScript(t *testing.T, handler func(args []string) (string, error)) (*Cluster, *scripted) {
	t.Helper()
	sc := &scripted{handler: handler}
	rt := newRuntimeWithExec("podman", sc.exec)
	cfg := DefaultConfig()
	cfg.Voters = 3
	c, err := NewCluster(cfg, rt, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Teardown(context.Background()) })
	return c, sc
}

// faultHandler answers the inspect calls of a three-voter cluster: all
// voters and sidecars run, and voter v has address 10.89.0.<v+1>.
func faultHandler(args []string) (string, error) {
	joined := strings.Join(args, " ")
	switch {
	case strings.HasPrefix(joined, "inspect --format {{.State.Running}}"):
		return "true", nil
	case strings.HasPrefix(joined, "inspect --format {{json .NetworkSettings.Networks}} ucchaos-v"):
		name := args[len(args)-1]
		for v := 1; v <= 3; v++ {
			if name == "ucchaos-v"+string(rune('0'+v)) {
				return `{"ucchaos-net":{"IPAddress":"10.89.0.` + string(rune('0'+v+1)) + `"}}`, nil
			}
		}
	}
	return "", nil
}

func TestIsolateDropsBothDirectionsToPeers(t *testing.T) {
	c, sc := clusterWithScript(t, faultHandler)
	if err := c.Isolate(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"exec ucchaos-v3-netem iptables -F INPUT",
		"exec ucchaos-v3-netem iptables -F OUTPUT",
		"exec ucchaos-v3-netem iptables -I INPUT -s 10.89.0.2 -j DROP",
		"exec ucchaos-v3-netem iptables -I OUTPUT -d 10.89.0.2 -j DROP",
		"exec ucchaos-v3-netem iptables -I INPUT -s 10.89.0.3 -j DROP",
		"exec ucchaos-v3-netem iptables -I OUTPUT -d 10.89.0.3 -j DROP",
	}
	assertContainsInOrder(t, sc.calls, want)
}

func TestIsolateOneWayOnlyTouchesOneChain(t *testing.T) {
	c, sc := clusterWithScript(t, faultHandler)
	if err := c.IsolateOneWay(context.Background(), 1, Outbound); err != nil {
		t.Fatal(err)
	}
	for _, call := range sc.calls {
		if strings.Contains(call, "exec ucchaos-v1-netem iptables -I INPUT") {
			t.Errorf("outbound-only partition inserted an INPUT rule: %s", call)
		}
	}
	assertContainsInOrder(t, sc.calls, []string{
		"exec ucchaos-v1-netem iptables -I OUTPUT -d 10.89.0.3 -j DROP",
		"exec ucchaos-v1-netem iptables -I OUTPUT -d 10.89.0.4 -j DROP",
	})
}

func TestHealFlushesBothChains(t *testing.T) {
	c, sc := clusterWithScript(t, faultHandler)
	if err := c.Heal(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"exec ucchaos-v2-netem iptables -F INPUT",
		"exec ucchaos-v2-netem iptables -F OUTPUT",
	}
	if got := execCalls(sc.calls); !reflect.DeepEqual(got, want) {
		t.Errorf("exec calls = %v, want %v", got, want)
	}
}

func TestFaultsRejectBrokenIndexesAndSidecars(t *testing.T) {
	c, _ := clusterWithScript(t, func(args []string) (string, error) {
		if strings.HasPrefix(strings.Join(args, " "), "inspect --format {{.State.Running}}") {
			return "false", nil
		}
		return "", nil
	})
	ctx := context.Background()
	if err := c.Isolate(ctx, 0); err == nil {
		t.Error("Isolate(0) accepted")
	}
	if err := c.Heal(ctx, 4); err == nil {
		t.Error("Heal(4) accepted for a three-voter cluster")
	}
	if err := c.Latency(ctx, 1, LatencySpec{Delay: time.Millisecond}); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("Latency with stopped sidecar: err = %v", err)
	}
}

func TestDropCommands(t *testing.T) {
	ips := []string{"10.0.0.2", "10.0.0.3"}
	got := dropCommands(Inbound, ips)
	if len(got) != 2 || got[0][2] != "INPUT" || got[1][4] != "10.0.0.3" {
		t.Errorf("inbound = %v", got)
	}
	if got := dropCommands(Both, ips); len(got) != 4 {
		t.Errorf("both: %d commands, want 4", len(got))
	}
	if got := dropCommands(Outbound, nil); len(got) != 0 {
		t.Errorf("no peers: %v, want none", got)
	}
}

func TestNetemArgs(t *testing.T) {
	tests := []struct {
		name    string
		spec    LatencySpec
		want    string
		wantErr bool
	}{
		{name: "delay with jitter", spec: LatencySpec{Delay: 100 * time.Millisecond, Jitter: 20 * time.Millisecond},
			want: "tc qdisc replace dev eth0 root netem delay 100ms 20ms"},
		{name: "delay only", spec: LatencySpec{Delay: 250 * time.Millisecond},
			want: "tc qdisc replace dev eth0 root netem delay 250ms"},
		{name: "loss only", spec: LatencySpec{LossPercent: 2.5},
			want: "tc qdisc replace dev eth0 root netem loss 2.5%"},
		{name: "delay and loss", spec: LatencySpec{Delay: time.Second, LossPercent: 1},
			want: "tc qdisc replace dev eth0 root netem delay 1000ms loss 1%"},
		{name: "jitter without delay", spec: LatencySpec{Jitter: time.Millisecond}, wantErr: true},
		{name: "nothing to emulate", spec: LatencySpec{}, wantErr: true},
		{name: "loss above 100", spec: LatencySpec{LossPercent: 101}, wantErr: true},
		{name: "negative delay", spec: LatencySpec{Delay: -time.Millisecond}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args, err := netemArgs(tt.spec)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && strings.Join(args, " ") != tt.want {
				t.Errorf("args = %q, want %q", strings.Join(args, " "), tt.want)
			}
		})
	}
}

func TestLatencyUsesSidecarOfVoter(t *testing.T) {
	c, sc := clusterWithScript(t, faultHandler)
	if err := c.Latency(context.Background(), 2, LatencySpec{Delay: 200 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	want := []string{"exec ucchaos-v2-netem tc qdisc replace dev eth0 root netem delay 200ms"}
	if got := execCalls(sc.calls); !reflect.DeepEqual(got, want) {
		t.Errorf("exec calls = %v, want %v", got, want)
	}
}

func TestClearLatencyOnlyWhenInstalled(t *testing.T) {
	none := func(args []string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.HasPrefix(joined, "inspect --format {{.State.Running}}") {
			return "true", nil
		}
		if strings.HasPrefix(joined, "exec ucchaos-v1-netem tc qdisc show") {
			return "qdisc noqueue 0: root refcnt 2\n", nil
		}
		return "", nil
	}
	c, sc := clusterWithScript(t, none)
	if err := c.ClearLatency(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	for _, call := range sc.calls {
		if strings.Contains(call, "qdisc del") {
			t.Errorf("deleted a qdisc that is not installed: %s", call)
		}
	}

	withNetem := func(args []string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.HasPrefix(joined, "exec ucchaos-v1-netem tc qdisc show") {
			return "qdisc netem 8001: root refcnt 2 limit 1000 delay 100ms\n", nil
		}
		return faultHandler(args)
	}
	c, sc = clusterWithScript(t, withNetem)
	if err := c.ClearLatency(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(sc.calls, "\n"), "exec ucchaos-v1-netem tc qdisc del dev eth0 root") {
		t.Errorf("netem not removed; calls = %v", sc.calls)
	}
}

func TestReachableIsFalseWhenConnectFails(t *testing.T) {
	failing := func(args []string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.HasPrefix(joined, "exec ucchaos-v1-netem nc") {
			return "", errStub{}
		}
		return faultHandler(args)
	}
	c, sc := clusterWithScript(t, failing)
	ok, err := c.Reachable(context.Background(), 1, 3, 9443)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("Reachable = true for a failed connect")
	}
	if got := sc.calls[len(sc.calls)-1]; got != "exec ucchaos-v1-netem nc -z -w 3 10.89.0.4 9443" {
		t.Errorf("connect call = %q", got)
	}
}

type errStub struct{}

func (errStub) Error() string { return "exit status 1" }

// execCalls keeps only the sidecar exec calls (drops liveness inspects).
func execCalls(calls []string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, "exec ") {
			out = append(out, c)
		}
	}
	return out
}

// assertContainsInOrder checks that want appears in got as a subsequence.
func assertContainsInOrder(t *testing.T, got, want []string) {
	t.Helper()
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	if i != len(want) {
		t.Errorf("calls = %q\nmissing in order: %q", got, want[i:])
	}
}
