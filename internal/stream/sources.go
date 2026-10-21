// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package stream

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/uptimecluster/internal/api/gen"
	"github.com/joinsider/uptimecluster/internal/checker"
	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/scheduler"
	"github.com/joinsider/uptimecluster/internal/store"
)

// stateQueueSize is the number of committed state changes StateBridge buffers
// before it starts dropping them.
const stateQueueSize = 1024

// stateReportInterval is how often StateBridge reports dropped events.
const stateReportInterval = time.Minute

// HeartbeatSink publishes every finished check of this node as a heartbeat
// event. It implements scheduler.Sink (internal/scheduler) and never blocks.
type HeartbeatSink struct {
	Hub    *Hub
	NodeID string
}

var _ scheduler.Sink = (*HeartbeatSink)(nil)

// Record implements scheduler.Sink.
func (s *HeartbeatSink) Record(_ context.Context, o scheduler.Outcome) {
	ts := o.Started.UTC()
	hb := gen.Heartbeat{
		NodeId:    s.NodeID,
		Ts:        ts,
		Status:    gen.CheckStatus(o.Result.Status.String()),
		LatencyMs: o.Result.LatencyMs,
	}
	if o.Result.ErrorClass != checker.ClassNone {
		ec := gen.ErrorClass(o.Result.ErrorClass)
		hb.ErrorClass = &ec
	}
	if o.Result.Message != "" {
		msg := o.Result.Message
		hb.Message = &msg
	}
	ev := gen.StreamEvent{
		Type:      gen.StreamEventTypeHeartbeat,
		Ts:        ts,
		MonitorId: parseMonitorID(o.Monitor.ID),
		Heartbeat: &hb,
	}
	s.Hub.publish(o.Monitor.TenantID, o.Monitor.ID, ev)
}

// IncidentReader reads incidents of a tenant (store.State satisfies it).
type IncidentReader interface {
	GetIncident(ctx context.Context, tenantID, id string) (store.Incident, error)
	ListIncidents(ctx context.Context, tenantID string, q store.IncidentQuery) (store.IncidentPage, error)
}

// StateBridge turns committed status changes (cluster.StateEvent) into state
// and incident events. Handle is called from the Raft FSM and must not block:
// it only queues; Run does the store reads and publishing.
type StateBridge struct {
	hub       *Hub
	incidents IncidentReader
	log       *slog.Logger
	queue     chan cluster.StateEvent
	dropped   atomic.Uint64
}

// NewStateBridge returns a bridge that publishes to hub. Call Run to start it.
func NewStateBridge(hub *Hub, incidents IncidentReader, log *slog.Logger) *StateBridge {
	return &StateBridge{
		hub:       hub,
		incidents: incidents,
		log:       log,
		queue:     make(chan cluster.StateEvent, stateQueueSize),
	}
}

// Handle queues ev for Run. It never blocks: when the queue is full, ev is
// dropped and counted. It does no logging or I/O, so it is safe to call from
// the FSM.
func (b *StateBridge) Handle(ev cluster.StateEvent) {
	select {
	case b.queue <- ev:
	default:
		b.dropped.Add(1)
	}
}

// Run publishes queued state changes until ctx is done.
func (b *StateBridge) Run(ctx context.Context) {
	report := time.NewTicker(stateReportInterval)
	defer report.Stop()
	var reported uint64
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-b.queue:
			b.publish(ctx, ev)
		case <-report.C:
			if n := b.dropped.Load(); n != reported {
				b.log.Warn("state events dropped, clients may miss updates", "count", n-reported)
				reported = n
			}
		}
	}
}

// publish emits the state event of ev and, if present, its incident events.
// Store errors are logged and the incident event is skipped.
func (b *StateBridge) publish(ctx context.Context, ev cluster.StateEvent) {
	status := gen.MonitorStatus(ev.Status)
	b.hub.publish(ev.TenantID, ev.MonitorID, gen.StreamEvent{
		Type:      gen.StreamEventTypeState,
		Ts:        time.UnixMilli(ev.SinceMs).UTC(),
		MonitorId: parseMonitorID(ev.MonitorID),
		Status:    &status,
	})

	if ev.IncidentOpened != "" {
		inc, err := b.incidents.GetIncident(ctx, ev.TenantID, ev.IncidentOpened)
		if err != nil {
			b.log.Warn("load opened incident for stream failed", "tenant", ev.TenantID, "incident", ev.IncidentOpened, "err", err)
		} else {
			b.publishIncident(ev, inc)
		}
	}

	if ev.IncidentResolved {
		page, err := b.incidents.ListIncidents(ctx, ev.TenantID, store.IncidentQuery{MonitorID: ev.MonitorID, Limit: 1})
		switch {
		case err != nil:
			b.log.Warn("load resolved incident for stream failed", "tenant", ev.TenantID, "monitor", ev.MonitorID, "err", err)
		case len(page.Items) == 0 || page.Items[0].ResolvedMs == 0:
			b.log.Warn("no resolved incident found for stream", "tenant", ev.TenantID, "monitor", ev.MonitorID)
		default:
			b.publishIncident(ev, page.Items[0])
		}
	}
}

// publishIncident emits an incident event. Its timestamp is the resolution
// time if the incident is resolved, else its start.
func (b *StateBridge) publishIncident(ev cluster.StateEvent, inc store.Incident) {
	out := toStreamIncident(inc)
	ts := out.StartedAt
	if out.ResolvedAt != nil {
		ts = *out.ResolvedAt
	}
	b.hub.publish(ev.TenantID, ev.MonitorID, gen.StreamEvent{
		Type:      gen.StreamEventTypeIncident,
		Ts:        ts,
		MonitorId: parseMonitorID(ev.MonitorID),
		Incident:  &out,
	})
}

// publish encodes ev and hands it to the hub for the given tenant and monitor.
// Encoding a gen.StreamEvent does not fail in practice; if it does, the event
// is dropped.
func (h *Hub) publish(tenantID, monitorID string, ev gen.StreamEvent) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	h.Publish(Event{TenantID: tenantID, MonitorID: monitorID, Data: data})
}

// parseMonitorID returns the UUID of a monitor ID, or nil if it is not one.
func parseMonitorID(id string) *uuid.UUID {
	u, err := uuid.Parse(id)
	if err != nil {
		return nil
	}
	return &u
}

// toStreamIncident mirrors api.toAPIIncident; it is duplicated because
// internal/api imports this package.
func toStreamIncident(inc store.Incident) gen.Incident {
	id, _ := uuid.Parse(inc.ID)
	monitorID, _ := uuid.Parse(inc.MonitorID)
	nodes := inc.AffectedNodes
	if nodes == nil {
		nodes = []string{}
	}
	out := gen.Incident{
		Id:            id,
		MonitorId:     monitorID,
		MonitorName:   inc.MonitorName,
		StartedAt:     time.UnixMilli(inc.StartedMs).UTC(),
		Cause:         inc.Cause,
		AffectedNodes: &nodes,
		Confirmed:     &inc.Confirmed,
	}
	if inc.ResolvedMs != 0 {
		at := time.UnixMilli(inc.ResolvedMs).UTC()
		out.ResolvedAt = &at
	}
	if inc.ErrorClass != "" {
		ec := gen.ErrorClass(inc.ErrorClass)
		out.ErrorClass = &ec
	}
	return out
}
