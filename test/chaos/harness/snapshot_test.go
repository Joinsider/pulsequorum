// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package harness

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/test/internal/ucapi"
)

func monitorFixture(id, name string, interval int, status string, updated time.Time) ucapi.Monitor {
	m := ucapi.Monitor{
		MonitorInput: ucapi.MonitorInput{
			Name:                   name,
			Type:                   "http",
			Config:                 map[string]any{"url": "http://target/" + name, "headers": "***"},
			IntervalSeconds:        interval,
			Retries:                1,
			ConsensusPolicy:        "majority",
			Tags:                   []string{"b", "a"},
			NotificationChannelIDs: []string{"ch2", "ch1"},
			Assignment:             &ucapi.Assignment{Mode: "all"},
		},
		ID:        id,
		Status:    status,
		UpdatedAt: updated,
	}
	return m
}

func TestNormalizeMonitorsDropsVolatileFields(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a := NormalizeMonitors([]ucapi.Monitor{monitorFixture("m2", "two", 20, "UP", t0), monitorFixture("m1", "one", 20, "DOWN", t0)})
	b := NormalizeMonitors([]ucapi.Monitor{monitorFixture("m1", "one", 20, "UP", t0.Add(time.Hour)), monitorFixture("m2", "two", 20, "PENDING", t0)})

	if a[0].ID != "m1" || a[1].ID != "m2" {
		t.Errorf("not sorted by ID: %s, %s", a[0].ID, a[1].ID)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("status and updatedAt must not affect the view:\n%+v\n%+v", a, b)
	}
	if !reflect.DeepEqual(a[0].Tags, []string{"a", "b"}) {
		t.Errorf("tags not sorted: %v", a[0].Tags)
	}
}

func TestCompareSnapshotsIdentical(t *testing.T) {
	s := Snapshot{
		Monitors: NormalizeMonitors([]ucapi.Monitor{monitorFixture("m1", "one", 20, "UP", time.Time{})}),
		Channels: NormalizeChannels([]ucapi.NotificationChannel{{ID: "c1", Name: "ntfy", Kind: "shoutrrr", MaskedURL: "ntfy://***", Events: []string{"up", "down"}, IsDefault: true}}),
		Members:  []MemberView{{ID: "n1", Address: "v1:9443", Voter: true}},
	}
	if diffs := CompareSnapshots(s, s); len(diffs) != 0 {
		t.Errorf("identical snapshots differ: %v", diffs)
	}
}

func TestCompareSnapshotsReportsDifferences(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a := Snapshot{Monitors: NormalizeMonitors([]ucapi.Monitor{
		monitorFixture("m1", "one", 20, "UP", t0),
		monitorFixture("m2", "two", 20, "UP", t0),
	})}
	b := Snapshot{Monitors: NormalizeMonitors([]ucapi.Monitor{
		monitorFixture("m1", "one", 60, "UP", t0),
		monitorFixture("m3", "three", 20, "UP", t0),
	})}
	diffs := CompareSnapshots(a, b)
	want := []string{
		"monitor one (m1) differs",
		"monitor three (m3) is only on the second voter",
		"monitor two (m2) is only on the first voter",
	}
	if !reflect.DeepEqual(diffs, want) {
		t.Errorf("diffs = %q\nwant %q", diffs, want)
	}
}

func TestCompareSnapshotsMembersAndUsers(t *testing.T) {
	a := Snapshot{Members: []MemberView{{ID: "n1", Address: "v1:9443", Voter: true}}}
	b := Snapshot{Members: []MemberView{{ID: "n1", Address: "v1:9443", Voter: false}}}
	if diffs := CompareSnapshots(a, b); len(diffs) != 1 {
		t.Errorf("voter flag change not reported: %v", diffs)
	}
	u := NormalizeUsers([]ucapi.User{{ID: "u1", Username: "admin", Role: "platform-admin"}})
	if len(u) != 1 || u[0].Username != "admin" {
		t.Errorf("users = %+v", u)
	}
}

func TestSnapshotJSONIsStable(t *testing.T) {
	s := Snapshot{Channels: NormalizeChannels([]ucapi.NotificationChannel{{ID: "c", Name: "n", Events: []string{"up"}}})}
	first, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := json.Marshal(s)
	if string(first) != string(second) {
		t.Error("snapshot JSON not stable")
	}
}
