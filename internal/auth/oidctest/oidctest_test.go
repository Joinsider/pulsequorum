// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package oidctest

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type discoveryDoc struct {
	Issuer                string   `json:"issuer"`
	JWKSURI               string   `json:"jwks_uri"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	EndSessionEndpoint    string   `json:"end_session_endpoint"`
	SigningAlgs           []string `json:"id_token_signing_alg_values_supported"`
}

type jwkDoc struct {
	Keys []jwkKey `json:"keys"`
}

type jwkKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Crv string `json:"crv"`
	N   string `json:"n"`
	E   string `json:"e"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

type parsedToken struct {
	header map[string]any
	claims map[string]any
	input  []byte // header.payload, the JWS signing input
	sig    []byte
}

// reply is the status, headers and body of an HTTP response.
type reply struct {
	status int
	header http.Header
	body   []byte
}

// fetch performs a GET on path and returns the reply.
func fetch(ctx context.Context, s *Server, path string) (reply, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Issuer()+path, nil)
	if err != nil {
		return reply{}, err
	}
	resp, err := s.Client().Do(req)
	if err != nil {
		return reply{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return reply{status: resp.StatusCode, header: resp.Header, body: body}, err
}

// mustFetch performs a GET on path and fails the test on transport errors.
func mustFetch(t *testing.T, s *Server, path string) reply {
	t.Helper()
	r, err := fetch(t.Context(), s, path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return r
}

func fetchJWKS(t *testing.T, s *Server) jwkDoc {
	t.Helper()
	var doc jwkDoc
	if err := json.Unmarshal(mustFetch(t, s, jwksPath).body, &doc); err != nil {
		t.Fatalf("decode JWKS: %v", err)
	}
	return doc
}

func findJWK(doc jwkDoc, kid string) (jwkKey, bool) {
	for _, j := range doc.Keys {
		if j.Kid == kid {
			return j, true
		}
	}
	return jwkKey{}, false
}

// mustJWK returns the published JWK for kid or fails the test.
func mustJWK(t *testing.T, s *Server, kid string) jwkKey {
	t.Helper()
	j, ok := findJWK(fetchJWKS(t, s), kid)
	if !ok {
		t.Fatalf("kid %q not published in JWKS", kid)
	}
	return j
}

func decodeB64(t *testing.T, v string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		t.Fatalf("value %q is not unpadded base64url: %v", v, err)
	}
	return b
}

// parseJWK converts a JWK back into a stdlib public key.
func parseJWK(t *testing.T, j jwkKey) any {
	t.Helper()
	switch j.Kty {
	case "RSA":
		e := new(big.Int).SetBytes(decodeB64(t, j.E))
		if !e.IsInt64() {
			t.Fatalf("exponent does not fit in int64")
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(decodeB64(t, j.N)), E: int(e.Int64())}
	case "EC":
		if j.Crv != "P-256" {
			t.Fatalf("unexpected curve %q", j.Crv)
		}
		x, y := decodeB64(t, j.X), decodeB64(t, j.Y)
		if len(x) != 32 || len(y) != 32 {
			t.Fatalf("EC coordinates are %d and %d bytes, want 32", len(x), len(y))
		}
		// Parsing also checks that the point lies on the curve.
		point := append(append([]byte{0x04}, x...), y...)
		pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), point)
		if err != nil {
			t.Fatalf("EC point is invalid: %v", err)
		}
		return pub
	case "OKP":
		if j.Crv != "Ed25519" {
			t.Fatalf("unexpected curve %q", j.Crv)
		}
		x := decodeB64(t, j.X)
		if len(x) != ed25519.PublicKeySize {
			t.Fatalf("Ed25519 key is %d bytes, want %d", len(x), ed25519.PublicKeySize)
		}
		return ed25519.PublicKey(x)
	}
	t.Fatalf("unexpected kty %q", j.Kty)
	return nil
}

func parseToken(t *testing.T, raw string) parsedToken {
	t.Helper()
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}
	tok := parsedToken{
		input: []byte(parts[0] + "." + parts[1]),
		sig:   decodeB64(t, parts[2]),
	}
	if err := json.Unmarshal(decodeB64(t, parts[0]), &tok.header); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if err := json.Unmarshal(decodeB64(t, parts[1]), &tok.claims); err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	return tok
}

// verifySignature checks a signature with the stdlib. ES256 signatures are
// raw R||S as in RFC 7518.
func verifySignature(t *testing.T, alg string, pub any, input, sig []byte) bool {
	t.Helper()
	switch alg {
	case "RS256":
		digest := sha256.Sum256(input)
		return rsa.VerifyPKCS1v15(pub.(*rsa.PublicKey), crypto.SHA256, digest[:], sig) == nil
	case "ES256":
		if len(sig) != 64 {
			return false
		}
		digest := sha256.Sum256(input)
		r := new(big.Int).SetBytes(sig[:32])
		s := new(big.Int).SetBytes(sig[32:])
		return ecdsa.Verify(pub.(*ecdsa.PublicKey), digest[:], r, s)
	case "EdDSA":
		return ed25519.Verify(pub.(ed25519.PublicKey), input, sig)
	}
	t.Fatalf("unsupported alg %q", alg)
	return false
}

// signedKid returns the kid in the header of a token signed with the active key.
func signedKid(t *testing.T, s *Server) string {
	t.Helper()
	tok := parseToken(t, s.Sign(s.Claims("user-1", time.Minute, time.Now())))
	kid, ok := tok.header["kid"].(string)
	if !ok {
		t.Fatalf("header kid is %v, want string", tok.header["kid"])
	}
	return kid
}

func TestDiscovery(t *testing.T) {
	tests := []struct {
		name     string
		override string
	}{
		{name: "default issuer"},
		{name: "overridden issuer", override: "https://evil.example.com/realm"},
		{name: "overridden issuer with trailing slash", override: "https://idp.example.com/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := New(t)
			if !strings.HasPrefix(s.Issuer(), "https://") || strings.HasSuffix(s.Issuer(), "/") {
				t.Fatalf("Issuer() = %q, want https base URL without trailing slash", s.Issuer())
			}
			wantIssuer := s.Issuer()
			if tc.override != "" {
				s.SetIssuerOverride(tc.override)
				wantIssuer = tc.override
			}

			var doc discoveryDoc
			if err := json.Unmarshal(mustFetch(t, s, discoveryPath).body, &doc); err != nil {
				t.Fatalf("decode discovery: %v", err)
			}
			if doc.Issuer != wantIssuer {
				t.Errorf("issuer = %q, want %q", doc.Issuer, wantIssuer)
			}
			if doc.JWKSURI != s.Issuer()+jwksPath {
				t.Errorf("jwks_uri = %q, want %q", doc.JWKSURI, s.Issuer()+jwksPath)
			}
			if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" || doc.EndSessionEndpoint == "" {
				t.Errorf("missing endpoints: %+v", doc)
			}
			for _, alg := range []string{"RS256", "ES256", "EdDSA"} {
				if !slices.Contains(doc.SigningAlgs, alg) {
					t.Errorf("id_token_signing_alg_values_supported %v lacks %q", doc.SigningAlgs, alg)
				}
			}
		})
	}
}

func TestJWKSPublishing(t *testing.T) {
	s := New(t)
	initial := fetchJWKS(t, s).Keys[0]
	kidES := s.AddKey("ES256")
	kidEd := s.AddKey("EdDSA")

	if doc := fetchJWKS(t, s); len(doc.Keys) != 3 {
		t.Fatalf("JWKS has %d keys, want 3", len(doc.Keys))
	}

	s.Unpublish(initial.Kid)
	doc := fetchJWKS(t, s)
	if _, ok := findJWK(doc, initial.Kid); ok {
		t.Error("unpublished key is still in JWKS")
	}
	for _, kid := range []string{kidES, kidEd} {
		if _, ok := findJWK(doc, kid); !ok {
			t.Errorf("published key %q missing from JWKS", kid)
		}
	}
	if len(doc.Keys) != 2 {
		t.Errorf("JWKS has %d keys after unpublish, want 2", len(doc.Keys))
	}

	// An unpublished key still signs. Its JWK was captured before unpublishing.
	tok := parseToken(t, s.SignWith(initial.Kid, nil, s.Claims("user-1", time.Minute, time.Now())))
	if tok.header["kid"] != initial.Kid {
		t.Errorf("header kid = %v, want %q", tok.header["kid"], initial.Kid)
	}
	if !verifySignature(t, "RS256", parseJWK(t, initial), tok.input, tok.sig) {
		t.Error("signature of unpublished key does not verify with its JWK")
	}
}

func TestJWKSHeadersAndFields(t *testing.T) {
	s := New(t)
	kid := s.AddKey("ES256")
	j := mustJWK(t, s, kid)
	if j.Kty != "EC" || j.Crv != "P-256" || j.Use != "sig" || j.Alg != "ES256" {
		t.Errorf("unexpected JWK fields: %+v", j)
	}

	tests := []struct {
		name    string
		maxAge  time.Duration
		noAlg   bool
		wantCC  string
		wantAlg bool
	}{
		{name: "default max-age", wantCC: "max-age=300", wantAlg: true},
		{name: "custom max-age", maxAge: 42 * time.Second, wantCC: "max-age=42", wantAlg: true},
		{name: "without alg member", wantCC: "max-age=300", noAlg: true, wantAlg: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.maxAge != 0 {
				s.SetJWKSMaxAge(tc.maxAge)
				t.Cleanup(func() { s.SetJWKSMaxAge(defaultJWKSMaxAge) })
			}
			if tc.noAlg {
				s.SetPublishAlg(false)
				t.Cleanup(func() { s.SetPublishAlg(true) })
			}
			r := mustFetch(t, s, jwksPath)
			if r.status != http.StatusOK {
				t.Fatalf("status = %d, want 200", r.status)
			}
			if got := r.header.Get("Cache-Control"); got != tc.wantCC {
				t.Errorf("Cache-Control = %q, want %q", got, tc.wantCC)
			}
			if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			var raw struct {
				Keys []map[string]any `json:"keys"`
			}
			if err := json.Unmarshal(r.body, &raw); err != nil {
				t.Fatalf("decode JWKS: %v", err)
			}
			for _, k := range raw.Keys {
				_, hasAlg := k["alg"]
				if hasAlg != tc.wantAlg {
					t.Errorf("key %v: alg present = %v, want %v", k["kid"], hasAlg, tc.wantAlg)
				}
			}
		})
	}
}

func TestSignatureVerifiesWithPublishedJWK(t *testing.T) {
	for _, alg := range []string{"RS256", "ES256", "EdDSA"} {
		t.Run(alg, func(t *testing.T) {
			s := New(t)
			kid := s.AddKey(alg)
			tok := parseToken(t, s.Sign(s.Claims("user-1", time.Minute, time.Now())))

			if tok.header["alg"] != alg || tok.header["kid"] != kid || tok.header["typ"] != "JWT" {
				t.Fatalf("header = %v, want alg %q, kid %q, typ JWT", tok.header, alg, kid)
			}
			j := mustJWK(t, s, kid)
			if j.Alg != alg {
				t.Errorf("JWK alg = %q, want %q", j.Alg, alg)
			}
			pub := parseJWK(t, j)
			if !verifySignature(t, alg, pub, tok.input, tok.sig) {
				t.Fatal("signature does not verify with the published JWK")
			}
			tampered := append(append([]byte{}, tok.input...), 'x')
			if verifySignature(t, alg, pub, tampered, tok.sig) {
				t.Error("signature verifies over modified input")
			}
		})
	}
}

func TestAddRSAKeyWeak(t *testing.T) {
	s := New(t)
	kid := s.AddRSAKey(1024)
	j := mustJWK(t, s, kid)
	if j.Kty != "RSA" || j.Alg != "RS256" {
		t.Fatalf("unexpected JWK: %+v", j)
	}
	pub, ok := parseJWK(t, j).(*rsa.PublicKey)
	if !ok {
		t.Fatal("RSA JWK did not parse to *rsa.PublicKey")
	}
	if pub.N.BitLen() != 1024 {
		t.Errorf("modulus has %d bits, want 1024", pub.N.BitLen())
	}
	if got := signedKid(t, s); got != kid {
		t.Errorf("active kid = %q, want %q", got, kid)
	}
	tok := parseToken(t, s.Sign(s.Claims("user-1", time.Minute, time.Now())))
	if !verifySignature(t, "RS256", pub, tok.input, tok.sig) {
		t.Error("signature with weak key does not verify")
	}
}

func TestSetActive(t *testing.T) {
	s := New(t)
	first := fetchJWKS(t, s).Keys[0].Kid
	second := s.AddKey("ES256")
	if got := signedKid(t, s); got != second {
		t.Errorf("after AddKey, active kid = %q, want %q", got, second)
	}
	s.SetActive(first)
	if got := signedKid(t, s); got != first {
		t.Errorf("after SetActive, active kid = %q, want %q", got, first)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("SetActive with unknown kid did not panic")
			}
		}()
		s.SetActive("missing")
	}()
}

func TestSignWithHeaders(t *testing.T) {
	s := New(t)
	other := s.AddKey("EdDSA")
	kid := s.AddKey("ES256") // active key

	tests := []struct {
		name   string
		kid    string         // passed to SignWith
		header map[string]any // passed to SignWith
		signer string         // kid of the key that must have made the signature
		want   map[string]any // expected header; nil value means absent
	}{
		{
			name:   "defaults",
			signer: kid,
			want:   map[string]any{"alg": "ES256", "kid": kid, "typ": "JWT"},
		},
		{
			name:   "explicit kid selects key",
			kid:    other,
			signer: other,
			want:   map[string]any{"alg": "EdDSA", "kid": other, "typ": "JWT"},
		},
		{
			name:   "declared alg differs from key",
			header: map[string]any{"alg": "RS256"},
			signer: kid,
			want:   map[string]any{"alg": "RS256", "kid": kid, "typ": "JWT"},
		},
		{
			name:   "typ override",
			header: map[string]any{"typ": "at+jwt"},
			signer: kid,
			want:   map[string]any{"alg": "ES256", "kid": kid, "typ": "at+jwt"},
		},
		{
			name:   "nil removes kid",
			header: map[string]any{"kid": nil},
			signer: kid,
			want:   map[string]any{"alg": "ES256", "kid": nil, "typ": "JWT"},
		},
		{
			name:   "extra header field",
			header: map[string]any{"x5t": "abc"},
			signer: kid,
			want:   map[string]any{"alg": "ES256", "kid": kid, "typ": "JWT", "x5t": "abc"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tok := parseToken(t, s.SignWith(tc.kid, tc.header, s.Claims("user-1", time.Minute, time.Now())))
			for name, want := range tc.want {
				got, present := tok.header[name]
				if want == nil {
					if present {
						t.Errorf("header[%q] = %v, want absent", name, got)
					}
					continue
				}
				if !present || got != want {
					t.Errorf("header[%q] = %v, want %v", name, got, want)
				}
			}
			// The signature is made with the signer's own algorithm,
			// whatever the header declares.
			j := mustJWK(t, s, tc.signer)
			if !verifySignature(t, j.Alg, parseJWK(t, j), tok.input, tok.sig) {
				t.Error("signature does not verify with the signing key's JWK")
			}
		})
	}
}

func TestSignNone(t *testing.T) {
	s := New(t)
	kid := s.AddKey("ES256")
	raw := s.SignNone(s.Claims("user-1", time.Minute, time.Now()))

	parts := strings.Split(raw, ".")
	if len(parts) != 3 || parts[2] != "" {
		t.Fatalf("unsigned token = %q, want header.payload.(empty)", raw)
	}
	tok := parseToken(t, raw)
	if tok.header["alg"] != "none" {
		t.Errorf("alg = %v, want none", tok.header["alg"])
	}
	if tok.header["kid"] != kid {
		t.Errorf("kid = %v, want %q", tok.header["kid"], kid)
	}
	if tok.claims["sub"] != "user-1" {
		t.Errorf("sub = %v, want user-1", tok.claims["sub"])
	}
}

func TestSignHS256WithPublicKey(t *testing.T) {
	for _, alg := range []string{"RS256", "ES256", "EdDSA"} {
		t.Run(alg, func(t *testing.T) {
			s := New(t)
			kid := s.AddKey(alg)
			der, err := x509.MarshalPKIXPublicKey(parseJWK(t, mustJWK(t, s, kid)))
			if err != nil {
				t.Fatalf("marshal public key: %v", err)
			}

			tok := parseToken(t, s.SignHS256WithPublicKey(kid, s.Claims("user-1", time.Minute, time.Now())))
			if tok.header["alg"] != "HS256" || tok.header["kid"] != kid {
				t.Fatalf("header = %v, want alg HS256, kid %q", tok.header, kid)
			}
			mac := hmac.New(sha256.New, der)
			_, _ = mac.Write(tok.input)
			if !hmac.Equal(mac.Sum(nil), tok.sig) {
				t.Error("HMAC does not verify with the DER public key as secret")
			}
		})
	}
}

func TestClaims(t *testing.T) {
	s := New(t)
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	ttl := 10 * time.Minute

	c := s.Claims("user-1", ttl, now)
	if c["iss"] != s.Issuer() {
		t.Errorf("iss = %v, want %q", c["iss"], s.Issuer())
	}
	if c["sub"] != "user-1" || c["azp"] != "spa-client" {
		t.Errorf("sub/azp = %v/%v", c["sub"], c["azp"])
	}
	aud, _ := c["aud"].([]string)
	if !slices.Equal(aud, []string{"project-1", "spa-client"}) {
		t.Errorf("aud = %v", c["aud"])
	}
	if c["iat"] != now.Unix() || c["nbf"] != now.Unix() {
		t.Errorf("iat/nbf = %v/%v, want %d", c["iat"], c["nbf"], now.Unix())
	}
	if c["exp"] != now.Add(ttl).Unix() {
		t.Errorf("exp = %v, want %d", c["exp"], now.Add(ttl).Unix())
	}
	jti, _ := c["jti"].(string)
	if jti == "" {
		t.Error("jti is empty")
	}
	if other, _ := s.Claims("user-1", ttl, now)["jti"].(string); other == jti {
		t.Error("jti is not random across calls")
	}
}

func TestSetDown(t *testing.T) {
	for _, path := range []string{discoveryPath, jwksPath} {
		t.Run(path, func(t *testing.T) {
			s := New(t)
			s.SetDown(true)
			if _, err := fetch(t.Context(), s, path); err == nil {
				t.Error("request succeeded while down")
			}
			s.SetDown(false)
			if got := mustFetch(t, s, path).status; got != http.StatusOK {
				t.Errorf("status after SetDown(false) = %d, want 200", got)
			}
		})
	}
}

func TestSetStatus(t *testing.T) {
	for _, path := range []string{discoveryPath, jwksPath} {
		for _, code := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable} {
			t.Run(path+" "+http.StatusText(code), func(t *testing.T) {
				s := New(t)
				s.SetStatus(code)
				if got := mustFetch(t, s, path).status; got != code {
					t.Errorf("status = %d, want %d", got, code)
				}
				s.SetStatus(0)
				if got := mustFetch(t, s, path).status; got != http.StatusOK {
					t.Errorf("status after SetStatus(0) = %d, want 200", got)
				}
			})
		}
	}
}

func TestSetDelay(t *testing.T) {
	const delay = 100 * time.Millisecond
	for _, path := range []string{discoveryPath, jwksPath} {
		t.Run(path, func(t *testing.T) {
			s := New(t)
			s.SetDelay(delay)
			start := time.Now()
			mustFetch(t, s, path)
			if elapsed := time.Since(start); elapsed < delay {
				t.Errorf("response after %v, want at least %v", elapsed, delay)
			}
		})
	}
}

func TestRequestCounters(t *testing.T) {
	s := New(t)
	if s.DiscoveryRequests() != 0 || s.JWKSRequests() != 0 {
		t.Fatalf("counters start at %d/%d, want 0/0", s.DiscoveryRequests(), s.JWKSRequests())
	}
	for range 3 {
		mustFetch(t, s, discoveryPath)
	}
	for range 2 {
		mustFetch(t, s, jwksPath)
	}
	if got := s.DiscoveryRequests(); got != 3 {
		t.Errorf("DiscoveryRequests = %d, want 3", got)
	}
	if got := s.JWKSRequests(); got != 2 {
		t.Errorf("JWKSRequests = %d, want 2", got)
	}

	s.SetStatus(http.StatusInternalServerError)
	mustFetch(t, s, discoveryPath)
	s.SetStatus(0)
	if got := s.DiscoveryRequests(); got != 4 {
		t.Errorf("DiscoveryRequests after injected status = %d, want 4", got)
	}

	mustFetch(t, s, "/unknown")
	if s.DiscoveryRequests() != 4 || s.JWKSRequests() != 2 {
		t.Errorf("unknown path changed counters to %d/%d", s.DiscoveryRequests(), s.JWKSRequests())
	}
}

func TestConcurrentSignAndAddKey(t *testing.T) {
	s := New(t)
	const workers = 4
	const iterations = 20

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range iterations {
				tok := s.Sign(s.Claims("user-1", time.Minute, time.Now()))
				if strings.Count(tok, ".") != 2 {
					t.Errorf("malformed token %q", tok)
				}
			}
		})
		wg.Go(func() {
			for range iterations {
				s.SetActive(s.AddKey("EdDSA"))
			}
		})
		wg.Go(func() {
			for range iterations {
				r, err := fetch(t.Context(), s, jwksPath)
				if err != nil {
					t.Errorf("fetch JWKS: %v", err)
					continue
				}
				if r.status != http.StatusOK {
					t.Errorf("JWKS status = %d", r.status)
				}
			}
		})
		wg.Go(func() {
			for i := range iterations {
				s.SetPublishAlg(i%2 == 0)
				s.SetJWKSMaxAge(time.Duration(i) * time.Second)
				s.SetDelay(0)
				s.SetStatus(0)
				s.SetIssuerOverride("")
			}
		})
	}
	wg.Wait()

	if got := s.JWKSRequests(); got != workers*iterations {
		t.Errorf("JWKSRequests = %d, want %d", got, workers*iterations)
	}
}
