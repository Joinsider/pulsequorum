// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package icmpcheck

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/joinsider/pulsequorum/internal/checker"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func check(t *testing.T, env *checker.Env, timeout time.Duration, cfg map[string]any) checker.Result {
	t.Helper()
	raw, _ := json.Marshal(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return (&Checker{}).Check(ctx, env, raw)
}

// availableMode returns a working ICMP mode or skips: CI runners and
// containers may forbid both socket types.
func availableMode(t *testing.T) string {
	t.Helper()
	mode, err := DetectMode(context.Background(), ModeAuto)
	if mode == ModeOff {
		t.Skipf("ICMP not available here: %v", err)
	}
	return mode
}

func TestPingLoopback(t *testing.T) {
	env := &checker.Env{Dialer: &checker.Dialer{}, ICMPMode: availableMode(t)}
	res := check(t, env, 5*time.Second, map[string]any{"host": "127.0.0.1", "count": 2, "intervalMs": 100})
	if res.Status != checker.StatusUp || res.Details["packetsReceived"] != 2 || res.Details["resolvedIp"] != "127.0.0.1" {
		t.Fatalf("result = %s/%s %s %v", res.Status, res.ErrorClass, res.Message, res.Details)
	}
}

func TestNoReply(t *testing.T) {
	env := &checker.Env{Dialer: &checker.Dialer{}, ICMPMode: availableMode(t)}
	// 192.0.2.0/24 (TEST-NET-1) is never routed.
	res := check(t, env, 1500*time.Millisecond, map[string]any{"host": "192.0.2.1", "count": 1})
	if res.Status != checker.StatusDown {
		t.Fatalf("result = %s/%s %s", res.Status, res.ErrorClass, res.Message)
	}
}

func TestBlockedBeforeSending(t *testing.T) {
	// No ICMP socket is needed: the policy rejects the target first.
	env := &checker.Env{Dialer: &checker.Dialer{Policy: checker.BlockPrivate{}}, ICMPMode: ModeUnprivileged}
	res := check(t, env, 2*time.Second, map[string]any{"host": "127.0.0.1"})
	if res.ErrorClass != checker.ClassSSRFBlocked {
		t.Fatalf("result = %s/%s %s", res.Status, res.ErrorClass, res.Message)
	}
}

func TestICMPOff(t *testing.T) {
	env := &checker.Env{Dialer: &checker.Dialer{}, ICMPMode: ModeOff}
	res := check(t, env, time.Second, map[string]any{"host": "127.0.0.1"})
	if res.Status != checker.StatusDown || res.ErrorClass != checker.ClassInternal {
		t.Fatalf("result = %s/%s", res.Status, res.ErrorClass)
	}
	if (&Checker{}).RequiredCapability() != Capability {
		t.Error("capability not declared")
	}
}

func TestDetectMode(t *testing.T) {
	if mode, err := DetectMode(context.Background(), ModeOff); mode != ModeOff || err != nil {
		t.Errorf("off = %s, %v", mode, err)
	}
	if _, err := DetectMode(context.Background(), "sometimes"); err == nil {
		t.Error("unknown mode accepted")
	}
}

func TestValidation(t *testing.T) {
	reg := checker.NewRegistry()
	reg.Register(&Checker{})
	if err := reg.Validate(Type, json.RawMessage(`{"host":"example.com","count":5,"maxPacketLossPercent":20}`)); err != nil {
		t.Errorf("valid: %v", err)
	}
	for _, cfg := range []string{`{}`, `{"host":"x","count":50}`, `{"host":"2852039166"}`, `{"host":"x","intervalMs":10}`} {
		if err := reg.Validate(Type, json.RawMessage(cfg)); err == nil {
			t.Errorf("%s accepted", cfg)
		}
	}
}
