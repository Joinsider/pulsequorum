// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package cluster

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/uptimecluster/internal/secrets"
	"github.com/joinsider/uptimecluster/internal/store"
)

// DefaultKeyRotation is the automatic data key rotation period (ADR 0011).
const DefaultKeyRotation = 365 * 24 * time.Hour

const bootstrapSecretKeyRequestID = "bootstrap:secret-key"

// EnsureSecretKey creates the first data key if none is active. Only the
// leader proposes it; followers receive it through Raft.
func EnsureSecretKey(ctx context.Context, n *Node, state store.State, kek *secrets.KEK) error {
	if err := n.WaitLeader(ctx); err != nil {
		return err
	}
	if _, ok, err := activeKey(ctx, state); err != nil || ok {
		return err
	}
	if !n.IsLeader() {
		return nil
	}
	return RotateSecretKey(ctx, n, kek, bootstrapSecretKeyRequestID)
}

// RotateSecretKey generates a new data key on the leader, wraps it with the
// master key and stores it through Raft. It becomes the active key; older keys
// stay available for decryption.
func RotateSecretKey(ctx context.Context, n *Node, kek *secrets.KEK, requestID string) error {
	keyID, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("data key ID: %w", err)
	}
	dek, err := secrets.NewDEK()
	if err != nil {
		return err
	}
	wrapped, err := kek.Wrap(keyID.String(), dek)
	if err != nil {
		return err
	}
	cmd, err := NewPutSecretKey(requestID, keyID.String(), SecretKeyDocument{WrappedDEK: wrapped, KEKFingerprint: kek.Fingerprint})
	if err != nil {
		return err
	}
	if _, err := n.Apply(ctx, cmd); err != nil {
		return fmt.Errorf("store data key: %w", err)
	}
	return nil
}

// RunKeyRotation rotates the data key on the leader once the active key is
// older than maxAge. It checks every interval until ctx ends.
func RunKeyRotation(ctx context.Context, n *Node, state store.State, kek *secrets.KEK, maxAge, interval time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !n.IsLeader() {
			continue
		}
		key, ok, err := activeKey(ctx, state)
		if err != nil || !ok || n.now().Sub(time.UnixMilli(key.CreatedMs)) < maxAge {
			continue
		}
		if err := RotateSecretKey(ctx, n, kek, "rotate:"+key.KeyID); err != nil {
			log.Warn("data key rotation failed", "err", err)
			continue
		}
		log.Info("rotated data key", "previous_key", key.KeyID)
	}
}

func activeKey(ctx context.Context, state store.State) (store.SecretKey, bool, error) {
	keys, err := state.ListSecretKeys(ctx)
	if err != nil {
		return store.SecretKey{}, false, err
	}
	for i := len(keys) - 1; i >= 0; i-- {
		if keys[i].RetiredMs == 0 {
			return keys[i], true, nil
		}
	}
	return store.SecretKey{}, false, nil
}
