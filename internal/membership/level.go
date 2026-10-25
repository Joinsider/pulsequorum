// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package membership

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/joinsider/uptimecluster/internal/cluster"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
)

// levelInterval is how often the leader compares the cluster command level
// with the voters (ADR 0018).
const levelInterval = 2 * time.Second

// targetLevel returns the command level all Raft members support (ADR 0018):
// the minimum of the max command versions of the voters and of the non-voters
// that are catching up, since those become voters next. A member without a
// registry entry, or with version 0, counts as the base level. A cluster
// without members has the base level.
func targetLevel(members []cluster.Member, registry []store.Node) uint32 {
	versions := make(map[string]uint32, len(registry))
	for _, n := range registry {
		versions[n.ID] = n.MaxCommandVersion
	}
	var target uint32
	found := false
	for _, m := range members {
		v := versions[m.ID]
		if v == 0 {
			v = cluster.BaseCommandLevel
		}
		if !found || v < target {
			target = v
			found = true
		}
	}
	if !found {
		return cluster.BaseCommandLevel
	}
	return target
}

// LevelNode is the part of cluster.Node the LevelWatcher uses.
type LevelNode interface {
	IsLeader() bool
	Members() ([]cluster.Member, uint64, error)
	CommandLevel(ctx context.Context) (uint32, error)
	Apply(ctx context.Context, cmd *pb.Command) (cluster.ApplyResult, error)
}

// LevelWatcher raises the cluster command level once all voters support a
// higher one (ADR 0018). Only the leader proposes. It never lowers the level.
type LevelWatcher struct {
	Node  LevelNode
	State store.State
	Log   *slog.Logger
}

// Run checks the level every levelInterval until ctx ends.
func (w *LevelWatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(levelInterval)
	defer ticker.Stop()
	for {
		w.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// tick proposes a higher level if the voters support one.
func (w *LevelWatcher) tick(ctx context.Context) {
	if !w.Node.IsLeader() {
		return
	}
	members, _, err := w.Node.Members()
	if err != nil {
		w.Log.Debug("reading the cluster configuration for the command level failed", "err", err)
		return
	}
	nodes, err := w.State.ListNodes(ctx)
	if err != nil {
		if ctx.Err() == nil {
			w.Log.Warn("reading the node registry for the command level failed", "err", err)
		}
		return
	}
	current, err := w.Node.CommandLevel(ctx)
	if err != nil {
		if ctx.Err() == nil {
			w.Log.Warn("reading the cluster command level failed", "err", err)
		}
		return
	}
	target := targetLevel(members, nodes)
	if target <= current {
		return
	}
	w.propose(ctx, current, target)
}

// propose raises the command level to level. The request ID is derived from
// the level, so a repeated proposal of the same level is deduplicated.
func (w *LevelWatcher) propose(ctx context.Context, from, to uint32) {
	callCtx, cancel := context.WithTimeout(ctx, changeTimeout)
	defer cancel()
	cmd := cluster.NewSetFeatureLevel("feature-level:"+strconv.FormatUint(uint64(to), 10), to)
	if _, err := w.Node.Apply(callCtx, cmd); err != nil {
		if ctx.Err() == nil {
			w.Log.Warn("raising the cluster command level failed, retrying", "from", from, "to", to, "err", err)
		}
		return
	}
	w.Log.Info("cluster command level raised", "from", from, "to", to)
}
