// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package membership

import (
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestPermanentJoinError covers ADR 0018: a binary too old for the cluster
// command level is a permanent join error, while a "not the leader" answer is
// retried.
func TestPermanentJoinError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "invalid token", err: status.Error(codes.PermissionDenied, "invalid join token"), want: true},
		{name: "address conflict", err: status.Error(codes.AlreadyExists, "address in use"), want: true},
		{name: "invalid request", err: status.Error(codes.InvalidArgument, "node ID must be a UUID"), want: true},
		{
			name: "binary too old for the command level",
			err: status.Error(codes.FailedPrecondition,
				"this binary supports command version 2, the cluster requires 3: upgrade the binary before joining (ADR 0018)"),
			want: true,
		},
		{name: "not the leader", err: status.Error(codes.FailedPrecondition, "not the leader; leader=10.0.0.1:8443"), want: false},
		{name: "unavailable", err: status.Error(codes.Unavailable, "no leader available, retry"), want: false},
		{name: "transport error", err: errors.New("connection refused"), want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := permanentJoinError(tc.err); got != tc.want {
				t.Errorf("permanentJoinError = %v, want %v", got, tc.want)
			}
		})
	}
}
