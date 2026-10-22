// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package secrets implements envelope encryption for secrets stored in the
// replicated state (ADR 0011, FA-SEC-SEC): a master key (KEK) wraps data
// encryption keys (DEKs), and DEKs encrypt individual values with AES-256-GCM.
// Only the standard library is used.
package secrets

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// KeySize is the size of KEKs and DEKs in bytes (AES-256).
const KeySize = 32

// GeneratedKEKFile is the file a KEK is generated into when none is configured
// (ADR 0011 option B).
const GeneratedKEKFile = "master.key"

// KEK sources.
const (
	SourceEnv       = "MASTER_KEY"
	SourceFile      = "MASTER_KEY_FILE"
	SourceGenerated = "generated"
)

// KEK is the master key. It never leaves the process except during a join
// over mTLS (phase 2).
type KEK struct {
	aead cipher.AEAD
	// Fingerprint identifies the key without revealing it.
	Fingerprint string
	// Source tells where the key came from (SourceEnv, SourceFile, SourceGenerated).
	Source string
	// generated keeps a generated key for the hand-over to joining voters
	// (ADR 0011 option B); external keys are never kept or handed over.
	generated []byte
}

// Transferable returns the key for a joining voter, only if it was generated
// in the data directory. Operators who configure MASTER_KEY or
// MASTER_KEY_FILE provide the same key to every voter themselves.
func (k *KEK) Transferable() ([]byte, bool) {
	if k.Source != SourceGenerated || len(k.generated) != KeySize {
		return nil, false
	}
	return append([]byte(nil), k.generated...), true
}

// KeyFingerprint returns the fingerprint a key would have as KEK.
func KeyFingerprint(key []byte) string {
	sum := sha256.Sum256(append([]byte("uptimecluster-kek-v1:"), key...))
	return hex.EncodeToString(sum[:16])
}

// InstallGenerated stores a master key received at join in dataDir, as if it
// had been generated there. It never overwrites an existing key; an existing
// key must be the same one (fingerprint), otherwise the node belongs to
// another cluster.
func InstallGenerated(dataDir string, key []byte, fingerprint string) error {
	if len(key) != KeySize || KeyFingerprint(key) != fingerprint {
		return errors.New("received master key does not match its fingerprint")
	}
	path := filepath.Join(dataDir, GeneratedKEKFile)
	if raw, err := os.ReadFile(path); err == nil { //nolint:gosec // G304: path is inside the data directory
		existing, err := decodeKey(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if KeyFingerprint(existing) != fingerprint {
			return fmt.Errorf("%s holds a different master key than the cluster; this data directory belongs to another cluster", path)
		}
		return nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: path is inside the data directory
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	_, werr := f.WriteString(base64.StdEncoding.EncodeToString(key) + "\n")
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// LoadKEK returns the master key from envKey (base64), from keyFile (base64 or
// 32 raw bytes) or, if neither is set, from dataDir/master.key, which is
// generated with mode 0600 on first start.
func LoadKEK(dataDir, envKey, keyFile string) (*KEK, error) {
	switch {
	case envKey != "" && keyFile != "":
		return nil, errors.New("set either MASTER_KEY or MASTER_KEY_FILE, not both")
	case envKey != "":
		key, err := decodeKey([]byte(envKey))
		if err != nil {
			return nil, fmt.Errorf("MASTER_KEY: %w", err)
		}
		return newKEK(key, SourceEnv)
	case keyFile != "":
		raw, err := os.ReadFile(keyFile) //nolint:gosec // G304: path is operator configuration
		if err != nil {
			return nil, fmt.Errorf("MASTER_KEY_FILE: %w", err)
		}
		key, err := decodeKey(raw)
		if err != nil {
			return nil, fmt.Errorf("MASTER_KEY_FILE %s: %w", keyFile, err)
		}
		return newKEK(key, SourceFile)
	default:
		return loadOrGenerate(filepath.Join(dataDir, GeneratedKEKFile))
	}
}

func decodeKey(raw []byte) ([]byte, error) {
	if len(raw) == KeySize {
		return raw, nil
	}
	trimmed := bytes.TrimSpace(raw)
	key, err := base64.StdEncoding.DecodeString(string(trimmed))
	if err != nil {
		return nil, fmt.Errorf("expected %d raw bytes or base64: %w", KeySize, err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("key has %d bytes, want %d", len(key), KeySize)
	}
	return key, nil
}

func loadOrGenerate(path string) (*KEK, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is inside the data directory
	if err == nil {
		key, err := decodeKey(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return newGeneratedKEK(key)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate master key: %w", err)
	}
	// O_EXCL: never overwrite a key another process created meanwhile.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: path is inside the data directory
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", path, err)
	}
	_, werr := f.WriteString(base64.StdEncoding.EncodeToString(key) + "\n")
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("write %s: %w", path, err)
	}
	return newGeneratedKEK(key)
}

func newGeneratedKEK(key []byte) (*KEK, error) {
	k, err := newKEK(key, SourceGenerated)
	if err != nil {
		return nil, err
	}
	k.generated = append([]byte(nil), key...)
	return k, nil
}

func newKEK(key []byte, source string) (*KEK, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	return &KEK{aead: aead, Fingerprint: KeyFingerprint(key), Source: source}, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// NewDEK returns a fresh random data encryption key.
func NewDEK() ([]byte, error) {
	dek := make([]byte, KeySize)
	if _, err := rand.Read(dek); err != nil {
		return nil, fmt.Errorf("generate data key: %w", err)
	}
	return dek, nil
}

// Wrap encrypts a DEK with the KEK, bound to its key ID.
func (k *KEK) Wrap(keyID string, dek []byte) ([]byte, error) {
	if len(dek) != KeySize {
		return nil, fmt.Errorf("data key has %d bytes, want %d", len(dek), KeySize)
	}
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return k.aead.Seal(nonce, nonce, dek, wrapAAD(keyID)), nil
}

// Unwrap decrypts a DEK wrapped by Wrap.
func (k *KEK) Unwrap(keyID string, wrapped []byte) ([]byte, error) {
	ns := k.aead.NonceSize()
	if len(wrapped) < ns {
		return nil, errors.New("wrapped data key too short")
	}
	dek, err := k.aead.Open(nil, wrapped[:ns], wrapped[ns:], wrapAAD(keyID))
	if err != nil {
		return nil, fmt.Errorf("unwrap data key %s: %w", keyID, err)
	}
	return dek, nil
}

func wrapAAD(keyID string) []byte { return []byte("uptimecluster-dek-v1/" + keyID) }
