// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package auth

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type testKey struct {
	kid  string
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newTestKey(t *testing.T, kid string) testKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return testKey{kid: kid, pub: pub, priv: priv}
}

// lookup knows only the key of k.
func (k testKey) lookup(kid string) (ed25519.PublicKey, bool) {
	if kid != k.kid {
		return nil, false
	}
	return k.pub, true
}

func enc(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// craftRaw signs header and payload exactly as given, so tests can produce
// tokens that a correct signer would never produce.
func craftRaw(priv ed25519.PrivateKey, hdr, pay []byte) string {
	in := enc(hdr) + "." + enc(pay)
	return in + "." + enc(ed25519.Sign(priv, []byte(in)))
}

func craft(t *testing.T, priv ed25519.PrivateKey, hdr, pay map[string]any) string {
	t.Helper()
	h, err := json.Marshal(hdr)
	if err != nil {
		t.Fatal(err)
	}
	p, err := json.Marshal(pay)
	if err != nil {
		t.Fatal(err)
	}
	return craftRaw(priv, h, p)
}

func baseHeader(kid string) map[string]any {
	return map[string]any{"alg": "EdDSA", "typ": "JWT", "kid": kid}
}

func basePayload(now time.Time) map[string]any {
	n := now.Unix()
	return map[string]any{
		"iss": Issuer, "aud": Audience, "sub": "user-1", "tid": "default",
		"roles": []string{"tenant-viewer"}, "iat": n, "nbf": n, "exp": n + 300, "jti": "j1",
	}
}

func validClaims(now time.Time) Claims {
	n := now.Unix()
	return Claims{Issuer: Issuer, Audience: Audience, Subject: "user-1", TenantID: "default",
		Roles: []string{"tenant-editor"}, IssuedAt: n, NotBefore: n, ExpiresAt: n + 600, ID: "j2"}
}

func TestVerifyAccessTokenAccepts(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	k := newTestKey(t, "kid-1")
	c := validClaims(now)
	tok, err := SignAccessToken(k.kid, k.priv, c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := VerifyAccessToken(tok, k.lookup, now)
	if err != nil {
		t.Fatalf("VerifyAccessToken: %v", err)
	}
	if got.Subject != c.Subject || got.TenantID != c.TenantID || got.ID != c.ID ||
		len(got.Roles) != 1 || got.Roles[0] != "tenant-editor" || got.ExpiresAt != c.ExpiresAt {
		t.Errorf("claims = %+v, want %+v", got, c)
	}
}

func TestVerifyAccessTokenLeeway(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	k := newTestKey(t, "kid-1")
	tests := []struct {
		name string
		edit func(map[string]any)
	}{
		{"expired 20 s ago", func(p map[string]any) { p["iat"] = now.Unix() - 60; p["exp"] = now.Unix() - 20 }},
		{"not before 20 s ahead", func(p map[string]any) { p["nbf"] = now.Unix() + 20 }},
		{"issued 20 s ahead", func(p map[string]any) { p["iat"] = now.Unix() + 20 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := basePayload(now)
			tt.edit(p)
			if _, err := VerifyAccessToken(craft(t, k.priv, baseHeader(k.kid), p), k.lookup, now); err != nil {
				t.Errorf("token within leeway rejected: %v", err)
			}
		})
	}
}

func TestVerifyAccessTokenRejects(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	k := newTestKey(t, "kid-1")
	other := newTestKey(t, "kid-1") // same kid, different key
	n := now.Unix()

	// withPayload signs a valid header with a payload edited by mut.
	withPayload := func(mut func(map[string]any)) func(testKey, time.Time) string {
		return func(k testKey, now time.Time) string {
			p := basePayload(now)
			mut(p)
			return craft(t, k.priv, baseHeader(k.kid), p)
		}
	}
	withHeader := func(mut func(map[string]any)) func(testKey, time.Time) string {
		return func(k testKey, now time.Time) string {
			h := baseHeader(k.kid)
			mut(h)
			return craft(t, k.priv, h, basePayload(now))
		}
	}
	valid := func(k testKey, now time.Time) string {
		return craft(t, k.priv, baseHeader(k.kid), basePayload(now))
	}

	tests := []struct {
		name  string
		build func(k testKey, now time.Time) string
	}{
		{"empty token", func(testKey, time.Time) string { return "" }},
		{"wrong signature (other key with same kid)", func(_ testKey, now time.Time) string {
			return craft(t, other.priv, baseHeader(k.kid), basePayload(now))
		}},
		{"payload changed after signing", func(k testKey, now time.Time) string {
			tok := valid(k, now)
			parts := strings.Split(tok, ".")
			p := basePayload(now)
			p["sub"] = "admin"
			raw, _ := json.Marshal(p)
			parts[1] = enc(raw)
			return strings.Join(parts, ".")
		}},
		{"unknown kid", func(k testKey, now time.Time) string {
			return craft(t, k.priv, baseHeader("unknown-kid"), basePayload(now))
		}},
		{"alg none", func(k testKey, now time.Time) string {
			h, _ := json.Marshal(map[string]any{"alg": "none", "typ": "JWT", "kid": k.kid})
			p, _ := json.Marshal(basePayload(now))
			return enc(h) + "." + enc(p) + "."
		}},
		{"alg HS256 with a signature", func(k testKey, now time.Time) string {
			h, _ := json.Marshal(map[string]any{"alg": "HS256", "typ": "JWT", "kid": k.kid})
			p, _ := json.Marshal(basePayload(now))
			return enc(h) + "." + enc(p) + "." + enc(make([]byte, 32))
		}},
		{"alg in lower case", withHeader(func(h map[string]any) { h["alg"] = "eddsa" })},
		{"typ not JWT", withHeader(func(h map[string]any) { h["typ"] = "at+jwt" })},
		{"missing typ", withHeader(func(h map[string]any) { delete(h, "typ") })},
		{"missing kid", withHeader(func(h map[string]any) { delete(h, "kid") })},
		{"unknown header field", withHeader(func(h map[string]any) { h["x5c"] = []string{"abc"} })},
		{"unknown claim field", withPayload(func(p map[string]any) { p["scope"] = "all" })},
		{"trailing data after payload", func(k testKey, now time.Time) string {
			p, _ := json.Marshal(basePayload(now))
			h, _ := json.Marshal(baseHeader(k.kid))
			return craftRaw(k.priv, h, append(p, []byte(" x")...))
		}},
		{"stray brace after payload", func(k testKey, now time.Time) string {
			p, _ := json.Marshal(basePayload(now))
			h, _ := json.Marshal(baseHeader(k.kid))
			return craftRaw(k.priv, h, append(p, '}'))
		}},
		{"stray bracket after payload", func(k testKey, now time.Time) string {
			p, _ := json.Marshal(basePayload(now))
			h, _ := json.Marshal(baseHeader(k.kid))
			return craftRaw(k.priv, h, append(p, ']'))
		}},
		{"stray brace after header", func(k testKey, now time.Time) string {
			p, _ := json.Marshal(basePayload(now))
			h, _ := json.Marshal(baseHeader(k.kid))
			return craftRaw(k.priv, append(h, '}'), p)
		}},
		{"expired beyond leeway", withPayload(func(p map[string]any) { p["iat"] = n - 60; p["exp"] = n - 31 })},
		{"not valid yet", withPayload(func(p map[string]any) { p["nbf"] = n + 31 })},
		{"issued in the future", withPayload(func(p map[string]any) { p["iat"] = n + 31; p["exp"] = n + 331 })},
		{"lifetime longer than 15 min", withPayload(func(p map[string]any) { p["exp"] = n + 15*60 + 1 })},
		{"wrong issuer", withPayload(func(p map[string]any) { p["iss"] = "someone-else" })},
		{"wrong audience", withPayload(func(p map[string]any) { p["aud"] = "other-service" })},
		{"empty subject", withPayload(func(p map[string]any) { p["sub"] = "" })},
		{"missing subject", withPayload(func(p map[string]any) { delete(p, "sub") })},
		{"empty tenant", withPayload(func(p map[string]any) { p["tid"] = "" })},
		{"empty roles", withPayload(func(p map[string]any) { p["roles"] = []string{} })},
		{"missing roles", withPayload(func(p map[string]any) { delete(p, "roles") })},
		{"unknown role", withPayload(func(p map[string]any) { p["roles"] = []string{"root"} })},
		{"one unknown role among valid ones", withPayload(func(p map[string]any) { p["roles"] = []string{"tenant-viewer", "root"} })},
		{"two segments", func(k testKey, now time.Time) string {
			return strings.Join(strings.Split(valid(k, now), ".")[:2], ".")
		}},
		{"four segments", func(k testKey, now time.Time) string { return valid(k, now) + ".AAAA" }},
		{"header not base64", func(k testKey, now time.Time) string {
			return "!!!." + strings.Split(valid(k, now), ".")[1] + "." + strings.Split(valid(k, now), ".")[2]
		}},
		{"signature not base64", func(k testKey, now time.Time) string {
			return strings.Split(valid(k, now), ".")[0] + "." + strings.Split(valid(k, now), ".")[1] + ".!!!"
		}},
		{"header is not JSON", func(k testKey, now time.Time) string {
			p, _ := json.Marshal(basePayload(now))
			return craftRaw(k.priv, []byte("not json"), p)
		}},
		{"payload is not JSON", func(k testKey, _ time.Time) string {
			h, _ := json.Marshal(baseHeader(k.kid))
			return craftRaw(k.priv, h, []byte("{"))
		}},
		{"token longer than 4096 bytes", withPayload(func(p map[string]any) { p["jti"] = strings.Repeat("a", 4096) })},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tok := tt.build(k, now)
			_, err := VerifyAccessToken(tok, k.lookup, now)
			if !errors.Is(err, ErrInvalidToken) {
				t.Errorf("VerifyAccessToken = %v, want ErrInvalidToken", err)
			}
		})
	}
}

func TestSignAccessTokenRejectsBadKey(t *testing.T) {
	k := newTestKey(t, "kid-1")
	if _, err := SignAccessToken("", k.priv, validClaims(time.Now())); err == nil {
		t.Error("empty kid accepted")
	}
	if _, err := SignAccessToken(k.kid, k.priv[:10], validClaims(time.Now())); err == nil {
		t.Error("short private key accepted")
	}
}

// ADR 0020: a token issued at T is accepted by a node whose clock is up to
// 30 s off (clockLeeway) and rejected beyond that, both at the start of its
// validity (iat, nbf) and at its end (exp).
func TestAccessTokenClockTolerance(t *testing.T) {
	issued := time.Unix(1_800_000_000, 0)
	k := newTestKey(t, "kid-1")
	c := Claims{Issuer: Issuer, Audience: Audience, Subject: "user-1", TenantID: "default",
		Roles: []string{"tenant-editor"}, IssuedAt: issued.Unix(), NotBefore: issued.Unix(),
		ExpiresAt: issued.Add(AccessTTL).Unix(), ID: "clock"}
	tok, err := SignAccessToken(k.kid, k.priv, c)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		offset time.Duration // verifier clock minus issuer clock
		ok     bool
	}{
		{"verifier 29 s behind at issuance", -29 * time.Second, true},
		{"verifier 31 s behind at issuance", -31 * time.Second, false},
		{"verifier 29 s ahead at expiry", AccessTTL + 29*time.Second, true},
		{"verifier 31 s ahead at expiry", AccessTTL + 31*time.Second, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The start-of-validity cases verify at issuance time plus offset;
			// the end cases at issuance plus AccessTTL plus the offset.
			at := issued.Add(tt.offset)
			_, err := VerifyAccessToken(tok, k.lookup, at)
			if (err == nil) != tt.ok {
				t.Errorf("verify at %v: err = %v, want accepted = %v", at.Unix()-issued.Unix(), err, tt.ok)
			}
		})
	}
}
