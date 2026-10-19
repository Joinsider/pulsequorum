// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package scheduler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/store/sqlite"
)

func TestReconcile(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	state, err := sqlite.OpenState(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = state.Close() }()
	put := func(fn func(tx store.StateTx) error) {
		t.Helper()
		if err := state.Update(ctx, fn); err != nil {
			t.Fatal(err)
		}
	}
	put(func(tx store.StateTx) error {
		for _, id := range []string{"t1", "t2"} {
			if err := tx.PutTenant(ctx, store.Tenant{ID: id, Name: id, Plan: "default"}); err != nil {
				return err
			}
		}
		return nil
	})
	addMonitor := func(tenant string, i int) {
		put(func(tx store.StateTx) error {
			return tx.PutMonitor(ctx, store.Monitor{TenantID: tenant, ID: fmt.Sprintf("%s-m%03d", tenant, i), Name: "m", Type: "fake",
				IntervalSeconds: 3600, TimeoutSeconds: 10, Assignment: store.Assignment{Mode: "all"}, ConsensusPolicy: "majority"}, 0)
		})
	}
	// More than one page for t1 to cover paging.
	for i := range 501 {
		addMonitor("t1", i)
	}
	addMonitor("t2", 0)

	s, _, _ := newScheduler(t, 0)
	changes := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		Reconcile(ctx, state, changes, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)), s)
		close(done)
	}()
	eventually(t, func() bool { return s.Len() == 502 }, "initial load incomplete")

	addMonitor("t2", 1)
	changes <- struct{}{}
	eventually(t, func() bool { return s.Len() == 503 }, "change signal not applied")

	cancel()
	<-done
}
