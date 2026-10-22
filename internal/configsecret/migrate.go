// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package configsecret

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/uptimecluster/internal/cluster"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
)

// Applier proposes commands to the Raft cluster.
type Applier interface {
	Apply(ctx context.Context, cmd *pb.Command) (cluster.ApplyResult, error)
}

// MigrationOptions configure RunMigration.
type MigrationOptions struct {
	State    store.State
	Cluster  Applier
	Keyring  Keyring
	Schemas  *Schemas
	IsLeader func() bool
	Logger   *slog.Logger
	// Interval between attempts while not leader or after errors (default 30 s).
	Interval time.Duration
}

// RunMigration encrypts secrets of monitors stored before ADR 0015. Only the
// leader proposes; the job ends after one complete pass without errors.
func RunMigration(ctx context.Context, opts MigrationOptions) {
	if opts.Interval <= 0 {
		opts.Interval = 30 * time.Second
	}
	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()
	for {
		if opts.IsLeader() {
			n, err := Migrate(ctx, opts)
			switch {
			case err == nil:
				if n > 0 {
					opts.Logger.Info("encrypted secrets of existing monitors; rotate these credentials, older Raft log entries still contain them until compaction", "monitors", n)
				}
				return
			case ctx.Err() == nil:
				opts.Logger.Warn("encrypting secrets of existing monitors failed, retrying", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Migrate rewrites every monitor whose configuration still contains a
// plaintext secret and returns how many were rewritten.
func Migrate(ctx context.Context, opts MigrationOptions) (int, error) {
	tenants, err := opts.State.ListTenants(ctx)
	if err != nil {
		return 0, err
	}
	count := 0
	var errs []error
	for _, t := range tenants {
		after := ""
		for {
			page, err := opts.State.ListMonitors(ctx, t.ID, store.MonitorQuery{AfterID: after, Limit: 500})
			if err != nil {
				return count, err
			}
			for _, m := range page.Items {
				done, err := migrateMonitor(ctx, opts, m)
				if err != nil {
					// The error names the monitor and pointer, never a value.
					errs = append(errs, fmt.Errorf("monitor %s: %w", m.ID, err))
				} else if done {
					count++
				}
			}
			if page.NextAfterID == "" {
				break
			}
			after = page.NextAfterID
		}
	}
	return count, errors.Join(errs...)
}

func migrateMonitor(ctx context.Context, opts MigrationOptions, m store.Monitor) (bool, error) {
	paths, err := opts.Schemas.Paths(m.Type)
	if err != nil || len(paths) == 0 {
		return false, err
	}
	plain, err := HasPlaintext(paths, m.Config)
	if err != nil || !plain {
		return false, err
	}
	config, sec, err := Split(opts.Keyring, paths, m.ID, m.Config, m.Secrets)
	if err != nil {
		return false, err
	}
	doc := cluster.MonitorDocumentFrom(m)
	doc.Config, doc.Secrets = config, sec
	cmd, err := cluster.NewPutMonitor(uuid.NewString(), m.TenantID, m.ID, doc, m.Revision)
	if err != nil {
		return false, err
	}
	res, err := opts.Cluster.Apply(ctx, cmd)
	if err == nil {
		err = res.Err
	}
	var cmdErr *cluster.CommandError
	if errors.As(err, &cmdErr) && cmdErr.Code == cluster.CodeConflict {
		return false, nil // changed concurrently; the next pass sees the new revision
	}
	return err == nil, err
}
