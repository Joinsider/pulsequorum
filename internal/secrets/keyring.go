// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package secrets

import (
	"context"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/joinsider/pulsequorum/internal/store"
)

// Errors of the keyring.
var (
	// ErrKEKMismatch means the replicated DEKs are wrapped with a different
	// master key than the one this node was started with (ADR 0011).
	ErrKEKMismatch = errors.New("master key does not match the cluster")
	// ErrNoActiveKey means no data key exists yet (the leader creates one at startup).
	ErrNoActiveKey = errors.New("no active data key")
	// ErrDecrypt covers malformed or tampered ciphertexts and wrong AAD.
	ErrDecrypt = errors.New("cannot decrypt secret")
)

// prefix marks encrypted values: "enc:v1:<key-id>:<base64(nonce|ciphertext)>".
const prefix = "enc:v1:"

// KeyLoader returns all data keys from the replicated state.
type KeyLoader func(ctx context.Context) ([]store.SecretKey, error)

// Keyring encrypts and decrypts values with the cluster's data keys. New keys
// arrive through Raft; an unknown key ID triggers a reload.
type Keyring struct {
	kek  *KEK
	load KeyLoader

	mu     sync.RWMutex
	keys   map[string]cipher.AEAD
	active string
}

// NewKeyring creates a keyring; call Reload before use.
func NewKeyring(kek *KEK, load KeyLoader) *Keyring {
	return &Keyring{kek: kek, load: load, keys: map[string]cipher.AEAD{}}
}

// Reload reads all data keys and unwraps them. It fails with ErrKEKMismatch
// if any key is wrapped with another master key.
func (r *Keyring) Reload(ctx context.Context) error {
	stored, err := r.load(ctx)
	if err != nil {
		return fmt.Errorf("load data keys: %w", err)
	}
	keys := make(map[string]cipher.AEAD, len(stored))
	active := ""
	for _, k := range stored {
		if k.KEKFingerprint != r.kek.Fingerprint {
			return fmt.Errorf("%w: data key %s is wrapped with master key %s, this node has %s",
				ErrKEKMismatch, k.KeyID, k.KEKFingerprint, r.kek.Fingerprint)
		}
		dek, err := r.kek.Unwrap(k.KeyID, k.WrappedDEK)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrKEKMismatch, err)
		}
		aead, err := newAEAD(dek)
		if err != nil {
			return err
		}
		keys[k.KeyID] = aead
		if k.RetiredMs == 0 {
			active = k.KeyID // keys are ordered by creation; the newest wins
		}
	}
	r.mu.Lock()
	r.keys, r.active = keys, active
	r.mu.Unlock()
	return nil
}

// ActiveKeyID returns the key new values are encrypted with ("" if none).
func (r *Keyring) ActiveKeyID() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.active
}

// Encrypt encrypts plaintext with the active key. aad binds the ciphertext to
// its place, e.g. "notification_channels/<id>/url", so it cannot be copied to
// another record (ADR 0011).
func (r *Keyring) Encrypt(plaintext []byte, aad string) (string, error) {
	r.mu.RLock()
	keyID, aead := r.active, r.keys[r.active]
	r.mu.RUnlock()
	if aead == nil {
		return "", ErrNoActiveKey
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("encrypt: %w", err)
	}
	sealed := aead.Seal(nonce, nonce, plaintext, valueAAD(keyID, aad))
	return prefix + keyID + ":" + base64.RawStdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt. Values encrypted with older (rotated) keys remain
// readable as long as the key exists.
func (r *Keyring) Decrypt(ctx context.Context, ciphertext, aad string) ([]byte, error) {
	keyID, payload, ok := parse(ciphertext)
	if !ok {
		return nil, fmt.Errorf("%w: not an encrypted value", ErrDecrypt)
	}
	aead, err := r.key(ctx, keyID)
	if err != nil {
		return nil, err
	}
	ns := aead.NonceSize()
	if len(payload) < ns {
		return nil, fmt.Errorf("%w: ciphertext too short", ErrDecrypt)
	}
	plain, err := aead.Open(nil, payload[:ns], payload[ns:], valueAAD(keyID, aad))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDecrypt, err)
	}
	return plain, nil
}

// KeyID returns the key a value is encrypted with, so a rotation job can find
// values that still use an old key.
func KeyID(ciphertext string) (string, bool) {
	keyID, _, ok := parse(ciphertext)
	return keyID, ok
}

// IsEncrypted reports whether s has the format produced by Encrypt.
func IsEncrypted(s string) bool { return strings.HasPrefix(s, prefix) }

func (r *Keyring) key(ctx context.Context, keyID string) (cipher.AEAD, error) {
	r.mu.RLock()
	aead := r.keys[keyID]
	r.mu.RUnlock()
	if aead != nil {
		return aead, nil
	}
	if err := r.Reload(ctx); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if aead = r.keys[keyID]; aead == nil {
		return nil, fmt.Errorf("%w: unknown data key %s", ErrDecrypt, keyID)
	}
	return aead, nil
}

func parse(ciphertext string) (keyID string, payload []byte, ok bool) {
	rest, found := strings.CutPrefix(ciphertext, prefix)
	if !found {
		return "", nil, false
	}
	keyID, encoded, found := strings.Cut(rest, ":")
	if !found || keyID == "" {
		return "", nil, false
	}
	payload, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return "", nil, false
	}
	return keyID, payload, true
}

func valueAAD(keyID, aad string) []byte {
	return []byte("pulsequorum-secret-v1/" + keyID + "/" + aad)
}

// Secret is a string that never appears in logs (ADR 0011).
type Secret string

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue("***") }

// String hides the value in fmt output as well.
func (Secret) String() string { return "***" }
