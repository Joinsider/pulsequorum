// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joinsider/uptimecluster/internal/store"
)

func testKey(b byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, KeySize))
}

func TestLoadKEK(t *testing.T) {
	dir := t.TempDir()

	env, err := LoadKEK(dir, testKey(1), "")
	if err != nil || env.Source != SourceEnv || len(env.Fingerprint) != 32 {
		t.Fatalf("env KEK = %+v, %v", env, err)
	}

	file := filepath.Join(dir, "kek")
	if err := os.WriteFile(file, []byte(testKey(1)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fromFile, err := LoadKEK(dir, "", file)
	if err != nil || fromFile.Source != SourceFile || fromFile.Fingerprint != env.Fingerprint {
		t.Fatalf("file KEK = %+v, %v; want same fingerprint as env", fromFile, err)
	}
	raw := filepath.Join(dir, "kek.raw")
	if err := os.WriteFile(raw, bytes.Repeat([]byte{1}, KeySize), 0o600); err != nil {
		t.Fatal(err)
	}
	if k, err := LoadKEK(dir, "", raw); err != nil || k.Fingerprint != env.Fingerprint {
		t.Fatalf("raw file KEK = %+v, %v", k, err)
	}

	gen, err := LoadKEK(dir, "", "")
	if err != nil || gen.Source != SourceGenerated {
		t.Fatalf("generated KEK = %+v, %v", gen, err)
	}
	info, err := os.Stat(filepath.Join(dir, GeneratedKEKFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("master.key mode = %v, %v; want 0600", info.Mode(), err)
	}
	again, err := LoadKEK(dir, "", "")
	if err != nil || again.Fingerprint != gen.Fingerprint {
		t.Fatalf("generated KEK not stable: %v", err)
	}
	if gen.Fingerprint == env.Fingerprint {
		t.Error("generated key equals a fixed test key")
	}
}

func TestLoadKEKErrors(t *testing.T) {
	dir := t.TempDir()
	tests := map[string]struct{ env, file string }{
		"both set":     {testKey(1), "/tmp/x"},
		"not base64":   {"!!!", ""},
		"wrong length": {base64.StdEncoding.EncodeToString([]byte("short")), ""},
		"missing file": {"", filepath.Join(dir, "missing")},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadKEK(dir, tt.env, tt.file); err == nil {
				t.Fatal("error expected")
			}
		})
	}
}

func TestWrapUnwrap(t *testing.T) {
	kek, _ := LoadKEK("", testKey(1), "")
	other, _ := LoadKEK("", testKey(2), "")
	dek, err := NewDEK()
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := kek.Wrap("k1", dek)
	if err != nil {
		t.Fatal(err)
	}
	got, err := kek.Unwrap("k1", wrapped)
	if err != nil || !bytes.Equal(got, dek) {
		t.Fatalf("unwrap = %v", err)
	}
	if _, err := kek.Unwrap("k2", wrapped); err == nil {
		t.Error("wrapped key accepted under another key ID")
	}
	if _, err := other.Unwrap("k1", wrapped); err == nil {
		t.Error("wrapped key accepted with another KEK")
	}
	if _, err := kek.Wrap("k1", []byte("short")); err == nil {
		t.Error("short DEK accepted")
	}
}

// memKeys simulates the replicated secret_keys table.
type memKeys struct{ keys []store.SecretKey }

func (m *memKeys) load(context.Context) ([]store.SecretKey, error) { return m.keys, nil }

func (m *memKeys) add(t *testing.T, kek *KEK, id string, created int64) {
	t.Helper()
	dek, _ := NewDEK()
	wrapped, err := kek.Wrap(id, dek)
	if err != nil {
		t.Fatal(err)
	}
	m.keys = append(m.keys, store.SecretKey{KeyID: id, WrappedDEK: wrapped, KEKFingerprint: kek.Fingerprint, CreatedMs: created})
}

func TestKeyringEncryptDecrypt(t *testing.T) {
	ctx := context.Background()
	kek, _ := LoadKEK("", testKey(1), "")
	keys := &memKeys{}
	ring := NewKeyring(kek, keys.load)
	if err := ring.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := ring.Encrypt([]byte("x"), "a"); !errors.Is(err, ErrNoActiveKey) {
		t.Fatalf("encrypt without key = %v, want ErrNoActiveKey", err)
	}

	keys.add(t, kek, "k1", 1)
	if err := ring.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	secret := []byte("telegram://token@telegram?chats=@ops")
	aad := "notification_channels/c1/url"
	c1, err := ring.Encrypt(secret, aad)
	if err != nil {
		t.Fatal(err)
	}
	c2, _ := ring.Encrypt(secret, aad)
	if c1 == c2 {
		t.Error("two encryptions are identical (nonce reuse)")
	}
	if !IsEncrypted(c1) || strings.Contains(c1, "token") {
		t.Errorf("ciphertext %q leaks plaintext or lacks prefix", c1)
	}
	if id, ok := KeyID(c1); !ok || id != "k1" {
		t.Errorf("KeyID = %q, %v", id, ok)
	}

	plain, err := ring.Decrypt(ctx, c1, aad)
	if err != nil || !bytes.Equal(plain, secret) {
		t.Fatalf("decrypt = %q, %v", plain, err)
	}

	// Copying the value to another record must fail (AAD binding).
	if _, err := ring.Decrypt(ctx, c1, "notification_channels/c2/url"); !errors.Is(err, ErrDecrypt) {
		t.Errorf("decrypt with other AAD = %v, want ErrDecrypt", err)
	}
	// Tampering must fail.
	tampered := c1[:len(c1)-2] + "AA"
	if tampered == c1 {
		tampered = c1[:len(c1)-2] + "BB"
	}
	if _, err := ring.Decrypt(ctx, tampered, aad); !errors.Is(err, ErrDecrypt) {
		t.Errorf("decrypt of tampered value = %v, want ErrDecrypt", err)
	}
	for _, bad := range []string{"plain", "enc:v1:", "enc:v1:k1:%%%", "enc:v1:k1:AAAA", "enc:v1:unknown:" + strings.Split(c1, ":")[3]} {
		if _, err := ring.Decrypt(ctx, bad, aad); !errors.Is(err, ErrDecrypt) {
			t.Errorf("Decrypt(%q) = %v, want ErrDecrypt", bad, err)
		}
	}
}

func TestKeyringRotation(t *testing.T) {
	ctx := context.Background()
	kek, _ := LoadKEK("", testKey(1), "")
	keys := &memKeys{}
	keys.add(t, kek, "k1", 1)
	ring := NewKeyring(kek, keys.load)
	if err := ring.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	old, _ := ring.Encrypt([]byte("old"), "x")

	// A new key arrives through Raft; a second keyring (another node) has not
	// reloaded yet and must pick it up on demand.
	stale := NewKeyring(kek, keys.load)
	_ = stale.Reload(ctx)
	keys.add(t, kek, "k2", 2)
	keys.keys[0].RetiredMs = 3
	if err := ring.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if ring.ActiveKeyID() != "k2" {
		t.Fatalf("active key = %q, want k2", ring.ActiveKeyID())
	}
	fresh, _ := ring.Encrypt([]byte("new"), "x")
	if id, _ := KeyID(fresh); id != "k2" {
		t.Errorf("new value encrypted with %s, want k2", id)
	}
	if p, err := ring.Decrypt(ctx, old, "x"); err != nil || string(p) != "old" {
		t.Errorf("old value no longer readable: %v", err)
	}
	if p, err := stale.Decrypt(ctx, fresh, "x"); err != nil || string(p) != "new" {
		t.Errorf("stale keyring did not reload: %v", err)
	}
}

func TestKeyringKEKMismatch(t *testing.T) {
	kek1, _ := LoadKEK("", testKey(1), "")
	kek2, _ := LoadKEK("", testKey(2), "")
	keys := &memKeys{}
	keys.add(t, kek1, "k1", 1)
	err := NewKeyring(kek2, keys.load).Reload(context.Background())
	if !errors.Is(err, ErrKEKMismatch) {
		t.Fatalf("Reload with other KEK = %v, want ErrKEKMismatch", err)
	}
}

func TestSecretIsMasked(t *testing.T) {
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "url", Secret("telegram://token@telegram"))
	if strings.Contains(buf.String(), "token") || fmt.Sprint(Secret("token")) != "***" {
		t.Errorf("secret leaked: %s", buf.String())
	}
}
