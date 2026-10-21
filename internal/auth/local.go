// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/uptimecluster/internal/cluster"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
)

// Local mode (ADR 0008): Argon2id passwords, EdDSA access tokens signed with
// keys from the replicated state, rotating refresh tokens stored as hashes.

const (
	// RefreshTTL is the sliding lifetime of a refresh token family.
	RefreshTTL = 14 * 24 * time.Hour
	// KeyRotation is how often a new signing key is created.
	KeyRotation = 30 * 24 * time.Hour
	// keyOverlap keeps a retired key valid for verification.
	keyOverlap = 24*time.Hour + AccessTTL
	setupTTL   = time.Hour
	// Throttling (FA-AUTH-04): free failures per account and per IP.
	accountFreeFailures = 5
	ipFreeFailures      = 20
	reloadMinInterval   = 10 * time.Second
	applyTimeout        = 5 * time.Second
)

// Errors of the local authentication. Callers map them to one generic message
// so responses never reveal whether an account exists.
var (
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrInvalidSession     = errors.New("session is not valid")
	ErrSetupUnavailable   = errors.New("setup is not available")
	ErrWeakPassword       = fmt.Errorf("password must have %d to %d characters", MinPasswordLen, MaxPasswordLen)
)

// ThrottledError asks the client to wait.
type ThrottledError struct{ RetryAfter time.Duration }

func (e *ThrottledError) Error() string { return "too many failed attempts" }

// Applier proposes Raft commands (*cluster.Node).
type Applier interface {
	Apply(ctx context.Context, cmd *pb.Command) (cluster.ApplyResult, error)
}

// Keyring encrypts the private signing keys (*secrets.Keyring, ADR 0011).
type Keyring interface {
	Encrypt(plaintext []byte, aad string) (string, error)
	Decrypt(ctx context.Context, ciphertext, aad string) ([]byte, error)
}

// LocalOptions configure the local authentication.
type LocalOptions struct {
	State   store.State
	Cluster Applier
	Keyring Keyring
	Logger  *slog.Logger
	Now     func() time.Time
}

// Tokens are the result of a login, setup or refresh. RefreshToken goes into
// the HttpOnly cookie only.
type Tokens struct {
	AccessToken    string
	AccessExpires  time.Time
	RefreshToken   string
	RefreshExpires time.Time
}

// Local implements Authenticator for AUTH_MODE=local.
type Local struct {
	opts LocalOptions

	mu         sync.RWMutex
	keys       map[string]signingKey
	lastReload time.Time

	setupMu      sync.Mutex
	setupHash    string
	setupExpires time.Time

	accounts *Throttle
	ips      *Throttle
}

type signingKey struct {
	pub     ed25519.PublicKey
	priv    ed25519.PrivateKey
	created time.Time
	retired time.Time // zero = active
}

var _ Authenticator = (*Local)(nil)

// NewLocal creates the local authentication.
func NewLocal(opts LocalOptions) *Local {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Local{opts: opts, keys: map[string]signingKey{},
		accounts: NewThrottle(accountFreeFailures), ips: NewThrottle(ipFreeFailures)}
}

func privateKeyAAD(kid string) string { return "signing_keys/" + kid + "/private" }

// ReloadKeys reads the signing keys from the replicated state.
func (l *Local) ReloadKeys(ctx context.Context) error {
	stored, err := l.opts.State.ListSigningKeys(ctx)
	if err != nil {
		return err
	}
	now := l.opts.Now()
	keys := make(map[string]signingKey, len(stored))
	for _, k := range stored {
		sk := signingKey{pub: ed25519.PublicKey(k.PublicKey), created: time.UnixMilli(k.CreatedMs)}
		if k.RetiredMs != 0 {
			sk.retired = time.UnixMilli(k.RetiredMs)
			if now.Sub(sk.retired) > keyOverlap {
				continue
			}
		}
		if sk.retired.IsZero() {
			priv, err := l.opts.Keyring.Decrypt(ctx, k.PrivateKeyEnc, privateKeyAAD(k.KID))
			if err != nil {
				return fmt.Errorf("decrypt signing key %s: %w", k.KID, err)
			}
			if len(priv) != ed25519.PrivateKeySize {
				return fmt.Errorf("signing key %s: wrong private key size", k.KID)
			}
			sk.priv = ed25519.PrivateKey(priv)
		}
		keys[k.KID] = sk
	}
	l.mu.Lock()
	l.keys, l.lastReload = keys, now
	l.mu.Unlock()
	return nil
}

// active returns the newest active signing key.
func (l *Local) active() (string, signingKey, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var kid string
	var best signingKey
	for id, k := range l.keys {
		if k.retired.IsZero() && k.priv != nil && (kid == "" || k.created.After(best.created) || (k.created.Equal(best.created) && id > kid)) {
			kid, best = id, k
		}
	}
	return kid, best, kid != ""
}

// lookup is the KeyLookup for verification. An unknown key ID triggers a
// rate-limited reload (a new key from another voter, FA-AUTH-02).
func (l *Local) lookup(ctx context.Context) KeyLookup {
	return func(kid string) (ed25519.PublicKey, bool) {
		l.mu.RLock()
		k, ok := l.keys[kid]
		stale := l.opts.Now().Sub(l.lastReload) > reloadMinInterval
		l.mu.RUnlock()
		if !ok && stale {
			if err := l.ReloadKeys(ctx); err != nil {
				l.opts.Logger.Warn("reloading signing keys failed", "err", err)
				return nil, false
			}
			l.mu.RLock()
			k, ok = l.keys[kid]
			l.mu.RUnlock()
		}
		if !ok || (!k.retired.IsZero() && l.opts.Now().Sub(k.retired) > keyOverlap) {
			return nil, false
		}
		return k.pub, true
	}
}

// EnsureSigningKey creates a signing key if there is none or the active one is
// older than KeyRotation, and retires the previous keys. Leader only.
func (l *Local) EnsureSigningKey(ctx context.Context) error {
	if err := l.ReloadKeys(ctx); err != nil {
		return err
	}
	now := l.opts.Now()
	if _, k, ok := l.active(); ok && now.Sub(k.created) < KeyRotation {
		return nil
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate signing key: %w", err)
	}
	kid := uuid.NewString()
	enc, err := l.opts.Keyring.Encrypt(priv, privateKeyAAD(kid))
	if err != nil {
		return fmt.Errorf("encrypt signing key: %w", err)
	}
	cmd, err := cluster.NewPutSigningKey("signing-key:"+kid, kid, cluster.SigningKeyDocument{Alg: jwtAlg, PublicKey: pub, PrivateKeyEnc: enc})
	if err != nil {
		return err
	}
	if _, err := l.apply(ctx, cmd); err != nil {
		return fmt.Errorf("store signing key: %w", err)
	}
	stored, err := l.opts.State.ListSigningKeys(ctx)
	if err != nil {
		return err
	}
	for _, k := range stored {
		if k.KID == kid || k.RetiredMs != 0 {
			continue
		}
		cmd, err := cluster.NewPutSigningKey("retire:"+k.KID, k.KID, cluster.SigningKeyDocument{
			Alg: k.Alg, PublicKey: k.PublicKey, PrivateKeyEnc: k.PrivateKeyEnc, RetiredMs: now.UnixMilli(),
		})
		if err != nil {
			return err
		}
		if _, err := l.apply(ctx, cmd); err != nil {
			return fmt.Errorf("retire signing key %s: %w", k.KID, err)
		}
	}
	l.opts.Logger.Info("created a new access token signing key", "kid", kid)
	return l.ReloadKeys(ctx)
}

// RunKeyMaintenance keeps the signing keys current: the leader rotates them,
// every node reloads them regularly.
func (l *Local) RunKeyMaintenance(ctx context.Context, isLeader func() bool, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		var err error
		if isLeader() {
			err = l.EnsureSigningKey(ctx)
		} else {
			err = l.ReloadKeys(ctx)
		}
		if err != nil && ctx.Err() == nil {
			l.opts.Logger.Warn("signing key maintenance failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// PrepareSetup creates the one-time setup token if no user exists yet
// (FA-AUTH-01). The token is returned for the startup log; only its hash is
// kept, for one hour.
func (l *Local) PrepareSetup(ctx context.Context) (string, bool, error) {
	n, err := l.opts.State.CountUsers(ctx)
	if err != nil || n > 0 {
		return "", false, err
	}
	token, err := NewOpaqueToken()
	if err != nil {
		return "", false, err
	}
	l.setupMu.Lock()
	l.setupHash, l.setupExpires = HashToken(token), l.opts.Now().Add(setupTTL)
	l.setupMu.Unlock()
	return token, true, nil
}

// Setup creates the first platform admin with the setup token and logs it in.
func (l *Local) Setup(ctx context.Context, token, username, displayName, password string) (Tokens, error) {
	l.setupMu.Lock()
	valid := l.setupHash != "" && l.opts.Now().Before(l.setupExpires) && TokenMatches(token, l.setupHash)
	l.setupMu.Unlock()
	if !valid {
		return Tokens{}, ErrSetupUnavailable
	}
	if err := CheckPassword(password); err != nil {
		return Tokens{}, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return Tokens{}, err
	}
	id := uuid.NewString()
	cmd, err := cluster.NewPutUser(uuid.NewString(), cluster.DefaultTenantID, id, cluster.UserDocument{
		Username: strings.TrimSpace(username), DisplayName: displayName, PasswordHash: hash,
		Role: string(RolePlatformAdmin), Setup: true,
	}, 0)
	if err != nil {
		return Tokens{}, err
	}
	if _, err := l.apply(ctx, cmd); err != nil {
		return Tokens{}, err
	}
	l.setupMu.Lock()
	l.setupHash = ""
	l.setupMu.Unlock()
	u, err := l.opts.State.GetUser(ctx, cluster.DefaultTenantID, id)
	if err != nil {
		return Tokens{}, err
	}
	return l.startSession(ctx, u)
}

// CheckPassword enforces the length policy (NFA-SEC-03, OWASP).
func CheckPassword(password string) error {
	if len(password) < MinPasswordLen || len(password) > MaxPasswordLen {
		return ErrWeakPassword
	}
	return nil
}

// Login checks the credentials with throttling per account and per IP
// (FA-AUTH-04) and starts a session.
func (l *Local) Login(ctx context.Context, username, password, clientIP string) (Tokens, error) {
	now := l.opts.Now()
	accountKey := "u:" + strings.ToLower(strings.TrimSpace(username))
	ipKey := "ip:" + clientIP
	if wait := max(l.accounts.Wait(accountKey, now), l.ips.Wait(ipKey, now)); wait > 0 {
		return Tokens{}, &ThrottledError{RetryAfter: wait}
	}
	if len(password) > MaxPasswordLen {
		// No stored password is that long. Hash a bounded prefix only to keep
		// the timing of a normal failure, then fail.
		DummyVerify(password[:MaxPasswordLen])
		l.fail(accountKey, ipKey, now)
		return Tokens{}, ErrInvalidCredentials
	}
	u, err := l.opts.State.GetUserByUsername(ctx, strings.TrimSpace(username))
	switch {
	case errors.Is(err, store.ErrNotFound):
		DummyVerify(password)
		l.fail(accountKey, ipKey, now)
		return Tokens{}, ErrInvalidCredentials
	case err != nil:
		return Tokens{}, err
	}
	ok, _, err := VerifyPassword(password, u.PasswordHash)
	if err != nil || !ok || u.Disabled {
		l.fail(accountKey, ipKey, now)
		return Tokens{}, ErrInvalidCredentials
	}
	l.accounts.Succeed(accountKey)
	return l.startSession(ctx, u)
}

func (l *Local) fail(accountKey, ipKey string, now time.Time) {
	l.accounts.Fail(accountKey, now)
	l.ips.Fail(ipKey, now)
}

// startSession creates a token family and the first token pair.
func (l *Local) startSession(ctx context.Context, u store.User) (Tokens, error) {
	refresh, err := NewOpaqueToken()
	if err != nil {
		return Tokens{}, err
	}
	expires := l.opts.Now().Add(RefreshTTL)
	cmd := cluster.NewCreateSession(uuid.NewString(), u.TenantID, u.ID, HashToken(refresh), uuid.NewString(), expires.UnixMilli())
	if _, err := l.apply(ctx, cmd); err != nil {
		return Tokens{}, err
	}
	access, accessExp, err := l.issue(u)
	if err != nil {
		return Tokens{}, err
	}
	return Tokens{AccessToken: access, AccessExpires: accessExp, RefreshToken: refresh, RefreshExpires: expires}, nil
}

func (l *Local) issue(u store.User) (string, time.Time, error) {
	kid, key, ok := l.active()
	if !ok {
		return "", time.Time{}, errors.New("no signing key available")
	}
	now := l.opts.Now()
	exp := now.Add(AccessTTL)
	token, err := SignAccessToken(kid, key.priv, Claims{
		Issuer: Issuer, Audience: Audience, Subject: u.ID, TenantID: u.TenantID, Roles: []string{u.Role},
		IssuedAt: now.Unix(), NotBefore: now.Unix(), ExpiresAt: exp.Unix(), ID: uuid.NewString(),
	})
	return token, exp, err
}

// Refresh rotates the refresh token (FA-AUTH-03). A reused token revokes its
// whole family in the FSM and fails here.
func (l *Local) Refresh(ctx context.Context, refreshToken string) (Tokens, error) {
	if refreshToken == "" {
		return Tokens{}, ErrInvalidSession
	}
	next, err := NewOpaqueToken()
	if err != nil {
		return Tokens{}, err
	}
	expires := l.opts.Now().Add(RefreshTTL)
	res, err := l.apply(ctx, cluster.NewRotateSession(HashToken(refreshToken), HashToken(next), expires.UnixMilli()))
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, cluster.ErrInvalidCommand) {
		return Tokens{}, ErrInvalidSession
	}
	if err != nil {
		return Tokens{}, err
	}
	var r cluster.SessionResult
	if err := json.Unmarshal(res.Result, &r); err != nil {
		return Tokens{}, fmt.Errorf("decode session result: %w", err)
	}
	if r.Status == cluster.SessionReused {
		l.opts.Logger.Warn("refresh token reused; session family revoked", "tenant", r.TenantID, "user", r.UserID)
	}
	if r.Status != cluster.SessionRotated {
		return Tokens{}, ErrInvalidSession
	}
	u, err := l.opts.State.GetUser(ctx, r.TenantID, r.UserID)
	if err != nil {
		return Tokens{}, ErrInvalidSession
	}
	access, accessExp, err := l.issue(u)
	if err != nil {
		return Tokens{}, err
	}
	return Tokens{AccessToken: access, AccessExpires: accessExp, RefreshToken: next, RefreshExpires: expires}, nil
}

// Logout revokes the token family of a refresh token; unknown tokens are ignored.
func (l *Local) Logout(ctx context.Context, refreshToken string) error {
	if refreshToken == "" {
		return nil
	}
	s, err := l.opts.State.GetSession(ctx, HashToken(refreshToken))
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = l.apply(ctx, cluster.NewRevokeFamily(uuid.NewString(), s.FamilyID))
	return err
}

// ChangePassword verifies the current password and stores the new one; the
// FSM revokes all sessions of the user (the caller starts a new one).
func (l *Local) ChangePassword(ctx context.Context, p Principal, current, next string) (Tokens, error) {
	u, err := l.opts.State.GetUser(ctx, p.TenantID, p.UserID)
	if err != nil {
		return Tokens{}, err
	}
	// A stolen access token must not allow unlimited guessing (FA-AUTH-04).
	now := l.opts.Now()
	accountKey := "u:" + strings.ToLower(u.Username)
	if wait := l.accounts.Wait(accountKey, now); wait > 0 {
		return Tokens{}, &ThrottledError{RetryAfter: wait}
	}
	if len(current) > MaxPasswordLen {
		current = ""
	}
	if ok, _, err := VerifyPassword(current, u.PasswordHash); err != nil || !ok {
		l.accounts.Fail(accountKey, now)
		return Tokens{}, ErrInvalidCredentials
	}
	l.accounts.Succeed(accountKey)
	if err := CheckPassword(next); err != nil {
		return Tokens{}, err
	}
	hash, err := HashPassword(next)
	if err != nil {
		return Tokens{}, err
	}
	cmd, err := cluster.NewPutUser(uuid.NewString(), u.TenantID, u.ID, cluster.UserDocument{
		Username: u.Username, DisplayName: u.DisplayName, PasswordHash: hash, Role: u.Role, Disabled: u.Disabled,
	}, u.Revision)
	if err != nil {
		return Tokens{}, err
	}
	if _, err := l.apply(ctx, cmd); err != nil {
		return Tokens{}, err
	}
	u, err = l.opts.State.GetUser(ctx, u.TenantID, u.ID)
	if err != nil {
		return Tokens{}, err
	}
	return l.startSession(ctx, u)
}

// Authenticate implements Authenticator: a valid bearer token of an existing,
// enabled user. The role comes from the current user record, so role changes
// apply immediately.
func (l *Local) Authenticate(r *http.Request) (Principal, error) {
	header := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || token == "" {
		return Principal{}, ErrUnauthenticated
	}
	c, err := VerifyAccessToken(token, l.lookup(r.Context()), l.opts.Now())
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	u, err := l.opts.State.GetUser(r.Context(), c.TenantID, c.Subject)
	if err != nil || u.Disabled || !ValidRole(Role(u.Role)) {
		return Principal{}, ErrUnauthenticated
	}
	return Principal{UserID: u.ID, Username: u.Username, TenantID: u.TenantID, Roles: []Role{Role(u.Role)}}, nil
}

func (l *Local) apply(ctx context.Context, cmd *pb.Command) (cluster.ApplyResult, error) {
	ctx, cancel := context.WithTimeout(ctx, applyTimeout)
	defer cancel()
	return l.opts.Cluster.Apply(ctx, cmd)
}

// CanAssignRole reports whether actor may give role to a user of its tenant:
// only platform admins create platform admins (FA-AUTH-09).
func CanAssignRole(actor Principal, role Role) bool {
	if !ValidRole(role) || !actor.CanAdmin() {
		return false
	}
	return role != RolePlatformAdmin || slices.Contains(actor.Roles, RolePlatformAdmin)
}
