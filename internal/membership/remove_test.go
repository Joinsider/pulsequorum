// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package membership

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/joinsider/pulsequorum/internal/cluster"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
)

// fakeRemoveNode is a Raft node with a fixed configuration. RemoveServer and
// Apply succeed unless removeErr or applyErr is set.
type fakeRemoveNode struct {
	Node       // not used by the tested paths
	leader     bool
	selfID     string
	leaderAddr string
	members    []cluster.Member
	removeErr  error
	applyErr   error
	removed    []string
}

func (f *fakeRemoveNode) IsLeader() bool               { return f.leader }
func (f *fakeRemoveNode) ID() string                   { return f.selfID }
func (f *fakeRemoveNode) Leader() (address, id string) { return f.leaderAddr, "leader" }
func (f *fakeRemoveNode) Members() ([]cluster.Member, uint64, error) {
	return f.members, 1, nil
}
func (f *fakeRemoveNode) RemoveServer(id string, _ uint64, _ time.Duration) (uint64, error) {
	if f.removeErr != nil {
		return 0, f.removeErr
	}
	f.removed = append(f.removed, id)
	return 2, nil
}
func (f *fakeRemoveNode) Apply(context.Context, *pb.Command) (cluster.ApplyResult, error) {
	return cluster.ApplyResult{}, f.applyErr
}

// threeVoters is a configuration of the leader "n1" and two voters.
func threeVoters() []cluster.Member {
	return []cluster.Member{
		{ID: "n1", Address: "10.0.0.1:8443", Voter: true},
		{ID: "n2", Address: "10.0.0.2:8443", Voter: true},
		{ID: "n3", Address: "10.0.0.3:8443", Voter: true},
	}
}

// fakeRemoveClient answers RemoveNode with a canned response or error and
// records the request.
type fakeRemoveClient struct {
	pb.ClusterServiceClient // not used by the tested paths
	resp                    *pb.RemoveNodeResponse
	err                     error
	got                     *pb.RemoveNodeRequest
}

func (f *fakeRemoveClient) RemoveNode(_ context.Context, req *pb.RemoveNodeRequest, _ ...grpc.CallOption) (*pb.RemoveNodeResponse, error) {
	f.got = req
	return f.resp, f.err
}

func TestServiceRemoveNode(t *testing.T) {
	tests := []struct {
		name       string
		req        *pb.RemoveNodeRequest
		leader     bool
		removeErr  error
		wantCode   codes.Code
		wantResp   *pb.RemoveNodeResponse
		wantRemove []string
	}{
		{
			name:     "empty node ID",
			req:      &pb.RemoveNodeRequest{},
			leader:   true,
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "follower never forwards again",
			req:      &pb.RemoveNodeRequest{NodeId: "n3"},
			leader:   false,
			wantCode: codes.FailedPrecondition,
		},
		{
			name:       "removed on the leader",
			req:        &pb.RemoveNodeRequest{NodeId: "n3", RequestedBy: "admin"},
			leader:     true,
			wantResp:   &pb.RemoveNodeResponse{VotersLeft: 2, Warning: "only 2 voter(s) left: the cluster tolerates no further failure"},
			wantRemove: []string{"n3"},
		},
		{
			name:     "unknown node",
			req:      &pb.RemoveNodeRequest{NodeId: "n9"},
			leader:   true,
			wantResp: &pb.RemoveNodeResponse{ErrorCode: "unknown_node", ErrorMessage: ErrUnknownNode.Error()},
		},
		{
			name:     "leader removes itself",
			req:      &pb.RemoveNodeRequest{NodeId: "n1"},
			leader:   true,
			wantResp: &pb.RemoveNodeResponse{ErrorCode: "remove_leader", ErrorMessage: ErrRemoveLeader.Error()},
		},
		{
			name:      "raft failure",
			req:       &pb.RemoveNodeRequest{NodeId: "n3"},
			leader:    true,
			removeErr: errors.New("raft busy"),
			wantCode:  codes.Unavailable,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node := &fakeRemoveNode{leader: tc.leader, selfID: "n1", leaderAddr: "10.0.0.1:8443", members: threeVoters(), removeErr: tc.removeErr}
			s := &Service{Node: node, Manager: &Manager{Raft: node, Log: slog.New(slog.DiscardHandler)}, Log: slog.New(slog.DiscardHandler)}
			resp, err := s.RemoveNode(context.Background(), tc.req)
			if tc.wantCode != codes.OK {
				if status.Code(err) != tc.wantCode {
					t.Fatalf("code = %v (err %v), want %v", status.Code(err), err, tc.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if resp.GetVotersLeft() != tc.wantResp.GetVotersLeft() || resp.GetWarning() != tc.wantResp.GetWarning() ||
				resp.GetErrorCode() != tc.wantResp.GetErrorCode() || resp.GetErrorMessage() != tc.wantResp.GetErrorMessage() {
				t.Errorf("resp = %+v, want %+v", resp, tc.wantResp)
			}
			if len(node.removed) != len(tc.wantRemove) || (len(tc.wantRemove) == 1 && node.removed[0] != tc.wantRemove[0]) {
				t.Errorf("removed = %v, want %v", node.removed, tc.wantRemove)
			}
		})
	}
}

func TestManagerRemoveFollower(t *testing.T) {
	tests := []struct {
		name        string
		leaderAddr  string
		dialErr     error
		resp        *pb.RemoveNodeResponse
		callErr     error
		wantDial    bool
		wantErr     error
		wantAnyErr  bool
		wantRemoval Removal
	}{
		{
			name:        "forwarded and accepted",
			leaderAddr:  "10.0.0.1:8443",
			wantDial:    true,
			resp:        &pb.RemoveNodeResponse{VotersLeft: 2, Warning: "even"},
			wantRemoval: Removal{VotersLeft: 2, Warning: "even"},
		},
		{
			name:       "unknown node",
			leaderAddr: "10.0.0.1:8443",
			wantDial:   true,
			resp:       &pb.RemoveNodeResponse{ErrorCode: "unknown_node", ErrorMessage: "x"},
			wantErr:    ErrUnknownNode,
		},
		{
			name:       "leader cannot be removed",
			leaderAddr: "10.0.0.1:8443",
			wantDial:   true,
			resp:       &pb.RemoveNodeResponse{ErrorCode: "remove_leader", ErrorMessage: "x"},
			wantErr:    ErrRemoveLeader,
		},
		{
			name:       "last voter",
			leaderAddr: "10.0.0.1:8443",
			wantDial:   true,
			resp:       &pb.RemoveNodeResponse{ErrorCode: "last_voter", ErrorMessage: "x"},
			wantErr:    ErrLastVoter,
		},
		{
			name:       "unknown error code is not a sentinel",
			leaderAddr: "10.0.0.1:8443",
			wantDial:   true,
			resp:       &pb.RemoveNodeResponse{ErrorCode: "other", ErrorMessage: "boom"},
			wantAnyErr: true,
		},
		{
			name:       "no known leader",
			leaderAddr: "",
			wantErr:    ErrNotLeader,
		},
		{
			name:       "leader moved",
			leaderAddr: "10.0.0.1:8443",
			wantDial:   true,
			callErr:    status.Error(codes.FailedPrecondition, "not the leader"),
			wantErr:    ErrNotLeader,
		},
		{
			name:       "old leader binary",
			leaderAddr: "10.0.0.1:8443",
			wantDial:   true,
			callErr:    status.Error(codes.Unimplemented, "unknown method"),
			wantErr:    ErrNotLeader,
		},
		{
			name:       "leader unavailable",
			leaderAddr: "10.0.0.1:8443",
			wantDial:   true,
			callErr:    status.Error(codes.Unavailable, "connection refused"),
			wantErr:    ErrLeaderUnavailable,
		},
		{
			name:       "dial failure",
			leaderAddr: "10.0.0.1:8443",
			dialErr:    errors.New("tls handshake"),
			wantDial:   true,
			wantErr:    ErrLeaderUnavailable,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node := &fakeRemoveNode{leader: false, selfID: "n2", leaderAddr: tc.leaderAddr, members: threeVoters()}
			client := &fakeRemoveClient{resp: tc.resp, err: tc.callErr}
			dialed := false
			m := &Manager{Raft: node, Log: slog.New(slog.DiscardHandler), DialLeader: func(address string) (pb.ClusterServiceClient, func(), error) {
				dialed = true
				if address != tc.leaderAddr {
					t.Errorf("dialed %q, want %q", address, tc.leaderAddr)
				}
				if tc.dialErr != nil {
					return nil, nil, tc.dialErr
				}
				return client, func() {}, nil
			}}
			removal, err := m.Remove(context.Background(), "n3", "admin-user")
			if dialed != tc.wantDial {
				t.Errorf("dialed = %v, want %v", dialed, tc.wantDial)
			}
			if tc.wantDial && tc.dialErr == nil && (client.got == nil || client.got.GetNodeId() != "n3" || client.got.GetRequestedBy() != "admin-user") {
				t.Errorf("forwarded request = %+v", client.got)
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if tc.wantAnyErr {
				if err == nil {
					t.Fatal("unknown error code: no error")
				}
				for _, sentinel := range []error{ErrUnknownNode, ErrRemoveLeader, ErrLastVoter, ErrNotLeader, ErrLeaderUnavailable} {
					if errors.Is(err, sentinel) {
						t.Errorf("unknown error code maps to %v", sentinel)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if removal != tc.wantRemoval {
				t.Errorf("removal = %+v, want %+v", removal, tc.wantRemoval)
			}
		})
	}
}
