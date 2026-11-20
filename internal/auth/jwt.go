// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package auth

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Access tokens are JWTs signed with Ed25519 (ADR 0008, FA-AUTH-02). The
// implementation is deliberately small: one algorithm, fixed header, strict
// claim checks; anything unexpected is rejected.
const (
	jwtAlg      = "EdDSA"
	jwtTyp      = "JWT"
	Issuer      = "pulsequorum"
	Audience    = "pulsequorum"
	AccessTTL   = 15 * time.Minute
	clockLeeway = 30 * time.Second
	maxJWTLen   = 4096
)

// ErrInvalidToken is returned for every token that must not be accepted. The
// reason is not exposed to callers.
var ErrInvalidToken = errors.New("invalid access token")

// Claims are the access token claims.
type Claims struct {
	Issuer    string   `json:"iss"`
	Audience  string   `json:"aud"`
	Subject   string   `json:"sub"` // user ID
	TenantID  string   `json:"tid"`
	Roles     []string `json:"roles"`
	IssuedAt  int64    `json:"iat"`
	NotBefore int64    `json:"nbf"`
	ExpiresAt int64    `json:"exp"`
	ID        string   `json:"jti"`
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

// SignAccessToken returns a signed token for the claims.
func SignAccessToken(kid string, key ed25519.PrivateKey, c Claims) (string, error) {
	if kid == "" || len(key) != ed25519.PrivateKeySize {
		return "", errors.New("sign access token: invalid signing key")
	}
	header, err := json.Marshal(jwtHeader{Alg: jwtAlg, Typ: jwtTyp, Kid: kid})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	b64 := base64.RawURLEncoding
	signingInput := b64.EncodeToString(header) + "." + b64.EncodeToString(payload)
	sig := ed25519.Sign(key, []byte(signingInput))
	return signingInput + "." + b64.EncodeToString(sig), nil
}

// KeyLookup returns the public key for a key ID, or false if the key is
// unknown or no longer valid for verification.
type KeyLookup func(kid string) (ed25519.PublicKey, bool)

// VerifyAccessToken checks signature, header and claims at now and returns the
// claims. Every failure is ErrInvalidToken.
func VerifyAccessToken(token string, keys KeyLookup, now time.Time) (Claims, error) {
	if len(token) > maxJWTLen {
		return Claims{}, ErrInvalidToken
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, ErrInvalidToken
	}
	b64 := base64.RawURLEncoding
	var h jwtHeader
	if err := strictUnmarshal(b64, parts[0], &h); err != nil {
		return Claims{}, ErrInvalidToken
	}
	// The algorithm is fixed; the header is not trusted to choose it.
	if h.Alg != jwtAlg || h.Typ != jwtTyp || h.Kid == "" {
		return Claims{}, ErrInvalidToken
	}
	pub, ok := keys(h.Kid)
	if !ok || len(pub) != ed25519.PublicKeySize {
		return Claims{}, ErrInvalidToken
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return Claims{}, ErrInvalidToken
	}
	var c Claims
	if err := strictUnmarshal(b64, parts[1], &c); err != nil {
		return Claims{}, ErrInvalidToken
	}
	n := now.Unix()
	leeway := int64(clockLeeway / time.Second)
	switch {
	case c.Issuer != Issuer, c.Audience != Audience:
		return Claims{}, ErrInvalidToken
	case c.Subject == "", c.TenantID == "", len(c.Roles) == 0:
		return Claims{}, ErrInvalidToken
	case c.ExpiresAt <= n-leeway, c.NotBefore > n+leeway, c.IssuedAt > n+leeway:
		return Claims{}, ErrInvalidToken
	case c.ExpiresAt-c.IssuedAt > int64(AccessTTL/time.Second):
		return Claims{}, ErrInvalidToken
	}
	for _, r := range c.Roles {
		if !ValidRole(Role(r)) {
			return Claims{}, ErrInvalidToken
		}
	}
	return c, nil
}

// strictUnmarshal decodes one base64url JSON segment and rejects unknown
// fields and trailing data.
func strictUnmarshal(enc *base64.Encoding, segment string, v any) error {
	raw, err := enc.DecodeString(segment)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	// More() misses a stray '}' or ']'; only EOF is acceptable after the value.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("trailing data")
	}
	return nil
}

// ValidRole reports whether r is one of the four roles (FA-AUTH-09).
func ValidRole(r Role) bool {
	switch r {
	case RolePlatformAdmin, RoleTenantAdmin, RoleTenantEditor, RoleTenantViewer:
		return true
	}
	return false
}
