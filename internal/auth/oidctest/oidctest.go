// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package oidctest provides a fake OpenID Connect identity provider for tests.
//
// A Server serves OIDC discovery and a JWKS endpoint over TLS and signs
// arbitrary claims with RSA, ECDSA P-256 and Ed25519 keys. Tests can rotate
// keys, inject failures, delay responses and count requests, so the token
// verifier can be exercised without a real IdP. The package depends on the
// standard library only.
package oidctest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	discoveryPath     = "/.well-known/openid-configuration"
	jwksPath          = "/oauth/v2/keys"
	defaultJWKSMaxAge = 300 * time.Second
	defaultRSABits    = 2048
)

// supportedAlgs is advertised in the discovery document.
var supportedAlgs = []string{"RS256", "ES256", "EdDSA"}

// key is one signing key. priv is a *rsa.PrivateKey, *ecdsa.PrivateKey or
// ed25519.PrivateKey; pub is the matching public key. The fields alg, kid,
// priv and pub are immutable after creation; published is guarded by Server.mu.
type key struct {
	kid       string
	alg       string
	priv      any
	pub       any
	published bool
}

// Server is a fake OpenID Connect identity provider. All methods are safe for
// concurrent use.
type Server struct {
	srv *httptest.Server

	discoveryRequests atomic.Int64
	jwksRequests      atomic.Int64

	mu             sync.Mutex
	keys           map[string]*key
	order          []string // kids in creation order
	active         *key
	issuerOverride string
	jwksMaxAge     time.Duration
	publishAlg     bool
	down           bool
	status         int
	delay          time.Duration
}

// New starts a fake IdP for the duration of the test. It registers Close with
// t.Cleanup and creates one active RS256 key with 2048 bits.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{
		keys:       make(map[string]*key),
		jwksMaxAge: defaultJWKSMaxAge,
		publishAlg: true,
	}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	s.AddKey("RS256")
	return s
}

// Close shuts the server down. New registers it with t.Cleanup, so calling it
// is only needed to stop the server early.
func (s *Server) Close() {
	s.srv.Close()
}

// Issuer returns the server's base URL (https, without trailing slash).
func (s *Server) Issuer() string {
	return s.srv.URL
}

// Client returns an HTTP client that trusts the server's TLS certificate.
func (s *Server) Client() *http.Client {
	return s.srv.Client()
}

// SetIssuerOverride makes discovery report iss as its issuer instead of
// Issuer(), to test the exact-issuer check. An empty string restores the
// default. The jwks_uri and the other endpoints keep pointing at the real
// server.
func (s *Server) SetIssuerOverride(iss string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issuerOverride = iss
}

// SetJWKSMaxAge sets the max-age of the Cache-Control header on JWKS
// responses. The default is 300 seconds; the value is truncated to whole
// seconds.
func (s *Server) SetJWKSMaxAge(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jwksMaxAge = d
}

// SetPublishAlg controls whether published JWKs carry the alg member. The
// default is true.
func (s *Server) SetPublishAlg(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publishAlg = on
}

// SetDown makes every endpoint close the connection without a response, as if
// the IdP were unreachable.
func (s *Server) SetDown(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = down
}

// SetStatus makes the discovery and JWKS endpoints answer with code. Zero
// restores normal responses.
func (s *Server) SetStatus(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = code
}

// SetDelay delays every response by d. Zero disables the delay.
func (s *Server) SetDelay(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delay = d
}

// DiscoveryRequests returns the number of requests received at the discovery
// endpoint. Requests are counted when they arrive, including those answered
// with an injected status or dropped by SetDown.
func (s *Server) DiscoveryRequests() int {
	return int(s.discoveryRequests.Load())
}

// JWKSRequests returns the number of requests received at the JWKS endpoint.
// Requests are counted when they arrive, including those answered with an
// injected status or dropped by SetDown.
func (s *Server) JWKSRequests() int {
	return int(s.jwksRequests.Load())
}

// AddKey generates a new signing key and publishes it. The algorithm is RS256
// (RSA with 2048 bits), ES256 (NIST P-256) or EdDSA (Ed25519). The new key
// becomes the active signing key. AddKey returns the key ID. It panics on an
// unsupported algorithm.
func (s *Server) AddKey(alg string) string {
	return s.register(newKey(alg, defaultRSABits))
}

// AddRSAKey generates an RS256 key with the given modulus size, for weak-key
// tests. The new key is published and becomes the active signing key. It
// returns the key ID.
func (s *Server) AddRSAKey(bits int) string {
	return s.register(newKey("RS256", bits))
}

// Unpublish removes the key from the JWKS. The key stays usable for signing
// through SignWith. It panics on an unknown kid.
func (s *Server) Unpublish(kid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookup(kid).published = false
}

// SetActive selects the key that Sign, SignNone and SignHS256WithPublicKey use
// by default. It panics on an unknown kid.
func (s *Server) SetActive(kid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active = s.lookup(kid)
}

// Sign signs claims with the active key and returns a compact JWS. The header
// is {"alg": <key alg>, "kid": <kid>, "typ": "JWT"}.
func (s *Server) Sign(claims map[string]any) string {
	return s.SignWith("", nil, claims)
}

// SignWith signs claims with the key identified by kid, or with the active key
// if kid is empty. The fields of header are merged over the default header;
// a nil value removes the field. The signature is always made with the key's
// own algorithm, so a header alg that differs from it yields a token whose
// declared algorithm does not match its signature. It panics on an unknown kid.
func (s *Server) SignWith(kid string, header, claims map[string]any) string {
	k := s.signingKey(kid)
	hdr := map[string]any{"alg": k.alg, "kid": k.kid, "typ": "JWT"}
	for name, v := range header {
		if v == nil {
			delete(hdr, name)
			continue
		}
		hdr[name] = v
	}
	return compact(hdr, claims, func(input []byte) []byte {
		return signInput(k, input)
	})
}

// SignNone returns an unsigned token with header {"alg": "none", "kid": <active
// kid>, "typ": "JWT"} and an empty signature. It is used for attack tests.
func (s *Server) SignNone(claims map[string]any) string {
	k := s.signingKey("")
	header := map[string]any{"alg": "none", "kid": k.kid, "typ": "JWT"}
	return compact(header, claims, func([]byte) []byte { return nil })
}

// SignHS256WithPublicKey returns a token with header {"alg": "HS256", "kid":
// kid, "typ": "JWT"}. The HMAC-SHA256 secret is the DER encoding of the public
// key of kid (x509.MarshalPKIXPublicKey), which is the classic algorithm
// confusion attack. An empty kid selects the active key. It panics on an
// unknown kid.
func (s *Server) SignHS256WithPublicKey(kid string, claims map[string]any) string {
	k := s.signingKey(kid)
	der, err := x509.MarshalPKIXPublicKey(k.pub)
	must(err)
	header := map[string]any{"alg": "HS256", "kid": k.kid, "typ": "JWT"}
	return compact(header, claims, func(input []byte) []byte {
		mac := hmac.New(sha256.New, der)
		_, _ = mac.Write(input)
		return mac.Sum(nil)
	})
}

// Claims returns a baseline claim set for subject sub, valid from now for ttl:
// iss is Issuer(), aud is ["project-1", "spa-client"], azp is "spa-client",
// iat and nbf are now, exp is now+ttl (all as Unix seconds), and jti is random.
// Tests may mutate the returned map.
func (s *Server) Claims(sub string, ttl time.Duration, now time.Time) map[string]any {
	return map[string]any{
		"iss": s.Issuer(),
		"sub": sub,
		"aud": []string{"project-1", "spa-client"},
		"azp": "spa-client",
		"iat": now.Unix(),
		"nbf": now.Unix(),
		"exp": now.Add(ttl).Unix(),
		"jti": randomHex(16),
	}
}

// serve handles all requests. Counting, delay and SetDown apply to every
// endpoint; SetStatus applies to discovery and JWKS only.
func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	var counter *atomic.Int64
	switch r.URL.Path {
	case discoveryPath:
		counter = &s.discoveryRequests
	case jwksPath:
		counter = &s.jwksRequests
	}
	if counter != nil {
		counter.Add(1)
	}

	s.mu.Lock()
	down, status, delay := s.down, s.status, s.delay
	s.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if down {
		abort(w)
		return
	}
	if counter == nil {
		http.NotFound(w, r)
		return
	}
	if status != 0 {
		http.Error(w, "injected by oidctest", status)
		return
	}
	if r.URL.Path == discoveryPath {
		s.writeDiscovery(w)
		return
	}
	s.writeJWKS(w)
}

func (s *Server) writeDiscovery(w http.ResponseWriter) {
	s.mu.Lock()
	issuer := s.issuerOverride
	s.mu.Unlock()
	if issuer == "" {
		issuer = s.Issuer()
	}
	writeJSON(w, map[string]any{
		"issuer":                                issuer,
		"jwks_uri":                              s.Issuer() + jwksPath,
		"authorization_endpoint":                s.Issuer() + "/oauth/v2/authorize",
		"token_endpoint":                        s.Issuer() + "/oauth/v2/token",
		"end_session_endpoint":                  s.Issuer() + "/oidc/v1/end_session",
		"id_token_signing_alg_values_supported": supportedAlgs,
	})
}

func (s *Server) writeJWKS(w http.ResponseWriter) {
	s.mu.Lock()
	maxAge, withAlg := s.jwksMaxAge, s.publishAlg
	keys := make([]map[string]any, 0, len(s.order))
	for _, kid := range s.order {
		if k := s.keys[kid]; k.published {
			keys = append(keys, publicJWK(k, withAlg))
		}
	}
	s.mu.Unlock()

	w.Header().Set("Cache-Control", fmt.Sprintf("max-age=%d", int(maxAge/time.Second)))
	writeJSON(w, map[string]any{"keys": keys})
}

// register stores k, publishes it and makes it the active key.
func (s *Server) register(k *key) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[k.kid] = k
	s.order = append(s.order, k.kid)
	s.active = k
	return k.kid
}

// lookup returns the key for kid. The caller must hold s.mu.
func (s *Server) lookup(kid string) *key {
	k, ok := s.keys[kid]
	if !ok {
		panic(fmt.Sprintf("oidctest: unknown kid %q", kid))
	}
	return k
}

// signingKey returns the key for kid, or the active key if kid is empty.
func (s *Server) signingKey(kid string) *key {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kid == "" {
		return s.active
	}
	return s.lookup(kid)
}

// newKey generates a key for alg. rsaBits applies to RS256 only.
func newKey(alg string, rsaBits int) *key {
	k := &key{alg: alg, published: true}
	switch alg {
	case "RS256":
		priv, err := rsa.GenerateKey(rand.Reader, rsaBits)
		must(err)
		k.priv, k.pub = priv, &priv.PublicKey
	case "ES256":
		priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		must(err)
		k.priv, k.pub = priv, &priv.PublicKey
	case "EdDSA":
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		must(err)
		k.priv, k.pub = priv, pub
	default:
		panic(fmt.Sprintf("oidctest: unsupported algorithm %q", alg))
	}
	k.kid = randomHex(16)
	return k
}

// publicJWK returns the public JWK of k (RFC 7517, RFC 7518, RFC 8037).
func publicJWK(k *key, withAlg bool) map[string]any {
	j := map[string]any{"kid": k.kid, "use": "sig"}
	if withAlg {
		j["alg"] = k.alg
	}
	switch pub := k.pub.(type) {
	case *rsa.PublicKey:
		j["kty"] = "RSA"
		j["n"] = b64(pub.N.Bytes())
		j["e"] = b64(big.NewInt(int64(pub.E)).Bytes())
	case *ecdsa.PublicKey:
		// SEC 1 uncompressed point: 0x04 || X || Y, 32 bytes each for P-256.
		point, err := pub.Bytes()
		must(err)
		j["kty"] = "EC"
		j["crv"] = "P-256"
		j["x"] = b64(point[1:33])
		j["y"] = b64(point[33:65])
	case ed25519.PublicKey:
		j["kty"] = "OKP"
		j["crv"] = "Ed25519"
		j["x"] = b64(pub)
	}
	return j
}

// signInput signs the JWS signing input with the native algorithm of k.
func signInput(k *key, input []byte) []byte {
	switch priv := k.priv.(type) {
	case *rsa.PrivateKey:
		digest := sha256.Sum256(input)
		sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
		must(err)
		return sig
	case *ecdsa.PrivateKey:
		digest := sha256.Sum256(input)
		r, s, err := ecdsa.Sign(rand.Reader, priv, digest[:])
		must(err)
		sig := make([]byte, 64)
		r.FillBytes(sig[:32])
		s.FillBytes(sig[32:])
		return sig
	case ed25519.PrivateKey:
		return ed25519.Sign(priv, input)
	}
	panic("oidctest: unknown key type")
}

// compact builds a compact JWS from header, claims and the signer's output.
func compact(header, claims map[string]any, signer func(input []byte) []byte) string {
	input := b64(mustJSON(header)) + "." + b64(mustJSON(claims))
	return input + "." + b64(signer([]byte(input)))
}

// abort drops the connection without writing a response.
func abort(w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		panic(http.ErrAbortHandler)
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	_ = conn.Close()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func b64(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	must(err)
	return b
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, err := rand.Read(b)
	must(err)
	return hex.EncodeToString(b)
}

// must panics on err. The helpers of this package only fail when the system
// random source fails or on programmer error.
func must(err error) {
	if err != nil {
		panic(fmt.Errorf("oidctest: %w", err))
	}
}
