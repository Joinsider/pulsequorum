// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package membership changes the voter set of the cluster (ADR 0005 point 7,
// ADR 0012 A3–A5, FA-CLU-03/04/08/10): joining nodes enter as nonvoters and
// are promoted once they caught up, changes are serialized, nodes are only
// removed by a platform admin and are revoked with it.
package membership

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/joinsider/pulsequorum/internal/cluster"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
)

// Timings of the promotion (FA-CLU-03).
const (
	// MaxLag is how far a nonvoter may be behind the leader's log to be promoted.
	MaxLag = 100
	// PromoteTimeout is how long the leader tries to promote a nonvoter.
	PromoteTimeout = 10 * time.Minute
	promotePoll    = 2 * time.Second
	changeTimeout  = 10 * time.Second
	// removeForwardTimeout bounds a removal forwarded to the leader, which
	// waits for the configuration change.
	removeForwardTimeout = 15 * time.Second
)

// Errors of membership changes.
var (
	ErrNotLeader     = errors.New("membership changes run on the leader")
	ErrUnknownNode   = errors.New("node is not a member of the cluster")
	ErrRemoveLeader  = errors.New("the leader cannot remove itself; transfer leadership first")
	ErrLastVoter     = errors.New("the last voter cannot be removed")
	errStatusPending = errors.New("node has not caught up yet")
	// ErrLeaderUnavailable is a transport failure while a follower forwards a
	// request to the leader; the caller may retry.
	ErrLeaderUnavailable = errors.New("no leader available, retry")
)

// AddressInUseError rejects a join of a new node under an address another
// node already has (FA-CLU-08, ADR 0012 A5).
type AddressInUseError struct{ NodeID string }

func (e *AddressInUseError) Error() string {
	return "address is already used by node " + e.NodeID + "; remove or replace that node first"
}

// Raft is the part of cluster.Node the manager uses.
type Raft interface {
	IsLeader() bool
	ID() string
	Status() cluster.Status
	Members() ([]cluster.Member, uint64, error)
	AddNonvoter(id, address string, prevIndex uint64, timeout time.Duration) (uint64, error)
	AddVoter(id, address string, prevIndex uint64, timeout time.Duration) (uint64, error)
	RemoveServer(id string, prevIndex uint64, timeout time.Duration) (uint64, error)
	Apply(ctx context.Context, cmd *pb.Command) (cluster.ApplyResult, error)
	Leader() (address, id string)
}

// StatusFunc asks a node for its status over the cluster port (GetStatus).
type StatusFunc func(ctx context.Context, address string) (*pb.GetStatusResponse, error)

// Manager serializes membership changes on the leader. The mutex covers
// multi-step changes (nonvoter → voter, remove → add); after a leader change
// the new leader continues them idempotently (ADR 0012 A4).
type Manager struct {
	Raft   Raft
	Status StatusFunc
	// DialLeader returns a client for the leader at address (mTLS as voter).
	// The caller closes the connection with the returned func.
	DialLeader func(address string) (pb.ClusterServiceClient, func(), error)
	Log        *slog.Logger

	mu        sync.Mutex
	promoting map[string]bool
}

// CheckAddress reports whether nodeID may use address: it is free or
// already belongs to nodeID. It runs before a join token is spent.
func (m *Manager) CheckAddress(nodeID, address string) error {
	members, _, err := m.Raft.Members()
	if err != nil {
		return err
	}
	for _, s := range members {
		if s.Address == address && s.ID != nodeID {
			return &AddressInUseError{NodeID: s.ID}
		}
	}
	return nil
}

// AddVoter enrolls a voter: as nonvoter first (or with a new address if it
// is a member already, FA-CLU-10), then promoted in the background once it
// caught up.
func (m *Manager) AddVoter(_ context.Context, nodeID, address string) error {
	if !m.Raft.IsLeader() {
		return ErrNotLeader
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	members, prev, err := m.Raft.Members()
	if err != nil {
		return err
	}
	var existing *cluster.Member
	for i, s := range members {
		if s.Address == address && s.ID != nodeID {
			return &AddressInUseError{NodeID: s.ID}
		}
		if s.ID == nodeID {
			existing = &members[i]
		}
	}
	switch {
	case existing != nil && existing.Address == address:
		if !existing.Voter {
			m.startPromotion(nodeID, address, prev)
		}
		return nil // repeated join: nothing to change
	case existing != nil && existing.Voter:
		// Address change of a voter; AddVoter updates it in place without
		// dropping the vote (FA-CLU-10).
		if _, err := m.Raft.AddVoter(nodeID, address, prev, changeTimeout); err != nil {
			return fmt.Errorf("update address of %s: %w", nodeID, err)
		}
		m.Log.Info("voter address changed", "node_id", nodeID, "from", existing.Address, "to", address)
		return nil
	}
	index, err := m.Raft.AddNonvoter(nodeID, address, prev, changeTimeout)
	if err != nil {
		return fmt.Errorf("add nonvoter %s: %w", nodeID, err)
	}
	m.Log.Info("node added as nonvoter", "node_id", nodeID, "address", address, "index", index)
	m.startPromotion(nodeID, address, index)
	return nil
}

func (m *Manager) startPromotion(nodeID, address string, joinIndex uint64) {
	if m.promoting == nil {
		m.promoting = map[string]bool{}
	}
	if m.promoting[nodeID] {
		return
	}
	m.promoting[nodeID] = true
	go m.promote(nodeID, address, joinIndex)
}

// promote waits until the nonvoter applied at least joinIndex and is less
// than MaxLag entries behind, then makes it a voter (ADR 0012 A3). An empty
// joiner is therefore never promoted before it caught up.
func (m *Manager) promote(nodeID, address string, joinIndex uint64) {
	defer func() {
		m.mu.Lock()
		delete(m.promoting, nodeID)
		m.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), PromoteTimeout)
	defer cancel()
	ticker := time.NewTicker(promotePoll)
	defer ticker.Stop()
	for {
		if !m.Raft.IsLeader() {
			return // the next leader resumes (Resume)
		}
		err := m.tryPromote(ctx, nodeID, address, joinIndex)
		switch {
		case err == nil:
			return
		case errors.Is(err, ErrUnknownNode):
			return // removed meanwhile
		case !errors.Is(err, errStatusPending):
			m.Log.Debug("promotion attempt failed", "node_id", nodeID, "err", err)
		}
		select {
		case <-ctx.Done():
			// A system alert follows with the self-monitoring (AP-3.6).
			m.Log.Error("node did not catch up and stays a nonvoter", "node_id", nodeID, "address", address, "timeout", PromoteTimeout)
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) tryPromote(ctx context.Context, nodeID, address string, joinIndex uint64) error {
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := m.Status(callCtx, address)
	if err != nil {
		return err
	}
	if st.GetNodeId() != nodeID {
		return fmt.Errorf("%w: %s answers as %s", errStatusPending, address, st.GetNodeId())
	}
	last := m.Raft.Status().LastLogIndex
	if st.GetAppliedIndex() < joinIndex || last >= st.GetAppliedIndex()+MaxLag {
		return errStatusPending
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	members, prev, err := m.Raft.Members()
	if err != nil {
		return err
	}
	for _, s := range members {
		if s.ID != nodeID {
			continue
		}
		if s.Voter {
			return nil
		}
		if _, err := m.Raft.AddVoter(nodeID, s.Address, prev, changeTimeout); err != nil {
			return err
		}
		m.Log.Info("nonvoter promoted to voter", "node_id", nodeID, "applied_index", st.GetAppliedIndex(), "leader_index", last)
		return nil
	}
	return ErrUnknownNode
}

// Resume continues the promotion of all nonvoters on a new leader.
func (m *Manager) Resume() {
	if !m.Raft.IsLeader() {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	members, index, err := m.Raft.Members()
	if err != nil {
		return
	}
	for _, s := range members {
		if !s.Voter {
			// The configuration index is at or after the nonvoter's own entry.
			m.startPromotion(s.ID, s.Address, index)
		}
	}
}

// Run calls Resume whenever this node becomes leader.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	wasLeader := false
	for {
		leader := m.Raft.IsLeader()
		if leader && !wasLeader {
			m.Resume()
		}
		wasLeader = leader
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Removal describes the cluster after a removal, for the quorum warning
// (FA-CLU-08, FA-CLU-11).
type Removal struct {
	VotersLeft int
	// Warning is set when the remaining voters tolerate no failure or are an
	// even number.
	Warning string
}

// Remove takes a node out of the cluster and revokes its certificates.
// Nodes are never removed automatically (FA-CLU-08); the caller is a
// platform admin. Any voter accepts the request: a follower forwards it to
// the leader, which does the work (RemoveNode). requestedBy is the admin's
// user ID, for the log only.
func (m *Manager) Remove(ctx context.Context, nodeID, requestedBy string) (Removal, error) {
	if m.Raft.IsLeader() {
		return m.removeLocal(ctx, nodeID)
	}
	return m.forwardRemove(ctx, nodeID, requestedBy)
}

// forwardRemove sends the removal to the leader. Without a known leader, a
// leader that moved meanwhile, or an older leader binary without RemoveNode,
// it returns ErrNotLeader so that the caller retries.
func (m *Manager) forwardRemove(ctx context.Context, nodeID, requestedBy string) (Removal, error) {
	addr, _ := m.Raft.Leader()
	if addr == "" {
		return Removal{}, ErrNotLeader
	}
	if m.DialLeader == nil {
		return Removal{}, errors.New("forward removal: no leader dialer configured")
	}
	client, done, err := m.DialLeader(addr)
	if err != nil {
		return Removal{}, fmt.Errorf("%w: connect to leader %s: %w", ErrLeaderUnavailable, addr, err)
	}
	defer done()
	callCtx, cancel := context.WithTimeout(ctx, removeForwardTimeout)
	defer cancel()
	resp, err := client.RemoveNode(callCtx, &pb.RemoveNodeRequest{NodeId: nodeID, RequestedBy: requestedBy})
	if err != nil {
		switch status.Code(err) {
		case codes.FailedPrecondition, codes.Unimplemented:
			return Removal{}, ErrNotLeader
		}
		return Removal{}, fmt.Errorf("%w: forward removal to %s: %w", ErrLeaderUnavailable, addr, err)
	}
	if code := resp.GetErrorCode(); code != "" {
		sentinel := removeErrorByCode(code)
		if sentinel == nil {
			return Removal{}, fmt.Errorf("remove %s on leader %s: %s", nodeID, addr, resp.GetErrorMessage())
		}
		return Removal{}, fmt.Errorf("remove %s on leader %s: %w", nodeID, addr, sentinel)
	}
	return Removal{VotersLeft: int(resp.GetVotersLeft()), Warning: resp.GetWarning()}, nil
}

// removeErrorByCode maps the deterministic rejection codes of RemoveNodeResponse
// to their sentinel errors; nil for an unknown code.
func removeErrorByCode(code string) error {
	switch code {
	case "unknown_node":
		return ErrUnknownNode
	case "remove_leader":
		return ErrRemoveLeader
	case "last_voter":
		return ErrLastVoter
	}
	return nil
}

// removeLocal removes the node on this leader. It is called by Remove on the
// leader and by RemoveNode, which never forwards again.
func (m *Manager) removeLocal(ctx context.Context, nodeID string) (Removal, error) {
	if !m.Raft.IsLeader() {
		return Removal{}, ErrNotLeader
	}
	if nodeID == m.Raft.ID() {
		return Removal{}, ErrRemoveLeader
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	members, prev, err := m.Raft.Members()
	if err != nil {
		return Removal{}, err
	}
	var target *cluster.Member
	voters := 0
	for i, s := range members {
		if s.Voter {
			voters++
		}
		if s.ID == nodeID {
			target = &members[i]
		}
	}
	if target == nil {
		return Removal{}, ErrUnknownNode
	}
	left := voters
	if target.Voter {
		left--
	}
	if left < 1 {
		return Removal{}, ErrLastVoter
	}
	if _, err := m.Raft.RemoveServer(nodeID, prev, changeTimeout); err != nil {
		return Removal{}, fmt.Errorf("remove %s: %w", nodeID, err)
	}
	// Revoke after the removal: a removed node must not reconnect with its
	// old certificate (FA-PKI-04).
	res, err := m.Raft.Apply(ctx, cluster.NewRevokeNode(uuid.NewString(), nodeID))
	if err == nil {
		err = res.Err
	}
	if err != nil {
		return Removal{}, fmt.Errorf("node %s removed, but revoking its certificates failed: %w", nodeID, err)
	}
	m.Log.Warn("node removed from the cluster", "node_id", nodeID, "address", target.Address, "voters_left", left)
	out := Removal{VotersLeft: left}
	switch {
	case left < 3:
		out.Warning = fmt.Sprintf("only %d voter(s) left: the cluster tolerates no further failure", left)
	case left%2 == 0:
		out.Warning = fmt.Sprintf("%d voters: an even number tolerates no more failures than %d", left, left-1)
	}
	return out, nil
}
