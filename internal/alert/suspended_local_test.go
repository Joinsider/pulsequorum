// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package alert

import (
	"context"
	"testing"

	"github.com/joinsider/pulsequorum/internal/store"
)

// TestLocalDropsDeliveriesOfSuspendedTenants checks that the degraded mode
// drops pending deliveries of suspended tenants without a send (ADR 0029).
func TestLocalDropsDeliveriesOfSuspendedTenants(t *testing.T) {
	l := newDoneLog()
	s := &recordingSender{}
	clk := newClock()
	d := NewLocalDispatcher(LocalDispatcherOptions{
		Sender: s, Now: clk.now, Jitter: func() float64 { return 0.5 },
		Suspended: func(tenantID string) bool { return tenantID == "acme" },
	})
	mine := localDel("mine", "ch1", store.ChannelWebhook, l)
	theirs := localDel("theirs", "ch2", store.ChannelWebhook, l)
	theirs.TenantID = "acme"
	d.Enqueue(mine)
	d.Enqueue(theirs)

	d.deliverDue(context.Background(), clk.now())

	if got := l.outcome("mine"); len(got) != 1 || got[0] != LocalOutcomeSent {
		t.Errorf("default tenant outcome = %v, want sent", got)
	}
	if got := l.outcome("theirs"); len(got) != 1 || got[0] != LocalOutcomeFailed+": tenant is suspended" {
		t.Errorf("suspended tenant outcome = %v, want one failed call", got)
	}
	if n := len(d.pending); n != 0 {
		t.Errorf("pending = %d, want 0", n)
	}
	wantCounts(t, s, 1, 0)
}
