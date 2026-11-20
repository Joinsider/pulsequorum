// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package replication

import (
	"bytes"
	"encoding/json"
	"math"

	"github.com/joinsider/pulsequorum/internal/cluster"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
)

// emptyDetails is stored for heartbeats without details, so that every stored
// heartbeat carries valid JSON.
func emptyDetails() json.RawMessage { return json.RawMessage("{}") }

// detailsOrEmpty returns a copy of the details, or "{}" if there are none.
func detailsOrEmpty(d []byte) json.RawMessage {
	if len(d) == 0 {
		return emptyDetails()
	}
	return json.RawMessage(bytes.Clone(d))
}

func heartbeatToPB(h store.Heartbeat) *pb.Heartbeat {
	return &pb.Heartbeat{
		NodeId:      h.NodeID,
		Incarnation: h.Incarnation,
		Seq:         h.Seq,
		TenantId:    h.TenantID,
		MonitorId:   h.MonitorID,
		TsMs:        h.TsMs,
		Status:      pb.CheckStatus(h.Status), //nolint:gosec // G115: statuses are 1..3
		LatencyMs:   uint32(min(max(h.LatencyMs, 0), math.MaxUint32)),
		ErrorClass:  cluster.ErrorClassToProto(h.ErrorClass),
		Message:     h.Message,
		DetailsJson: detailsOrEmpty(h.Details),
	}
}

func heartbeatFromPB(p *pb.Heartbeat) store.Heartbeat {
	return store.Heartbeat{
		NodeID:      p.GetNodeId(),
		Incarnation: p.GetIncarnation(),
		Seq:         p.GetSeq(),
		TenantID:    p.GetTenantId(),
		MonitorID:   p.GetMonitorId(),
		TsMs:        p.GetTsMs(),
		Status:      store.CheckStatus(p.GetStatus()),
		LatencyMs:   int(p.GetLatencyMs()),
		ErrorClass:  cluster.ErrorClassFromProto(p.GetErrorClass()),
		Message:     p.GetMessage(),
		Details:     detailsOrEmpty(p.GetDetailsJson()),
	}
}

func heartbeatsToPB(hbs []store.Heartbeat) []*pb.Heartbeat {
	out := make([]*pb.Heartbeat, len(hbs))
	for i, h := range hbs {
		out[i] = heartbeatToPB(h)
	}
	return out
}

func heartbeatsFromPB(ps []*pb.Heartbeat) []store.Heartbeat {
	out := make([]store.Heartbeat, len(ps))
	for i, p := range ps {
		out[i] = heartbeatFromPB(p)
	}
	return out
}

func positionsToPB(ps []store.StreamPosition) []*pb.StreamPosition {
	out := make([]*pb.StreamPosition, len(ps))
	for i, p := range ps {
		out[i] = &pb.StreamPosition{NodeId: p.NodeID, Incarnation: p.Incarnation, Seq: p.Seq}
	}
	return out
}

func positionsFromPB(ps []*pb.StreamPosition) []store.StreamPosition {
	out := make([]store.StreamPosition, len(ps))
	for i, p := range ps {
		out[i] = store.StreamPosition{NodeID: p.GetNodeId(), Incarnation: p.GetIncarnation(), Seq: p.GetSeq()}
	}
	return out
}
