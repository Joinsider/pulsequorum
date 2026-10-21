// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package auth

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestHashVerifyRoundTrip(t *testing.T) {
	const pw = "correct horse battery"
	h, err := HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	ok, needsRehash, err := VerifyPassword(pw, h)
	if err != nil || !ok {
		t.Fatalf("VerifyPassword(correct) = %v, %v, %v; want true", ok, needsRehash, err)
	}
	if needsRehash {
		t.Error("default parameters must not need a rehash")
	}

	ok, _, err = VerifyPassword("correct horse batterY", h)
	if err != nil || ok {
		t.Errorf("VerifyPassword(wrong) = %v, %v; want false, nil", ok, err)
	}
}

func TestHashFormat(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	const prefix = "$argon2id$v=19$m=19456,t=2,p=1$"
	if !strings.HasPrefix(h, prefix) {
		t.Fatalf("hash = %q, want prefix %q", h, prefix)
	}
	parts := strings.Split(h, "$")
	if len(parts) != 6 {
		t.Fatalf("hash has %d parts, want 6", len(parts))
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) != 16 {
		t.Errorf("salt = %d bytes, %v; want 16", len(salt), err)
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) != 32 {
		t.Errorf("key = %d bytes, %v; want 32", len(key), err)
	}
}

func TestHashesOfSamePasswordDiffer(t *testing.T) {
	a, err := HashPassword("same password here")
	if err != nil {
		t.Fatal(err)
	}
	b, err := HashPassword("same password here")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two hashes of the same password are equal; the salt is not random")
	}
}

func TestNeedsRehash(t *testing.T) {
	weak := func(mem, tm uint32) string {
		h, err := hashWith("correct horse battery", Argon2Params{MemoryKiB: mem, Time: tm, Threads: 1, SaltLen: 16, KeyLen: 32})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	tests := []struct {
		name string
		hash string
		want bool
	}{
		{"less memory", weak(8*1024, 2), true},
		{"less time", weak(DefaultArgon2.MemoryKiB, 1), true},
		{"default", weak(DefaultArgon2.MemoryKiB, DefaultArgon2.Time), false},
		{"stronger", weak(DefaultArgon2.MemoryKiB*2, DefaultArgon2.Time+1), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, needs, err := VerifyPassword("correct horse battery", tt.hash)
			if err != nil || !ok {
				t.Fatalf("verify = %v, %v; want true", ok, err)
			}
			if needs != tt.want {
				t.Errorf("needsRehash = %v, want %v", needs, tt.want)
			}
		})
	}
}

func TestMalformedHashes(t *testing.T) {
	b64 := base64.RawStdEncoding.EncodeToString
	salt := b64(make([]byte, 16))
	key := b64(make([]byte, 32))
	tests := []struct {
		name string
		hash string
	}{
		{"empty", ""},
		{"not a hash", "hunter2"},
		{"wrong algorithm", "$argon2i$v=19$m=19456,t=2,p=1$" + salt + "$" + key},
		{"bad version", "$argon2id$v=18$m=19456,t=2,p=1$" + salt + "$" + key},
		{"non numeric version", "$argon2id$v=x$m=19456,t=2,p=1$" + salt + "$" + key},
		{"bad params", "$argon2id$v=19$m=abc,t=2,p=1$" + salt + "$" + key},
		{"absurd memory", "$argon2id$v=19$m=99999999,t=2,p=1$" + salt + "$" + key},
		{"memory too small", "$argon2id$v=19$m=4,t=2,p=1$" + salt + "$" + key},
		{"zero time", "$argon2id$v=19$m=19456,t=0,p=1$" + salt + "$" + key},
		{"absurd time", "$argon2id$v=19$m=19456,t=1000,p=1$" + salt + "$" + key},
		{"zero threads", "$argon2id$v=19$m=19456,t=2,p=0$" + salt + "$" + key},
		{"too many threads", "$argon2id$v=19$m=19456,t=2,p=17$" + salt + "$" + key},
		{"threads above bound", "$argon2id$v=19$m=19456,t=2,p=5$" + salt + "$" + key},
		{"memory above 256 MiB", "$argon2id$v=19$m=262145,t=2,p=1$" + salt + "$" + key},
		{"threads overflow uint8", "$argon2id$v=19$m=19456,t=2,p=300$" + salt + "$" + key},
		{"bad salt encoding", "$argon2id$v=19$m=19456,t=2,p=1$!!!$" + key},
		{"short salt", "$argon2id$v=19$m=19456,t=2,p=1$" + b64(make([]byte, 4)) + "$" + key},
		{"short key", "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + b64(make([]byte, 8))},
		{"key too long", "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + b64(make([]byte, 1025))},
		{"bad key encoding", "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$%%%"},
		{"extra part", "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + key + "$x"},
		{"missing leading dollar", "argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + key},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ok bool
			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("VerifyPassword panicked: %v", r)
					}
				}()
				ok, _, err = VerifyPassword("correct horse battery", tt.hash)
			}()
			if ok || !errors.Is(err, ErrInvalidHash) {
				t.Errorf("VerifyPassword = %v, %v; want false, ErrInvalidHash", ok, err)
			}
		})
	}
}

func TestDummyVerifyDoesNotPanic(_ *testing.T) {
	DummyVerify("")
	DummyVerify("some password that is not stored anywhere")
}
