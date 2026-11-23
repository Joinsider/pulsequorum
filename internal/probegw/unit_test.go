// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package probegw

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
)

func TestTokenBucket(t *testing.T) {
	// Default clock when now is nil.
	tbDefault := newTokenBucket(10, 5, nil)
	if err := tbDefault.wait(context.Background(), 0); err != nil {
		t.Fatalf("wait(0): %v", err)
	}
	if err := tbDefault.wait(context.Background(), 3); err != nil {
		t.Fatalf("wait(3): %v", err)
	}

	// Controlled clock: refill, setRate, rate <= 0 fallback, timer wait, and ctx cancel.
	now := time.Unix(1000, 0)
	tb := newTokenBucket(100, 2, func() time.Time { return now })
	// Drain initial burst.
	if err := tb.wait(context.Background(), 2); err != nil {
		t.Fatalf("drain burst: %v", err)
	}
	// Advance clock by 10ms -> refills 1 token.
	now = now.Add(10 * time.Millisecond)
	tb.setRate(1000)
	if err := tb.wait(context.Background(), 1); err != nil {
		t.Fatalf("wait after refill: %v", err)
	}

	// When tokens are empty and rate is high, wait sleeps briefly and succeeds once clock advances.
	var ticks atomic.Int32
	tbTimer := newTokenBucket(1000, 1, func() time.Time {
		n := ticks.Add(1)
		return now.Add(time.Duration(n) * 5 * time.Millisecond)
	})
	if err := tbTimer.wait(context.Background(), 1); err != nil {
		t.Fatalf("first token: %v", err)
	}
	if err := tbTimer.wait(context.Background(), 1); err != nil {
		t.Fatalf("second token via timer: %v", err)
	}

	// Context cancellation when rate <= 0 and bucket is empty.
	tbZero := newTokenBucket(0, 1, func() time.Time { return now })
	_ = tbZero.wait(context.Background(), 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := tbZero.wait(ctx, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait canceled = %v, want DeadlineExceeded", err)
	}
}

func TestSessionRegistryLifecycle(t *testing.T) {
	r := newSessionRegistry()
	if got := r.sessionsProto(); got != nil {
		t.Fatalf("empty sessionsProto = %v, want nil", got)
	}
	if got := r.expectedRate("unknown"); got != 0 {
		t.Fatalf("expectedRate(unknown) = %v, want 0", got)
	}
	if got := r.incarnation("unknown"); got != "" {
		t.Fatalf("incarnation(unknown) = %q, want empty", got)
	}
	r.cancelNode("unknown")
	r.unregisterSubscribe("unknown", 1)
	r.unregisterUpload("unknown", 1)

	var subCanceled1, subCanceled2, upCanceled1, upCanceled2 atomic.Bool
	buf := &pb.ProbeBufferStatus{Bytes: 128, Unacked: 4}
	tokSub1 := r.registerSubscribe("p1", "inc-1", 100, buf, func() { subCanceled1.Store(true) })
	// Second subscribe replaces first and cancels subCanceled1.
	tokSub2 := r.registerSubscribe("p1", "inc-2", 200, nil, func() { subCanceled2.Store(true) })
	if !subCanceled1.Load() {
		t.Fatal("expected previous subscribe session to be canceled")
	}
	// Stale token is ignored.
	r.unregisterSubscribe("p1", tokSub1)
	if r.incarnation("p1") != "inc-2" {
		t.Fatalf("incarnation = %q, want inc-2", r.incarnation("p1"))
	}

	tokUp1 := r.registerUpload("p1", 250, func() { upCanceled1.Store(true) })
	tokUp2 := r.registerUpload("p1", 300, func() { upCanceled2.Store(true) })
	if !upCanceled1.Load() {
		t.Fatal("expected previous upload session to be canceled")
	}
	r.unregisterUpload("p1", tokUp1)

	r.touch("p1", 400)
	r.setExpectedRate("p1", 2.5)
	if got := r.expectedRate("p1"); got != 2.5 {
		t.Fatalf("expectedRate = %v, want 2.5", got)
	}
	if ids := r.activeIDs(); len(ids) != 1 || ids[0] != "p1" {
		t.Fatalf("activeIDs = %v, want [p1]", ids)
	}
	if protos := r.sessionsProto(); len(protos) != 1 || protos[0].GetLastMessageMs() != 400 {
		t.Fatalf("sessionsProto = %+v", protos)
	}
	if det := r.details(); det["p1"].Buffer != buf || det["p1"].LastMessageMs != 400 {
		t.Fatalf("details = %+v", det)
	}

	// Unregister subscribe while upload is still active keeps the session.
	r.unregisterSubscribe("p1", tokSub2)
	if len(r.activeIDs()) != 1 {
		t.Fatal("session should remain while upload is active")
	}
	// Unregister upload removes the session once subscribe is also gone.
	r.unregisterUpload("p1", tokUp2)
	if len(r.activeIDs()) != 0 {
		t.Fatal("session should be removed after both streams unregister")
	}

	// cancelNode cancels both active streams and deletes the entry.
	var subCanceled3, upCanceled3 atomic.Bool
	r.registerSubscribe("p2", "inc-3", 500, nil, func() { subCanceled3.Store(true) })
	r.registerUpload("p2", 500, func() { upCanceled3.Store(true) })
	r.cancelNode("p2")
	if !subCanceled3.Load() || !upCanceled3.Load() || len(r.activeIDs()) != 0 {
		t.Fatal("cancelNode did not cancel both streams and remove session")
	}
}
