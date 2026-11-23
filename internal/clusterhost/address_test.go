// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package clusterhost

import (
	"context"
	"testing"
	"time"
)

// TestLeaderAnnouncesNewAddress: a voter that leads after a restart under a
// new address updates its own entry in the Raft configuration. The other
// voters would name it as leader, and its old address is not reachable, so
// asking them never succeeds (FA-CLU-10).
func TestLeaderAnnouncesNewAddress(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("restarts a voter")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	n1, err := startNode(ctx, t, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "n1 leader", 10*time.Second, n1.host.Node.IsLeader)
	old := n1.addr

	moved := restart(ctx, t, n1)
	defer stop(t, moved)
	if moved.addr == old {
		t.Fatal("restart kept the address")
	}
	eventually(t, "restarted node leads", 15*time.Second, moved.host.Node.IsLeader)
	eventually(t, "own address updated", 15*time.Second, func() bool {
		members, _, err := moved.host.Node.Members()
		if err != nil {
			return false
		}
		for _, m := range members {
			if m.ID == moved.id {
				return m.Address == moved.addr && m.Voter
			}
		}
		return false
	})
}
