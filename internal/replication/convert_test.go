// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package replication

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/joinsider/uptimecluster/internal/store"
)

func TestHeartbeatRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		in   store.Heartbeat
	}{
		{
			name: "full heartbeat",
			in: store.Heartbeat{
				NodeID: "node-a", Incarnation: "inc-1", Seq: 42, TenantID: "t1", MonitorID: "m9",
				TsMs: 1_700_000_000_123, Status: store.CheckDown, LatencyMs: 350,
				ErrorClass: "http_status", Message: "HTTP 503", Details: json.RawMessage(`{"status":503}`),
			},
		},
		{
			name: "healthy check without error class",
			in: store.Heartbeat{
				NodeID: "probe-1", Incarnation: "inc-2", Seq: 1, TenantID: "t2", MonitorID: "m1",
				TsMs: 1, Status: store.CheckUp, LatencyMs: 0, Details: json.RawMessage(`{"ip":"192.0.2.1"}`),
			},
		},
		{
			name: "degraded with dns class",
			in: store.Heartbeat{
				NodeID: "node-b", Incarnation: "inc-3", Seq: 1 << 40, TenantID: "t1", MonitorID: "m2",
				TsMs: 5, Status: store.CheckDegraded, ErrorClass: "dns", Details: json.RawMessage(`{}`),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := heartbeatFromPB(heartbeatToPB(tt.in))
			if !reflect.DeepEqual(got, tt.in) {
				t.Errorf("round trip changed heartbeat:\n got %+v\nwant %+v", got, tt.in)
			}
		})
	}
}

func TestMissingDetailsBecomeEmptyObject(t *testing.T) {
	in := makeHB("node-a", "inc-1", 7)
	in.Details = nil

	if got := string(heartbeatToPB(in).GetDetailsJson()); got != "{}" {
		t.Errorf("pb details = %q, want {}", got)
	}
	if got := string(heartbeatFromPB(heartbeatToPB(in)).Details); got != "{}" {
		t.Errorf("round trip details = %q, want {}", got)
	}
}

func TestPositionRoundTrip(t *testing.T) {
	in := []store.StreamPosition{
		{NodeID: "a", Incarnation: "x", Seq: 10},
		{NodeID: "b", Incarnation: "y", Seq: 0},
	}
	if got := positionsFromPB(positionsToPB(in)); !reflect.DeepEqual(got, in) {
		t.Errorf("round trip = %+v, want %+v", got, in)
	}
}
