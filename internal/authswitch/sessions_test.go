// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package authswitch

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/joinsider/uptimecluster/internal/cluster"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
)

// fakeLister returns the users or err and counts the calls.
type fakeLister struct {
	users []store.SessionUser
	err   error
	calls int
}

func (f *fakeLister) ListUnrevokedSessionUsers(_ context.Context) ([]store.SessionUser, error) {
	f.calls++
	return f.users, f.err
}

// failAtApplier fails the command at index failAt (0-based) and succeeds otherwise.
type failAtApplier struct {
	fakeApplier
	failAt int
}

func (f *failAtApplier) Apply(_ context.Context, cmd *pb.Command) (cluster.ApplyResult, error) {
	idx := len(f.cmds)
	f.cmds = append(f.cmds, cmd)
	if idx == f.failAt {
		return cluster.ApplyResult{}, errors.New("no quorum")
	}
	return cluster.ApplyResult{}, nil
}

func TestRevokeOnce(t *testing.T) {
	users := []store.SessionUser{{TenantID: "t1", UserID: "u1"}, {TenantID: "t1", UserID: "u2"}, {TenantID: "t2", UserID: "u3"}}
	tests := []struct {
		name        string
		users       []store.SessionUser
		listErr     error
		wantCount   int
		wantApplied int
		wantErr     bool
	}{
		{name: "no users", users: nil, wantCount: 0, wantApplied: 0},
		{name: "three users", users: users, wantCount: 3, wantApplied: 3},
		{name: "lister error", listErr: errors.New("db closed"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lister := &fakeLister{users: tt.users, err: tt.listErr}
			n := &fakeApplier{leader: true}
			var buf bytes.Buffer
			count, err := RevokeOnce(t.Context(), testLogger(&buf), n, lister)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if count != tt.wantCount {
				t.Errorf("count = %d, want %d", count, tt.wantCount)
			}
			if len(n.cmds) != tt.wantApplied {
				t.Fatalf("applied %d commands, want %d", len(n.cmds), tt.wantApplied)
			}
			seen := map[string]bool{}
			for i, cmd := range n.cmds {
				u := tt.users[i]
				if cmd.GetTenantId() != u.TenantID || cmd.GetRevokeSessions().GetUserId() != u.UserID {
					t.Errorf("command %d revokes user %s in tenant %s, want %s in %s", i,
						cmd.GetRevokeSessions().GetUserId(), cmd.GetTenantId(), u.UserID, u.TenantID)
				}
				if cmd.GetRequestId() == "" || seen[cmd.GetRequestId()] {
					t.Errorf("command %d has request ID %q, empty or repeated", i, cmd.GetRequestId())
				}
				seen[cmd.GetRequestId()] = true
			}
		})
	}
}

func TestRevokeOnceStopsAtFirstFailure(t *testing.T) {
	users := []store.SessionUser{{TenantID: "t1", UserID: "u1"}, {TenantID: "t1", UserID: "u2"}, {TenantID: "t1", UserID: "u3"}}
	lister := &fakeLister{users: users}
	n := &failAtApplier{failAt: 1}
	var buf bytes.Buffer
	count, err := RevokeOnce(t.Context(), testLogger(&buf), n, lister)
	if err == nil {
		t.Fatal("RevokeOnce returned no error")
	}
	if count != 1 {
		t.Errorf("count = %d, want the index 1 of the failed user", count)
	}
	if len(n.cmds) != 2 {
		t.Errorf("applied %d commands, want 2 (the third is not tried)", len(n.cmds))
	}
}

func TestRevokeLocalSessionsOnlyLeaderLists(t *testing.T) {
	lister := &fakeLister{users: []store.SessionUser{{TenantID: "t1", UserID: "u1"}}}
	n := &fakeApplier{leader: false}
	var buf bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	RevokeLocalSessions(ctx, testLogger(&buf), n, lister, time.Millisecond)
	if lister.calls != 0 || len(n.cmds) != 0 {
		t.Errorf("follower listed %d times and applied %d commands, want none", lister.calls, len(n.cmds))
	}
}

func TestRevokeLocalSessionsRevokesWhileLeader(t *testing.T) {
	lister := &fakeLister{users: []store.SessionUser{{TenantID: "t1", UserID: "u1"}, {TenantID: "t1", UserID: "u2"}}}
	n := &fakeApplier{leader: true}
	var buf bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	RevokeLocalSessions(ctx, testLogger(&buf), n, lister, time.Millisecond)
	if lister.calls < 2 {
		t.Errorf("listed %d times, want at least 2 rounds", lister.calls)
	}
	if len(n.cmds) < 2 || len(n.cmds)%2 != 0 {
		t.Errorf("applied %d commands, want a positive multiple of 2 (two users per round)", len(n.cmds))
	}
}

func TestRevokeLocalSessionsReturnsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan struct{})
	go func() {
		RevokeLocalSessions(ctx, testLogger(&bytes.Buffer{}), &fakeApplier{leader: true}, &fakeLister{}, time.Hour)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RevokeLocalSessions did not return after the context was cancelled")
	}
}
