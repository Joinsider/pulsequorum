// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package secrets

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/store"
)

// syncKeys is a replicated secret_keys table that Raft fills while Refresh reads it.
type syncKeys struct {
	mu   sync.Mutex
	keys []store.SecretKey
}

func (s *syncKeys) load(context.Context) ([]store.SecretKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.SecretKey(nil), s.keys...), nil
}

func (s *syncKeys) add(t *testing.T, kek *KEK, id string, created int64, retirePrevious bool) {
	t.Helper()
	dek, _ := NewDEK()
	wrapped, err := kek.Wrap(id, dek)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if retirePrevious && len(s.keys) > 0 {
		s.keys[len(s.keys)-1].RetiredMs = created
	}
	s.keys = append(s.keys, store.SecretKey{KeyID: id, WrappedDEK: wrapped, KEKFingerprint: kek.Fingerprint, CreatedMs: created})
}

func waitActive(t *testing.T, ring *Keyring, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for ring.ActiveKeyID() != want {
		if time.Now().After(deadline) {
			t.Fatalf("active key = %q, want %q", ring.ActiveKeyID(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// A voter that joins loads its keyring before it has caught up. Refresh must
// give it the cluster's key, and later the rotated one, without a restart.
func TestKeyringRefreshAfterJoinAndRotation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kek, _ := LoadKEK("", testKey(1), "")
	keys := &syncKeys{}
	ring := NewKeyring(kek, keys.load)
	if err := ring.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := ring.Encrypt([]byte("x"), "a"); !errors.Is(err, ErrNoActiveKey) {
		t.Fatalf("encrypt before catch-up = %v, want ErrNoActiveKey", err)
	}

	done := make(chan error, 1)
	go func() { done <- ring.Refresh(ctx, time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil))) }()

	keys.add(t, kek, "k1", 1, false) // the log catches up
	waitActive(t, ring, "k1")
	if c, err := ring.Encrypt([]byte("x"), "a"); err != nil {
		t.Fatalf("encrypt after catch-up: %v", err)
	} else if id, _ := KeyID(c); id != "k1" {
		t.Errorf("encrypted with %q, want k1", id)
	}

	keys.add(t, kek, "k2", 2, true) // the leader rotates
	waitActive(t, ring, "k2")

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Refresh after cancel = %v, want nil", err)
	}
}

// A master key that does not match the cluster stops Refresh, so the node stops.
func TestKeyringRefreshKEKMismatch(t *testing.T) {
	kek1, _ := LoadKEK("", testKey(1), "")
	kek2, _ := LoadKEK("", testKey(2), "")
	keys := &syncKeys{}
	ring := NewKeyring(kek2, keys.load)
	keys.add(t, kek1, "k1", 1, false)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := ring.Refresh(ctx, time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !errors.Is(err, ErrKEKMismatch) {
		t.Fatalf("Refresh with other KEK = %v, want ErrKEKMismatch", err)
	}
}
