// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package membership

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/joinsider/uptimecluster/internal/cluster"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
)

// Apply runs a command forwarded by a follower (FA-CLU-05). Only the leader
// applies; a follower answers FAILED_PRECONDITION and never forwards again,
// so forwarding cannot loop.
func (s *Service) Apply(ctx context.Context, req *pb.ApplyRequest) (*pb.ApplyResponse, error) {
	cmd := req.GetCommand()
	if cmd == nil {
		return nil, status.Error(codes.InvalidArgument, "command is required")
	}
	if !s.Node.IsLeader() {
		_, leaderID := s.Node.Leader()
		return nil, status.Errorf(codes.FailedPrecondition, "not the leader; leader=%s", leaderID)
	}

	res, err := s.Node.ApplyLocal(ctx, cmd)
	resp := &pb.ApplyResponse{Index: res.Index, ResultJson: res.Result, Duplicate: res.Duplicate}
	var cmdErr *cluster.CommandError
	switch {
	case err == nil:
		return resp, nil
	case errors.As(err, &cmdErr):
		// A deterministic rejection is a result: the follower maps it back.
		resp.ErrorCode = cmdErr.Code
		resp.ErrorMessage = cmdErr.Message
		return resp, nil
	case errors.Is(err, cluster.ErrNotApplied):
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, cluster.ErrUnknownOutcome):
		return nil, status.Error(codes.Unavailable, err.Error())
	case errors.Is(err, cluster.ErrFSMHalted):
		return nil, status.Error(codes.Internal, err.Error())
	default:
		return nil, status.Error(codes.Unknown, err.Error())
	}
}

// Forwarder sends commands of a follower to the leader over the cluster port
// and implements cluster.Forwarder.
type Forwarder struct {
	// Client returns a client for the leader at address (mTLS as voter). The
	// client is reused across calls; the caller owns its connection.
	Client func(address string) (pb.ClusterServiceClient, error)
}

var _ cluster.Forwarder = (*Forwarder)(nil)

// Forward calls Apply on the leader. The error contract of cluster.Forwarder:
// FAILED_PRECONDITION (the callee is not the leader) is ErrNotApplied; any
// other transport or server failure is ErrUnknownOutcome, because the leader
// may have appended the command before the failure.
func (f *Forwarder) Forward(ctx context.Context, leaderAddress string, cmd *pb.Command) (cluster.ApplyResult, error) {
	client, err := f.Client(leaderAddress)
	if err != nil {
		return cluster.ApplyResult{}, fmt.Errorf("%w: connect to leader %s: %w", cluster.ErrUnknownOutcome, leaderAddress, err)
	}
	resp, err := client.Apply(ctx, &pb.ApplyRequest{Command: cmd})
	if err != nil {
		if status.Code(err) == codes.FailedPrecondition {
			return cluster.ApplyResult{}, fmt.Errorf("%w: %s is not the leader: %s", cluster.ErrNotApplied, leaderAddress, status.Convert(err).Message())
		}
		return cluster.ApplyResult{}, fmt.Errorf("%w: forward to %s: %w", cluster.ErrUnknownOutcome, leaderAddress, err)
	}

	res := cluster.ApplyResult{Index: resp.GetIndex(), Result: resp.GetResultJson(), Duplicate: resp.GetDuplicate()}
	if code := resp.GetErrorCode(); code != "" {
		res.Err = &cluster.CommandError{Code: code, Message: resp.GetErrorMessage()}
		return res, res.Err
	}
	return res, nil
}
