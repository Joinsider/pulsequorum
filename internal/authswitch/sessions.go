// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package authswitch holds the leader tasks that run after a cluster moved
// from AUTH_MODE=local to oidc (ADR 0032, FA-AUTH-08).
package authswitch

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/uptimecluster/internal/cluster"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
)

// SessionInterval is how often the leader looks for local refresh sessions.
// It also bounds how long a session issued by a voter that still runs in
// local mode during a rolling restart stays valid (ADR 0032).
const SessionInterval = 30 * time.Second

// Applier is the part of cluster.Node the tasks use.
type Applier interface {
	IsLeader() bool
	Apply(ctx context.Context, cmd *pb.Command) (cluster.ApplyResult, error)
}

// SessionLister lists the users that still hold a refresh token.
type SessionLister interface {
	ListUnrevokedSessionUsers(ctx context.Context) ([]store.SessionUser, error)
}

// RevokeLocalSessions revokes, while this node leads, every refresh session
// of a local account, at once and then every interval until ctx ends. Local
// accounts stay for emergency access (ADR 0019), but a switch back to local
// must not revive cookies from before the migration (ADR 0032, option 1B).
// The caller starts it only in AUTH_MODE=oidc.
func RevokeLocalSessions(ctx context.Context, log *slog.Logger, n Applier, st SessionLister, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if n.IsLeader() {
			if _, err := RevokeOnce(ctx, log, n, st); err != nil && ctx.Err() == nil {
				log.Warn("could not revoke local refresh sessions", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RevokeOnce revokes the refresh sessions of every user that holds one and
// returns the number of users. It stops at the first failed command; the
// next run retries the rest.
func RevokeOnce(ctx context.Context, log *slog.Logger, n Applier, st SessionLister) (int, error) {
	users, err := st.ListUnrevokedSessionUsers(ctx)
	if err != nil {
		return 0, fmt.Errorf("list session users: %w", err)
	}
	for i, u := range users {
		// A fresh request ID per run: a stable one would let request
		// deduplication swallow the revocation of sessions issued later.
		cmd := cluster.NewRevokeUserSessions(uuid.NewString(), u.TenantID, u.UserID)
		applyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := n.Apply(applyCtx, cmd)
		cancel()
		if err != nil {
			return i, fmt.Errorf("revoke sessions of user %s in tenant %s: %w", u.UserID, u.TenantID, err)
		}
	}
	if len(users) > 0 {
		log.Info("revoked local refresh sessions after the switch to oidc", "users", len(users))
	}
	return len(users), nil
}
