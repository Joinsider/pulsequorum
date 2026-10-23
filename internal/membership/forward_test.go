// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package membership

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/joinsider/uptimecluster/internal/cluster"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
)

// fakeNode is a leader or follower whose ApplyLocal returns a canned result.
type fakeNode struct {
	Node   // not used by the tested paths
	leader bool
	res    cluster.ApplyResult
	err    error
	called bool
}

func (f *fakeNode) IsLeader() bool               { return f.leader }
func (f *fakeNode) Leader() (address, id string) { return "10.0.0.1:8443", "node-1" }
func (f *fakeNode) ApplyLocal(context.Context, *pb.Command) (cluster.ApplyResult, error) {
	f.called = true
	return f.res, f.err
}

// fakeClient answers Apply with a canned response or error.
type fakeClient struct {
	pb.ClusterServiceClient // not used by the tested paths
	resp                    *pb.ApplyResponse
	err                     error
}

func (f *fakeClient) Apply(context.Context, *pb.ApplyRequest, ...grpc.CallOption) (*pb.ApplyResponse, error) {
	return f.resp, f.err
}

func TestServiceApply(t *testing.T) {
	cmd := &pb.Command{RequestId: "r1"}
	cmdErr := &cluster.CommandError{Code: cluster.CodeNotFound, Message: "monitor not found"}
	tests := []struct {
		name        string
		req         *pb.ApplyRequest
		leader      bool
		res         cluster.ApplyResult
		localErr    error
		wantCode    codes.Code
		wantCalled  bool
		wantErrCode string
		wantDup     bool
		wantIndex   uint64
	}{
		{name: "nil command", req: &pb.ApplyRequest{}, leader: true, wantCode: codes.InvalidArgument},
		{name: "follower never forwards", req: &pb.ApplyRequest{Command: cmd}, leader: false, wantCode: codes.FailedPrecondition},
		{name: "committed", req: &pb.ApplyRequest{Command: cmd}, leader: true, res: cluster.ApplyResult{Index: 7}, wantCode: codes.OK, wantCalled: true, wantIndex: 7},
		{name: "duplicate", req: &pb.ApplyRequest{Command: cmd}, leader: true, res: cluster.ApplyResult{Index: 3, Duplicate: true}, wantCode: codes.OK, wantCalled: true, wantIndex: 3, wantDup: true},
		{name: "command rejected is a result", req: &pb.ApplyRequest{Command: cmd}, leader: true, res: cluster.ApplyResult{Index: 9, Err: cmdErr}, localErr: cmdErr, wantCode: codes.OK, wantCalled: true, wantIndex: 9, wantErrCode: cluster.CodeNotFound},
		{name: "not applied", req: &pb.ApplyRequest{Command: cmd}, leader: true, localErr: cluster.ErrNotApplied, wantCode: codes.FailedPrecondition, wantCalled: true},
		{name: "unknown outcome", req: &pb.ApplyRequest{Command: cmd}, leader: true, localErr: cluster.ErrUnknownOutcome, wantCode: codes.Unavailable, wantCalled: true},
		{name: "fsm halted", req: &pb.ApplyRequest{Command: cmd}, leader: true, localErr: cluster.ErrFSMHalted, wantCode: codes.Internal, wantCalled: true},
		{name: "other error", req: &pb.ApplyRequest{Command: cmd}, leader: true, localErr: errors.New("boom"), wantCode: codes.Unknown, wantCalled: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node := &fakeNode{leader: tc.leader, res: tc.res, err: tc.localErr}
			s := &Service{Node: node}
			resp, err := s.Apply(context.Background(), tc.req)
			if got := status.Code(err); got != tc.wantCode {
				t.Fatalf("code = %v, want %v (err %v)", got, tc.wantCode, err)
			}
			if node.called != tc.wantCalled {
				t.Errorf("ApplyLocal called = %v, want %v", node.called, tc.wantCalled)
			}
			if tc.wantCode != codes.OK {
				return
			}
			if resp.GetIndex() != tc.wantIndex || resp.GetDuplicate() != tc.wantDup {
				t.Errorf("response index=%d duplicate=%v, want %d %v", resp.GetIndex(), resp.GetDuplicate(), tc.wantIndex, tc.wantDup)
			}
			if resp.GetErrorCode() != tc.wantErrCode {
				t.Errorf("error code = %q, want %q", resp.GetErrorCode(), tc.wantErrCode)
			}
		})
	}
}

func TestForwarderForward(t *testing.T) {
	cmd := &pb.Command{RequestId: "r1"}
	cmdErr := &cluster.CommandError{Code: cluster.CodeConflict, Message: "revision changed"}
	dialErr := errors.New("dial refused")
	tests := []struct {
		name      string
		dialErr   error
		resp      *pb.ApplyResponse
		callErr   error
		wantErr   error // sentinel matched with errors.Is; nil for success
		wantCmd   *cluster.CommandError
		wantIndex uint64
		wantDup   bool
	}{
		{name: "committed", resp: &pb.ApplyResponse{Index: 12, ResultJson: []byte(`{"id":"x"}`)}, wantIndex: 12},
		{name: "duplicate", resp: &pb.ApplyResponse{Index: 4, Duplicate: true}, wantIndex: 4, wantDup: true},
		{name: "rejected deterministically", resp: &pb.ApplyResponse{Index: 5, ErrorCode: cmdErr.Code, ErrorMessage: cmdErr.Message}, wantIndex: 5, wantCmd: cmdErr},
		{name: "dial failure is unknown", dialErr: dialErr, wantErr: cluster.ErrUnknownOutcome},
		{name: "not the leader is not applied", callErr: status.Error(codes.FailedPrecondition, "not the leader; leader=n2"), wantErr: cluster.ErrNotApplied},
		{name: "unavailable is unknown", callErr: status.Error(codes.Unavailable, "connection reset"), wantErr: cluster.ErrUnknownOutcome},
		{name: "deadline is unknown", callErr: status.Error(codes.DeadlineExceeded, "deadline"), wantErr: cluster.ErrUnknownOutcome},
		{name: "canceled is unknown", callErr: status.Error(codes.Canceled, "canceled"), wantErr: cluster.ErrUnknownOutcome},
		{name: "internal is unknown", callErr: status.Error(codes.Internal, "fsm halted"), wantErr: cluster.ErrUnknownOutcome},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &Forwarder{Client: func(string) (pb.ClusterServiceClient, error) {
				if tc.dialErr != nil {
					return nil, tc.dialErr
				}
				return &fakeClient{resp: tc.resp, err: tc.callErr}, nil
			}}
			res, err := f.Forward(context.Background(), "10.0.0.1:8443", cmd)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if tc.wantCmd != nil {
				var got *cluster.CommandError
				if !errors.As(err, &got) || got.Code != tc.wantCmd.Code || got.Message != tc.wantCmd.Message {
					t.Fatalf("err = %v, want command error %v", err, tc.wantCmd)
				}
				if res.Err == nil {
					t.Error("result has no command error")
				}
			} else if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if res.Index != tc.wantIndex || res.Duplicate != tc.wantDup {
				t.Errorf("result index=%d duplicate=%v, want %d %v", res.Index, res.Duplicate, tc.wantIndex, tc.wantDup)
			}
		})
	}
}
