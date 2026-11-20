// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package degraded

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"
)

// fixture is a fake cluster view driven by the test.
type fixture struct {
	now        time.Time
	leader     string
	caught     bool
	voters     []string
	voterErr   error
	peers      map[string]PeerView
	selfFailed bool
	votersRead int
}

func newFixture() *fixture {
	return &fixture{
		now:    time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC),
		caught: true,
		voters: []string{"node-a", "node-b", "node-c"},
		peers:  map[string]PeerView{},
	}
}

func (f *fixture) detector(nodeID string, after time.Duration) *Detector {
	return New(Options{
		NodeID:   nodeID,
		After:    after,
		LeaderID: func() string { return f.leader },
		CaughtUp: func() bool { return f.caught },
		Voters: func(context.Context) ([]string, error) {
			f.votersRead++
			return f.voters, f.voterErr
		},
		Peers:           func() map[string]PeerView { return f.peers },
		SelfCheckFailed: func() bool { return f.selfFailed },
		Now:             func() time.Time { return f.now },
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// fresh returns a PeerView that answered just now.
func (f *fixture) fresh(leaderID string, selfFailed bool) PeerView {
	return PeerView{AnsweredAt: f.now, LeaderID: leaderID, SelfCheckFailed: selfFailed}
}

// step advances the fake clock, sets the leader view and evaluates the detector.
type step struct {
	advance      time.Duration
	leader       string
	caught       bool
	wantDegraded bool
}

func TestDegradedTransitions(t *testing.T) {
	tests := []struct {
		name  string
		steps []step
	}{
		{"not degraded before After", []step{
			{0, "", false, false},
			{14 * time.Second, "", false, false},
		}},
		{"degraded at After", []step{
			{0, "", false, false},
			{15 * time.Second, "", false, true},
		}},
		{"leader known but not caught up stays degraded", []step{
			{0, "", false, false},
			{15 * time.Second, "", false, true},
			{time.Second, "node-b", false, true},
			{time.Minute, "node-b", false, true},
		}},
		{"caught up leaves degraded", []step{
			{0, "", false, false},
			{15 * time.Second, "", false, true},
			{time.Second, "node-b", true, false},
		}},
		{"leaderless timer restarts after a known leader", []step{
			{0, "", false, false},
			{10 * time.Second, "node-b", true, false},
			{0, "", false, false},
			{14 * time.Second, "", false, false},
			{time.Second, "", false, true},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			d := f.detector("node-a", 0)
			for i, s := range tc.steps {
				f.now = f.now.Add(s.advance)
				f.leader, f.caught = s.leader, s.caught
				d.Update(context.Background())
				if got := d.State().Degraded; got != s.wantDegraded {
					t.Fatalf("step %d: Degraded = %v, want %v", i, got, s.wantDegraded)
				}
			}
		})
	}
}

func TestSinceIsStartOfLeaderlessPeriod(t *testing.T) {
	f := newFixture()
	d := f.detector("node-a", 0)
	start := f.now
	d.Update(context.Background())
	f.now = f.now.Add(20 * time.Second)
	d.Update(context.Background())
	if s := d.State(); !s.Degraded || !s.Since.Equal(start) {
		t.Fatalf("State = %+v, want degraded since %v", s, start)
	}
}

func TestDefaultsAndDisabled(t *testing.T) {
	t.Run("zero After means default 15s", func(t *testing.T) {
		f := newFixture()
		d := f.detector("node-a", 0)
		if d.opts.After != DefaultAfter {
			t.Fatalf("After = %v, want %v", d.opts.After, DefaultAfter)
		}
	})
	t.Run("negative After never degrades", func(t *testing.T) {
		f := newFixture()
		d := f.detector("node-a", -1)
		for range 60 {
			f.now = f.now.Add(time.Second)
			d.Update(context.Background())
		}
		if d.State().Degraded || d.IsSender() {
			t.Fatalf("disabled detector degraded: %+v", d.State())
		}
		if f.votersRead != 0 {
			t.Errorf("disabled detector read the registry %d times", f.votersRead)
		}
	})
}

func TestPartitionAndElection(t *testing.T) {
	tests := []struct {
		name          string
		nodeID        string
		voters        []string
		peers         func(f *fixture) map[string]PeerView
		selfFailed    bool
		wantPartition []string
		wantSender    string
	}{
		{
			name:          "alone with no peer answers",
			nodeID:        "node-b",
			wantPartition: []string{"node-b"},
			wantSender:    "node-b",
		},
		{
			name:   "stale answer is not reachable",
			nodeID: "node-b",
			peers: func(f *fixture) map[string]PeerView {
				return map[string]PeerView{"node-a": {AnsweredAt: f.now.Add(-16 * time.Second)}}
			},
			wantPartition: []string{"node-b"},
			wantSender:    "node-b",
		},
		{
			name:   "answer exactly at ReachableWithin is reachable",
			nodeID: "node-b",
			peers: func(f *fixture) map[string]PeerView {
				return map[string]PeerView{"node-a": {AnsweredAt: f.now.Add(-15 * time.Second)}}
			},
			wantPartition: []string{"node-a", "node-b"},
			wantSender:    "node-a",
		},
		{
			name:   "peer that reports a leader is excluded",
			nodeID: "node-b",
			peers: func(f *fixture) map[string]PeerView {
				return map[string]PeerView{"node-a": f.fresh("node-c", false)}
			},
			wantPartition: []string{"node-b"},
			wantSender:    "node-b",
		},
		{
			name:   "leaderless reachable peer joins the partition",
			nodeID: "node-b",
			peers: func(f *fixture) map[string]PeerView {
				return map[string]PeerView{"node-c": f.fresh("", false), "node-a": f.fresh("", false)}
			},
			wantPartition: []string{"node-a", "node-b", "node-c"},
			wantSender:    "node-a",
		},
		{
			name:   "peer outside the registry is ignored",
			nodeID: "node-b",
			voters: []string{"node-a", "node-b"},
			peers: func(f *fixture) map[string]PeerView {
				return map[string]PeerView{"node-x": f.fresh("", false), "node-a": f.fresh("", false)}
			},
			wantPartition: []string{"node-a", "node-b"},
			wantSender:    "node-a",
		},
		{
			name:   "failed self-check of a peer is skipped",
			nodeID: "node-b",
			peers: func(f *fixture) map[string]PeerView {
				return map[string]PeerView{"node-a": f.fresh("", true), "node-c": f.fresh("", false)}
			},
			wantPartition: []string{"node-a", "node-b", "node-c"},
			wantSender:    "node-b",
		},
		{
			name:   "failed own self-check is skipped",
			nodeID: "node-b",
			peers: func(f *fixture) map[string]PeerView {
				return map[string]PeerView{"node-a": f.fresh("", false)}
			},
			selfFailed:    true,
			wantPartition: []string{"node-a", "node-b"},
			wantSender:    "node-a",
		},
		{
			name:   "all failed: smallest ID of the partition",
			nodeID: "node-b",
			peers: func(f *fixture) map[string]PeerView {
				return map[string]PeerView{"node-a": f.fresh("", true), "node-c": f.fresh("", true)}
			},
			selfFailed:    true,
			wantPartition: []string{"node-a", "node-b", "node-c"},
			wantSender:    "node-a",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			if tc.voters != nil {
				f.voters = tc.voters
			}
			f.selfFailed = tc.selfFailed
			d := f.detector(tc.nodeID, 0)
			d.Update(context.Background()) // leaderless starts
			f.now = f.now.Add(DefaultAfter)
			if tc.peers != nil {
				f.peers = tc.peers(f)
			}
			d.Update(context.Background())

			s := d.State()
			if !s.Degraded {
				t.Fatalf("not degraded: %+v", s)
			}
			if !reflect.DeepEqual(s.Partition, tc.wantPartition) {
				t.Errorf("Partition = %v, want %v", s.Partition, tc.wantPartition)
			}
			if s.Reachable != len(tc.wantPartition) {
				t.Errorf("Reachable = %d, want %d", s.Reachable, len(tc.wantPartition))
			}
			if s.Sender != tc.wantSender {
				t.Errorf("Sender = %q, want %q", s.Sender, tc.wantSender)
			}
			if s.Total != len(f.voters) {
				t.Errorf("Total = %d, want %d", s.Total, len(f.voters))
			}
			if got, want := d.IsSender(), tc.wantSender == tc.nodeID; got != want {
				t.Errorf("IsSender = %v, want %v", got, want)
			}
		})
	}
}

func TestSenderEmptyWhenNotDegraded(t *testing.T) {
	f := newFixture()
	f.leader = "node-a"
	d := f.detector("node-b", 0)
	d.Update(context.Background())
	s := d.State()
	if s.Degraded || s.Sender != "" || len(s.Partition) != 0 || s.Total != 3 {
		t.Fatalf("State = %+v, want not degraded, no sender, total 3", s)
	}
}

func TestVoterRegistryErrorKeepsPartition(t *testing.T) {
	f := newFixture()
	d := f.detector("node-b", 0)
	d.Update(context.Background())
	f.now = f.now.Add(DefaultAfter)
	f.peers = map[string]PeerView{"node-a": f.fresh("", false)}
	d.Update(context.Background())
	before := d.State()

	f.voterErr = errors.New("registry busy")
	f.now = f.now.Add(time.Second)
	d.Update(context.Background())
	after := d.State()

	if !after.Degraded {
		t.Fatal("registry error must not end degraded mode")
	}
	if after.Total != before.Total || !reflect.DeepEqual(after.Partition, before.Partition) {
		t.Errorf("after registry error: Total %d, Partition %v; want %d, %v",
			after.Total, after.Partition, before.Total, before.Partition)
	}
}

func TestChangesSignalled(t *testing.T) {
	recv := func(d *Detector) bool {
		select {
		case <-d.Changes():
			return true
		default:
			return false
		}
	}

	f := newFixture()
	d := f.detector("node-b", 0)
	d.Update(context.Background())
	if recv(d) {
		t.Fatal("signal without a change")
	}

	// Entering degraded mode: Degraded changes, node-b is the sender alone.
	f.now = f.now.Add(DefaultAfter)
	d.Update(context.Background())
	if !recv(d) {
		t.Fatal("no signal on entering degraded mode")
	}
	d.Update(context.Background())
	if recv(d) {
		t.Fatal("signal without a change of Degraded or IsSender")
	}

	// A healthy peer with a smaller ID joins and takes over: IsSender changes.
	f.peers = map[string]PeerView{"node-a": f.fresh("", false)}
	f.selfFailed = true
	d.Update(context.Background())
	if !recv(d) {
		t.Fatal("no signal when IsSender changed")
	}
	if d.IsSender() {
		t.Fatal("node-b must not be sender after its failed self-check")
	}

	// Leaving degraded mode.
	f.leader, f.caught = "node-a", true
	d.Update(context.Background())
	if !recv(d) {
		t.Fatal("no signal on leaving degraded mode")
	}
}
