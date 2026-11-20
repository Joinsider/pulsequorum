// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2Params are the Argon2id cost parameters (ADR 0008, FA-AUTH-01).
type Argon2Params struct {
	MemoryKiB uint32
	Time      uint32
	Threads   uint8
	SaltLen   uint32
	KeyLen    uint32
}

// DefaultArgon2 is the OWASP minimum: m=19 MiB, t=2, p=1.
var DefaultArgon2 = Argon2Params{MemoryKiB: 19 * 1024, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32}

// Password length limits (bytes); the upper bound caps hashing cost.
const (
	MinPasswordLen = 12
	MaxPasswordLen = 256
)

// ErrInvalidHash is returned for hashes that are not a supported PHC string.
var ErrInvalidHash = errors.New("invalid password hash")

// HashPassword returns an Argon2id hash in PHC format
// ($argon2id$v=19$m=…,t=…,p=…$salt$hash); parameters travel with the hash so
// they can be raised later.
func HashPassword(password string) (string, error) {
	return hashWith(password, DefaultArgon2)
}

func hashWith(password string, p Argon2Params) (string, error) {
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("password salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, p.Time, p.MemoryKiB, p.Threads, p.KeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.MemoryKiB, p.Time, p.Threads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword checks password against an encoded hash in constant time.
// needsRehash reports weaker parameters than DefaultArgon2.
func VerifyPassword(password, encoded string) (ok, needsRehash bool, err error) {
	p, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, false, err
	}
	got := argon2.IDKey([]byte(password), salt, p.Time, p.MemoryKiB, p.Threads, uint32(len(want))) //nolint:gosec // len(want) ≤ 1024 (checked)
	ok = subtle.ConstantTimeCompare(got, want) == 1
	needsRehash = p.MemoryKiB < DefaultArgon2.MemoryKiB || p.Time < DefaultArgon2.Time
	return ok, needsRehash, nil
}

// dummyHash is verified for unknown users, so a login takes the same time
// whether or not the account exists (FA-AUTH-04).
var dummyHash = func() string {
	h, err := HashPassword("dummy password for timing equalization")
	if err != nil {
		panic(err)
	}
	return h
}()

// DummyVerify spends the same work as VerifyPassword and always fails.
func DummyVerify(password string) {
	_, _, _ = VerifyPassword(password, dummyHash)
}

func decodeHash(encoded string) (Argon2Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}
	var p Argon2Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.MemoryKiB, &p.Time, &p.Threads); err != nil {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}
	// Bounds keep a corrupted hash from exhausting memory or CPU.
	if p.MemoryKiB < 8 || p.MemoryKiB > 256*1024 || p.Time < 1 || p.Time > 16 || p.Threads < 1 || p.Threads > 4 {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) < 16 || len(key) > 1024 {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}
	return p, salt, key, nil
}
