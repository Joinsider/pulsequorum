// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package cluster

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/joinsider/uptimecluster/internal/secrets"
)

func TestSecretKeyBootstrapAndRotation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dir := t.TempDir()
	st := openStore(t, dir)
	n, err := Start(ctx, Config{NodeID: "33333333-3333-3333-3333-333333333333", Advertise: "node1:9443", DataDir: dir,
		Timeout: 200 * time.Millisecond, Logger: slog.New(slog.DiscardHandler), RaftLogLevel: "error"}, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.Shutdown() }()

	kek, err := secrets.LoadKEK(dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 { // idempotent
		if err := EnsureSecretKey(ctx, n, st, kek); err != nil {
			t.Fatal(err)
		}
	}
	keys, err := st.ListSecretKeys(ctx)
	if err != nil || len(keys) != 1 || keys[0].KEKFingerprint != kek.Fingerprint || keys[0].CreatedMs == 0 {
		t.Fatalf("keys = %+v, %v", keys, err)
	}

	ring := secrets.NewKeyring(kek, st.ListSecretKeys)
	if err := ring.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	enc, err := ring.Encrypt([]byte("s3cret"), "test/1/value")
	if err != nil {
		t.Fatal(err)
	}

	if err := RotateSecretKey(ctx, n, kek, "manual-rotation"); err != nil {
		t.Fatal(err)
	}
	if err := ring.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if ring.ActiveKeyID() == keys[0].KeyID {
		t.Error("rotation did not change the active key")
	}
	if p, err := ring.Decrypt(ctx, enc, "test/1/value"); err != nil || string(p) != "s3cret" {
		t.Errorf("value from before the rotation = %q, %v", p, err)
	}

	// A node started with a different master key must refuse (ADR 0011).
	other, _ := secrets.LoadKEK("", "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=", "")
	if err := secrets.NewKeyring(other, st.ListSecretKeys).Reload(ctx); !errors.Is(err, secrets.ErrKEKMismatch) {
		t.Errorf("Reload with other KEK = %v, want ErrKEKMismatch", err)
	}
}
