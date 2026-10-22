// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package pki

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

// Join tokens have the form "uc1.<token-id>.<secret>.<ca-fingerprint>"
// (ADR 0006). The ID names the stored token, the secret proves possession,
// and the fingerprint lets the joining node pin the cluster CA before it
// sends anything (FA-PKI-02, PF-10). Only a hash of the secret is stored.

const (
	tokenPrefix   = "uc1"
	tokenIDBytes  = 12
	tokenSecBytes = 32
	// fingerprintLen is the length of a base64url SHA-256 without padding.
	fingerprintLen = 43
)

// ErrInvalidToken means the token is malformed. Callers report every token
// failure the same way, so a client cannot tell why it was rejected.
var ErrInvalidToken = errors.New("invalid join token")

// JoinToken is a parsed join token.
type JoinToken struct {
	ID          string
	Secret      string
	Fingerprint string
}

// String formats the token for the operator.
func (t JoinToken) String() string {
	return tokenPrefix + "." + t.ID + "." + t.Secret + "." + t.Fingerprint
}

// NewJoinToken creates a token for the CA with the given fingerprint.
func NewJoinToken(caFingerprint string) (JoinToken, error) {
	if len(caFingerprint) != fingerprintLen {
		return JoinToken{}, errors.New("new join token: invalid CA fingerprint")
	}
	id, err := randomString(tokenIDBytes)
	if err != nil {
		return JoinToken{}, err
	}
	secret, err := randomString(tokenSecBytes)
	if err != nil {
		return JoinToken{}, err
	}
	return JoinToken{ID: id, Secret: secret, Fingerprint: caFingerprint}, nil
}

// ParseJoinToken checks the format strictly.
func ParseJoinToken(s string) (JoinToken, error) {
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) != 4 || parts[0] != tokenPrefix {
		return JoinToken{}, ErrInvalidToken
	}
	t := JoinToken{ID: parts[1], Secret: parts[2], Fingerprint: parts[3]}
	if !isB64(t.ID, tokenIDBytes) || !isB64(t.Secret, tokenSecBytes) || len(t.Fingerprint) != fingerprintLen ||
		!isB64(t.Fingerprint, sha256.Size) {
		return JoinToken{}, ErrInvalidToken
	}
	return t, nil
}

// HashSecret is what the cluster stores. The secret has 256 bits of entropy,
// so a plain domain-separated SHA-256 is sufficient (no password hashing).
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte("uptimecluster join token v1\x00" + secret))
	return hex.EncodeToString(sum[:])
}

// SecretMatches compares a presented secret with a stored hash in constant time.
func SecretMatches(secret, storedHash string) bool {
	h := HashSecret(secret)
	return len(h) == len(storedHash) && subtle.ConstantTimeCompare([]byte(h), []byte(storedHash)) == 1
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func isB64(s string, n int) bool {
	b, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil && len(b) == n
}
