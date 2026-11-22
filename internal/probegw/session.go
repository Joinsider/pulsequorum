// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package probegw

import (
	"context"
	"maps"
	"slices"
	"sync"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
)

// SessionDetail describes an active probe session connected to this voter.
type SessionDetail struct {
	NodeID           string
	ConnectedSinceMs int64
	LastMessageMs    int64
	Buffer           *pb.ProbeBufferStatus
}

type probeSession struct {
	nodeID           string
	incarnation      string
	connectedSinceMs int64
	lastMessageMs    int64
	buffer           *pb.ProbeBufferStatus
	expectedRate     float64

	subToken  uint64
	subCancel context.CancelFunc
	upToken   uint64
	upCancel  context.CancelFunc
}

type sessionRegistry struct {
	mu       sync.Mutex
	nextTok  uint64
	sessions map[string]*probeSession
}

func newSessionRegistry() *sessionRegistry {
	return &sessionRegistry{sessions: make(map[string]*probeSession)}
}

func (r *sessionRegistry) getOrCreateLocked(nodeID string, nowMs int64) *probeSession {
	s, ok := r.sessions[nodeID]
	if !ok {
		s = &probeSession{nodeID: nodeID, connectedSinceMs: nowMs, lastMessageMs: nowMs}
		r.sessions[nodeID] = s
	}
	return s
}

func (r *sessionRegistry) registerSubscribe(nodeID, incarnation string, nowMs int64, buf *pb.ProbeBufferStatus, cancel context.CancelFunc) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.getOrCreateLocked(nodeID, nowMs)
	if s.subCancel != nil {
		s.subCancel()
	}
	r.nextTok++
	s.subToken = r.nextTok
	s.subCancel = cancel
	if incarnation != "" {
		s.incarnation = incarnation
	}
	s.lastMessageMs = nowMs
	if buf != nil {
		s.buffer = buf
	}
	return s.subToken
}

func (r *sessionRegistry) unregisterSubscribe(nodeID string, token uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[nodeID]
	if !ok || s.subToken != token {
		return
	}
	s.subCancel = nil
	if s.upCancel == nil {
		delete(r.sessions, nodeID)
	}
}

func (r *sessionRegistry) registerUpload(nodeID string, nowMs int64, cancel context.CancelFunc) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.getOrCreateLocked(nodeID, nowMs)
	if s.upCancel != nil {
		s.upCancel()
	}
	r.nextTok++
	s.upToken = r.nextTok
	s.upCancel = cancel
	s.lastMessageMs = nowMs
	return s.upToken
}

func (r *sessionRegistry) unregisterUpload(nodeID string, token uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[nodeID]
	if !ok || s.upToken != token {
		return
	}
	s.upCancel = nil
	if s.subCancel == nil {
		delete(r.sessions, nodeID)
	}
}

func (r *sessionRegistry) touch(nodeID string, nowMs int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[nodeID]; ok {
		s.lastMessageMs = nowMs
	}
}

func (r *sessionRegistry) setExpectedRate(nodeID string, rate float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[nodeID]; ok {
		s.expectedRate = rate
	}
}

func (r *sessionRegistry) expectedRate(nodeID string) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[nodeID]; ok {
		return s.expectedRate
	}
	return 0
}

func (r *sessionRegistry) incarnation(nodeID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[nodeID]; ok {
		return s.incarnation
	}
	return ""
}

func (r *sessionRegistry) cancelNode(nodeID string) {
	r.mu.Lock()
	s, ok := r.sessions[nodeID]
	if ok {
		delete(r.sessions, nodeID)
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	if s.subCancel != nil {
		s.subCancel()
	}
	if s.upCancel != nil {
		s.upCancel()
	}
}

func (r *sessionRegistry) activeIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Sorted(maps.Keys(r.sessions))
}

func (r *sessionRegistry) sessionsProto() []*pb.ProbeSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.sessions) == 0 {
		return nil
	}
	ids := slices.Sorted(maps.Keys(r.sessions))
	out := make([]*pb.ProbeSession, 0, len(ids))
	for _, id := range ids {
		s := r.sessions[id]
		out = append(out, &pb.ProbeSession{
			NodeId:           s.nodeID,
			ConnectedSinceMs: s.connectedSinceMs,
			LastMessageMs:    s.lastMessageMs,
		})
	}
	return out
}

func (r *sessionRegistry) details() map[string]SessionDetail {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]SessionDetail, len(r.sessions))
	for id, s := range r.sessions {
		out[id] = SessionDetail{
			NodeID:           s.nodeID,
			ConnectedSinceMs: s.connectedSinceMs,
			LastMessageMs:    s.lastMessageMs,
			Buffer:           s.buffer,
		}
	}
	return out
}
