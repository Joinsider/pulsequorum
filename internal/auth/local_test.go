// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package auth

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/uptimecluster/internal/cluster"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/secrets"
	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/store/sqlite"
)

// Test credentials; they are fixtures, not secrets.
const (
	testPassword    = "correct horse battery staple"
	testPasswordNew = "another long passphrase 42"
	testIP          = "203.0.113.10"
)

func testKeyring(t *testing.T) *secrets.Keyring {
	t.Helper()
	kek, err := secrets.LoadKEK("", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))), "")
	if err != nil {
		t.Fatal(err)
	}
	dek, err := secrets.NewDEK()
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := kek.Wrap("key-1", dek)
	if err != nil {
		t.Fatal(err)
	}
	kr := secrets.NewKeyring(kek, func(context.Context) ([]store.SecretKey, error) {
		return []store.SecretKey{{KeyID: "key-1", WrappedDEK: wrapped, KEKFingerprint: kek.Fingerprint}}, nil
	})
	if err := kr.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	return kr
}

// localFixture is a real single-node cluster on SQLite with an injected clock.
// The Raft layer stamps commands with the wall clock, so the injected clock
// starts at the wall clock and only moves forward.
type localFixture struct {
	t     *testing.T
	ctx   context.Context
	state *sqlite.StateStore
	node  *cluster.Node
	local *Local
	now   time.Time
}

func newLocalFixture(t *testing.T) *localFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	dir := t.TempDir()
	log := slog.New(slog.DiscardHandler)
	state, err := sqlite.OpenState(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	node, err := cluster.Start(ctx, cluster.Config{NodeID: "11111111-1111-1111-1111-111111111111", Advertise: "n:9443",
		DataDir: dir, Timeout: 200 * time.Millisecond, Logger: log, RaftLogLevel: "error"}, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = node.Shutdown()
		_ = state.Close()
	})
	if err := cluster.EnsureDefaultTenant(ctx, node, state); err != nil {
		t.Fatal(err)
	}
	f := &localFixture{t: t, ctx: ctx, state: state, node: node, now: time.Now()}
	f.local = NewLocal(LocalOptions{State: state, Cluster: node, Keyring: testKeyring(t), Logger: log,
		Now: func() time.Time { return f.now }})
	if err := f.local.EnsureSigningKey(ctx); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *localFixture) apply(cmd *pb.Command) {
	f.t.Helper()
	if _, err := f.node.Apply(f.ctx, cmd); err != nil {
		f.t.Fatal(err)
	}
}

// createUser stores a user through Raft with a real Argon2id hash.
func (f *localFixture) createUser(username, role string, disabled bool) store.User {
	f.t.Helper()
	hash, err := HashPassword(testPassword)
	if err != nil {
		f.t.Fatal(err)
	}
	id := uuid.NewString()
	cmd, err := cluster.NewPutUser(uuid.NewString(), cluster.DefaultTenantID, id, cluster.UserDocument{
		Username: username, PasswordHash: hash, Role: role, Disabled: disabled,
	}, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	f.apply(cmd)
	u, err := f.state.GetUser(f.ctx, cluster.DefaultTenantID, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return u
}

// updateUser changes a user through Raft; mut edits the document first.
func (f *localFixture) updateUser(u store.User, mut func(*cluster.UserDocument)) store.User {
	f.t.Helper()
	doc := cluster.UserDocument{Username: u.Username, DisplayName: u.DisplayName, PasswordHash: u.PasswordHash,
		Role: u.Role, Disabled: u.Disabled}
	mut(&doc)
	cmd, err := cluster.NewPutUser(uuid.NewString(), u.TenantID, u.ID, doc, u.Revision)
	if err != nil {
		f.t.Fatal(err)
	}
	f.apply(cmd)
	got, err := f.state.GetUser(f.ctx, u.TenantID, u.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

func (f *localFixture) login(username, password string) (Tokens, error) {
	return f.local.Login(f.ctx, username, password, testIP)
}

func (f *localFixture) mustLogin(username string) Tokens {
	f.t.Helper()
	tok, err := f.login(username, testPassword)
	if err != nil {
		f.t.Fatalf("login %s: %v", username, err)
	}
	return tok
}

func bearerRequest(auth string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	return r
}

// mint signs an access token with a known key, bypassing the local issuer, so
// tests can check key lifetimes independently of the token TTL.
func mint(kid string, priv ed25519.PrivateKey, sub string, now time.Time) string {
	tok, err := SignAccessToken(kid, priv, Claims{
		Issuer: Issuer, Audience: Audience, Subject: sub, TenantID: cluster.DefaultTenantID,
		Roles: []string{string(RoleTenantViewer)}, IssuedAt: now.Unix(), NotBefore: now.Unix(),
		ExpiresAt: now.Add(AccessTTL).Unix(), ID: uuid.NewString(),
	})
	if err != nil {
		panic(err)
	}
	return tok
}

func TestCheckPasswordLength(t *testing.T) {
	tests := []struct {
		length  int
		wantErr bool
	}{
		{MinPasswordLen - 1, true},
		{MinPasswordLen, false},
		{MaxPasswordLen, false},
		{MaxPasswordLen + 1, true},
	}
	for _, tt := range tests {
		err := CheckPassword(strings.Repeat("p", tt.length))
		if (err != nil) != tt.wantErr || (err != nil && !errors.Is(err, ErrWeakPassword)) {
			t.Errorf("CheckPassword(%d bytes) = %v, want error %v", tt.length, err, tt.wantErr)
		}
	}
}

func TestEnsureSigningKeyCreatesOnce(t *testing.T) {
	f := newLocalFixture(t)
	keys, err := f.state.ListSigningKeys(f.ctx)
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys after first start = %d, %v; want 1", len(keys), err)
	}
	if err := f.local.EnsureSigningKey(f.ctx); err != nil {
		t.Fatal(err)
	}
	keys, err = f.state.ListSigningKeys(f.ctx)
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys after second call = %d, %v; want 1", len(keys), err)
	}
	if keys[0].RetiredMs != 0 || keys[0].Alg != "EdDSA" || len(keys[0].PublicKey) != 32 {
		t.Errorf("key = %+v", keys[0])
	}
	if !secrets.IsEncrypted(keys[0].PrivateKeyEnc) {
		t.Error("private key is not stored encrypted")
	}
}

// TestSigningKeyRotation covers FA-AUTH-02: a new key after 31 days, the old
// one retired and valid for verification for about one day only.
func TestSigningKeyRotation(t *testing.T) {
	f := newLocalFixture(t)
	base := f.now
	u := f.createUser("erin", string(RoleTenantViewer), false)
	oldKID, oldKey, ok := f.local.active()
	if !ok {
		t.Fatal("no active key")
	}

	f.now = base.Add(31 * 24 * time.Hour)
	if err := f.local.EnsureSigningKey(f.ctx); err != nil {
		t.Fatal(err)
	}
	keys, err := f.state.ListSigningKeys(f.ctx)
	if err != nil || len(keys) != 2 {
		t.Fatalf("keys after rotation = %d, %v; want 2", len(keys), err)
	}
	newKID, newKey, ok := f.local.active()
	if !ok || newKID == oldKID {
		t.Fatalf("active key after rotation = %q, want a new one", newKID)
	}
	for _, k := range keys {
		switch k.KID {
		case oldKID:
			if k.RetiredMs != f.now.UnixMilli() {
				t.Errorf("old key retired at %d, want %d", k.RetiredMs, f.now.UnixMilli())
			}
		case newKID:
			if k.RetiredMs != 0 {
				t.Error("new key is retired")
			}
		default:
			t.Errorf("unexpected key %s", k.KID)
		}
	}

	retired := f.now
	tests := []struct {
		name   string
		after  time.Duration
		wantOK bool
		useOld bool // sign with the retired key instead of the new one
	}{
		{"old key one hour after rotation", time.Hour, true, true},
		{"old key 23 h after rotation", 23 * time.Hour, true, true},
		{"old key 25 h after rotation", 25 * time.Hour, false, true},
		{"new key 25 h after rotation", 25 * time.Hour, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f.now = retired.Add(tt.after)
			kid, priv := newKID, newKey.priv
			if tt.useOld {
				kid, priv = oldKID, oldKey.priv
			}
			_, err := f.local.Authenticate(bearerRequest("Bearer " + mint(kid, priv, u.ID, f.now)))
			if (err == nil) != tt.wantOK {
				t.Errorf("Authenticate = %v, want success %v", err, tt.wantOK)
			}
			if err != nil && !errors.Is(err, ErrUnauthenticated) {
				t.Errorf("error = %v, want ErrUnauthenticated", err)
			}
		})
	}
}

func TestSetupFlow(t *testing.T) {
	f := newLocalFixture(t)

	tok, ok, err := f.local.PrepareSetup(f.ctx)
	if err != nil || !ok || tok == "" {
		t.Fatalf("PrepareSetup = %q, %v, %v; want a token", tok, ok, err)
	}
	if _, err := f.local.Setup(f.ctx, "not-the-token", "admin", "", testPassword); !errors.Is(err, ErrSetupUnavailable) {
		t.Errorf("Setup with wrong token = %v, want ErrSetupUnavailable", err)
	}
	if _, err := f.local.Setup(f.ctx, tok, "admin", "", "too short"); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("Setup with weak password = %v, want ErrWeakPassword", err)
	}

	// The setup token is valid for one hour only.
	f.now = f.now.Add(61 * time.Minute)
	if _, err := f.local.Setup(f.ctx, tok, "admin", "", testPassword); !errors.Is(err, ErrSetupUnavailable) {
		t.Errorf("Setup with expired token = %v, want ErrSetupUnavailable", err)
	}

	tok, ok, err = f.local.PrepareSetup(f.ctx)
	if err != nil || !ok {
		t.Fatalf("PrepareSetup again = %v, %v", ok, err)
	}
	tokens, err := f.local.Setup(f.ctx, tok, "admin", "Administrator", testPassword)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if tokens.AccessToken == "" || tokens.RefreshToken == "" {
		t.Fatal("Setup returned no tokens")
	}
	u, err := f.state.GetUserByUsername(f.ctx, "admin")
	if err != nil || u.Role != string(RolePlatformAdmin) {
		t.Fatalf("admin = %+v, %v; want platform-admin", u, err)
	}
	p, err := f.local.Authenticate(bearerRequest("Bearer " + tokens.AccessToken))
	if err != nil || !p.Has(RolePlatformAdmin) {
		t.Errorf("Authenticate with setup token = %+v, %v", p, err)
	}

	if _, err := f.local.Setup(f.ctx, tok, "second", "", testPassword); !errors.Is(err, ErrSetupUnavailable) {
		t.Errorf("second Setup = %v, want ErrSetupUnavailable", err)
	}
	if tok, ok, err := f.local.PrepareSetup(f.ctx); err != nil || ok || tok != "" {
		t.Errorf("PrepareSetup after setup = %q, %v, %v; want no token", tok, ok, err)
	}
}

func TestLoginOutcomes(t *testing.T) {
	f := newLocalFixture(t)
	f.createUser("alice", string(RoleTenantAdmin), false)
	f.createUser("dave", string(RoleTenantViewer), true)

	tokens, err := f.login("alice", testPassword)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if !tokens.AccessExpires.Equal(f.now.Add(AccessTTL)) || !tokens.RefreshExpires.Equal(f.now.Add(RefreshTTL)) {
		t.Errorf("expiries = %v / %v", tokens.AccessExpires, tokens.RefreshExpires)
	}
	if tokens.AccessToken == "" || tokens.RefreshToken == "" {
		t.Error("login returned empty tokens")
	}

	tests := []struct {
		name, user, password string
	}{
		{"wrong password", "alice", testPassword + "!"},
		{"unknown user", "mallory", testPassword},
		{"disabled user with right password", "dave", testPassword},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := f.login(tt.user, tt.password); !errors.Is(err, ErrInvalidCredentials) {
				t.Errorf("Login = %v, want ErrInvalidCredentials", err)
			}
		})
	}
}

// TestLoginAccountThrottle covers FA-AUTH-04 per account. Throttle.Free is 5:
// the fifth failure starts a 1 s delay, so the sixth attempt is blocked even
// with the right password.
func TestLoginAccountThrottle(t *testing.T) {
	f := newLocalFixture(t)
	f.createUser("carol", string(RoleTenantEditor), false)
	for i := range 5 {
		if _, err := f.login("carol", "wrong password "+strconv.Itoa(i)); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d = %v, want ErrInvalidCredentials", i+1, err)
		}
	}
	_, err := f.login("carol", testPassword)
	var throttled *ThrottledError
	if !errors.As(err, &throttled) || throttled.RetryAfter <= 0 || throttled.RetryAfter > time.Second {
		t.Fatalf("login during delay = %v, want ThrottledError with RetryAfter in (0, 1s]", err)
	}

	f.now = f.now.Add(time.Second)
	if _, err := f.login("carol", testPassword); err != nil {
		t.Errorf("login after the delay = %v", err)
	}
}

// TestLoginIPThrottle covers FA-AUTH-04 per IP: failures spread over different
// accounts still throttle the address. Throttle.Free is 20, so the 21st attempt
// is blocked after 20 failures.
func TestLoginIPThrottle(t *testing.T) {
	f := newLocalFixture(t)
	f.createUser("frank", string(RoleTenantViewer), false)
	const attacker = "198.51.100.7"
	for i := range 20 {
		name := "nobody-" + strconv.Itoa(i)
		if _, err := f.local.Login(f.ctx, name, testPassword, attacker); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d = %v, want ErrInvalidCredentials", i+1, err)
		}
	}
	var throttled *ThrottledError
	if _, err := f.local.Login(f.ctx, "frank", testPassword, attacker); !errors.As(err, &throttled) {
		t.Fatalf("login from throttled IP = %v, want ThrottledError", err)
	}
	if _, err := f.local.Login(f.ctx, "frank", testPassword, "198.51.100.8"); err != nil {
		t.Errorf("login from another IP = %v", err)
	}
}

// TestLoginOverlongPassword: a password longer than MaxPasswordLen must not
// match the stored password of its first MaxPasswordLen bytes.
func TestLoginOverlongPassword(t *testing.T) {
	f := newLocalFixture(t)
	long := strings.Repeat("Ab1!", MaxPasswordLen/4) // exactly MaxPasswordLen bytes
	hash, err := HashPassword(long)
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := cluster.NewPutUser(uuid.NewString(), cluster.DefaultTenantID, uuid.NewString(), cluster.UserDocument{
		Username: "longpw", PasswordHash: hash, Role: string(RoleTenantViewer),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.apply(cmd)

	if _, err := f.login("longpw", long+"X"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("overlong password with matching prefix = %v, want ErrInvalidCredentials", err)
	}
	if _, err := f.login("longpw", long); err != nil {
		t.Errorf("stored password = %v", err)
	}
}

// TestChangePasswordThrottle: wrong current passwords count as failures; after
// five of them the next call waits, even with the right current password.
func TestChangePasswordThrottle(t *testing.T) {
	f := newLocalFixture(t)
	u := f.createUser("ivan2", string(RoleTenantEditor), false)
	p := Principal{UserID: u.ID, Username: u.Username, TenantID: u.TenantID, Roles: []Role{RoleTenantEditor}}
	for i := range 5 {
		if _, err := f.local.ChangePassword(f.ctx, p, "wrong current "+strconv.Itoa(i), testPasswordNew); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d = %v, want ErrInvalidCredentials", i+1, err)
		}
	}
	_, err := f.local.ChangePassword(f.ctx, p, testPassword, testPasswordNew)
	var throttled *ThrottledError
	if !errors.As(err, &throttled) || throttled.RetryAfter <= 0 {
		t.Fatalf("ChangePassword after five failures = %v, want ThrottledError", err)
	}

	f.now = f.now.Add(time.Second)
	if _, err := f.local.ChangePassword(f.ctx, p, testPassword, testPasswordNew); err != nil {
		t.Errorf("ChangePassword after the delay = %v", err)
	}
}

func TestRefreshRotatesAndDetectsReuse(t *testing.T) {
	f := newLocalFixture(t)
	f.createUser("gina", string(RoleTenantViewer), false)
	t1 := f.mustLogin("gina")

	t2, err := f.local.Refresh(f.ctx, t1.RefreshToken)
	if err != nil || t2.RefreshToken == "" || t2.RefreshToken == t1.RefreshToken {
		t.Fatalf("first refresh = %v", err)
	}
	t3, err := f.local.Refresh(f.ctx, t2.RefreshToken)
	if err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if _, err := f.local.Authenticate(bearerRequest("Bearer " + t3.AccessToken)); err != nil {
		t.Errorf("access token from refresh = %v", err)
	}

	// Presenting the first token again is a reuse: the whole family is revoked,
	// including the newest token.
	if _, err := f.local.Refresh(f.ctx, t1.RefreshToken); !errors.Is(err, ErrInvalidSession) {
		t.Errorf("reused token = %v, want ErrInvalidSession", err)
	}
	if _, err := f.local.Refresh(f.ctx, t3.RefreshToken); !errors.Is(err, ErrInvalidSession) {
		t.Errorf("newest token after reuse = %v, want ErrInvalidSession", err)
	}
}

func TestRefreshRejectsUnknownTokens(t *testing.T) {
	f := newLocalFixture(t)
	for _, tok := range []string{"", "garbage", strings.Repeat("A", 43)} {
		if _, err := f.local.Refresh(f.ctx, tok); !errors.Is(err, ErrInvalidSession) {
			t.Errorf("Refresh(%q) = %v, want ErrInvalidSession", tok, err)
		}
	}
}

func TestLogoutRevokesFamily(t *testing.T) {
	f := newLocalFixture(t)
	f.createUser("hank", string(RoleTenantViewer), false)
	t1 := f.mustLogin("hank")
	t2, err := f.local.Refresh(f.ctx, t1.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.local.Logout(f.ctx, t2.RefreshToken); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := f.local.Refresh(f.ctx, t2.RefreshToken); !errors.Is(err, ErrInvalidSession) {
		t.Errorf("Refresh after logout = %v, want ErrInvalidSession", err)
	}
	for _, tok := range []string{"", "unknown-token"} {
		if err := f.local.Logout(f.ctx, tok); err != nil {
			t.Errorf("Logout(%q) = %v, want nil", tok, err)
		}
	}
}

func TestChangePasswordRevokesSessions(t *testing.T) {
	f := newLocalFixture(t)
	u := f.createUser("ivan", string(RoleTenantEditor), false)
	sA := f.mustLogin("ivan")
	sB := f.mustLogin("ivan")
	p := Principal{UserID: u.ID, Username: u.Username, TenantID: u.TenantID, Roles: []Role{RoleTenantEditor}}

	if _, err := f.local.ChangePassword(f.ctx, p, "not the password", testPasswordNew); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("ChangePassword with wrong current = %v, want ErrInvalidCredentials", err)
	}
	if _, err := f.local.ChangePassword(f.ctx, p, testPassword, "short"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("ChangePassword with weak new = %v, want ErrWeakPassword", err)
	}
	if _, err := f.login("ivan", testPassword); err != nil {
		t.Fatalf("old password must still work after failed changes: %v", err)
	}

	t3, err := f.local.ChangePassword(f.ctx, p, testPassword, testPasswordNew)
	if err != nil || t3.RefreshToken == "" {
		t.Fatalf("ChangePassword = %v", err)
	}
	for name, old := range map[string]string{"first session": sA.RefreshToken, "second session": sB.RefreshToken} {
		if _, err := f.local.Refresh(f.ctx, old); !errors.Is(err, ErrInvalidSession) {
			t.Errorf("%s after password change = %v, want ErrInvalidSession", name, err)
		}
	}
	if _, err := f.local.Refresh(f.ctx, t3.RefreshToken); err != nil {
		t.Errorf("session started by the password change = %v", err)
	}
	if _, err := f.login("ivan", testPassword); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("old password after change = %v, want ErrInvalidCredentials", err)
	}
	if _, err := f.login("ivan", testPasswordNew); err != nil {
		t.Errorf("new password = %v", err)
	}
}

func TestAuthenticateRejectsBadHeaders(t *testing.T) {
	f := newLocalFixture(t)
	f.createUser("judy", string(RoleTenantViewer), false)
	refresh := f.mustLogin("judy").RefreshToken
	for _, h := range []string{"", "Basic dXNlcjpwdw==", "Bearer", "Bearer ", "Bearer garbage", "Bearer " + refresh} {
		if _, err := f.local.Authenticate(bearerRequest(h)); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("Authenticate(%q) = %v, want ErrUnauthenticated", h, err)
		}
	}
}

func TestAuthenticateUsesCurrentUserRecord(t *testing.T) {
	f := newLocalFixture(t)
	u := f.createUser("kim", string(RoleTenantViewer), false)
	access := f.mustLogin("kim").AccessToken

	p, err := f.local.Authenticate(bearerRequest("Bearer " + access))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.UserID != u.ID || p.TenantID != cluster.DefaultTenantID || p.Username != "kim" || len(p.Roles) != 1 || p.Roles[0] != RoleTenantViewer {
		t.Fatalf("principal = %+v", p)
	}

	// A role change applies to the same, still valid access token at once.
	u = f.updateUser(u, func(d *cluster.UserDocument) { d.Role = string(RoleTenantAdmin) })
	if p, err := f.local.Authenticate(bearerRequest("Bearer " + access)); err != nil || !p.Has(RoleTenantAdmin) || p.Has(RoleTenantViewer) {
		t.Errorf("after role change: %+v, %v; want tenant-admin", p, err)
	}

	// Disabling the user ends authentication with the same token.
	u = f.updateUser(u, func(d *cluster.UserDocument) { d.Disabled = true })
	if _, err := f.local.Authenticate(bearerRequest("Bearer " + access)); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("token of disabled user = %v, want ErrUnauthenticated", err)
	}

	// So does deleting the user.
	f.updateUser(u, func(d *cluster.UserDocument) { d.Disabled = false })
	access = f.mustLogin("kim").AccessToken
	if _, err := f.local.Authenticate(bearerRequest("Bearer " + access)); err != nil {
		t.Fatalf("token after re-enabling: %v", err)
	}
	f.apply(cluster.NewDeleteUser(uuid.NewString(), cluster.DefaultTenantID, u.ID))
	if _, err := f.local.Authenticate(bearerRequest("Bearer " + access)); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("token of deleted user = %v, want ErrUnauthenticated", err)
	}
}

func TestCanAssignRole(t *testing.T) {
	platform := Principal{Roles: []Role{RolePlatformAdmin}}
	tenantAdmin := Principal{Roles: []Role{RoleTenantAdmin}}
	editor := Principal{Roles: []Role{RoleTenantEditor}}
	viewer := Principal{Roles: []Role{RoleTenantViewer}}
	tests := []struct {
		name  string
		actor Principal
		role  Role
		want  bool
	}{
		{"platform admin creates platform admin", platform, RolePlatformAdmin, true},
		{"platform admin creates tenant admin", platform, RoleTenantAdmin, true},
		{"platform admin creates editor", platform, RoleTenantEditor, true},
		{"platform admin creates viewer", platform, RoleTenantViewer, true},
		{"tenant admin creates platform admin", tenantAdmin, RolePlatformAdmin, false},
		{"tenant admin creates tenant admin", tenantAdmin, RoleTenantAdmin, true},
		{"tenant admin creates editor", tenantAdmin, RoleTenantEditor, true},
		{"tenant admin creates viewer", tenantAdmin, RoleTenantViewer, true},
		{"editor creates viewer", editor, RoleTenantViewer, false},
		{"viewer creates viewer", viewer, RoleTenantViewer, false},
		{"unknown role by platform admin", platform, Role("root"), false},
		{"empty role by tenant admin", tenantAdmin, Role(""), false},
		{"no roles", Principal{}, RoleTenantViewer, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CanAssignRole(tt.actor, tt.role); got != tt.want {
				t.Errorf("CanAssignRole = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestLoginReloadsMissingSigningKey covers a voter that loaded its keys before
// it had caught up with the leader: it had no signing key, and logins failed
// until the hourly maintenance. A login now reloads the keys, at most once
// per reloadMinInterval.
func TestLoginReloadsMissingSigningKey(t *testing.T) {
	f := newLocalFixture(t)
	f.createUser("alice", string(RoleTenantAdmin), false)

	late := NewLocal(LocalOptions{State: f.state, Cluster: f.node, Keyring: f.local.opts.Keyring,
		Logger: slog.New(slog.DiscardHandler), Now: func() time.Time { return f.now }})
	// As if the boot reload had found no key a moment ago.
	late.keys, late.lastReload = map[string]signingKey{}, f.now

	if _, err := late.Login(f.ctx, "alice", testPassword, testIP); err == nil {
		t.Fatal("login right after an empty reload succeeded, want the rate limit to hold")
	}
	f.now = f.now.Add(reloadMinInterval + time.Second)
	tokens, err := late.Login(f.ctx, "alice", testPassword, testIP)
	if err != nil {
		t.Fatalf("login after the reload interval: %v", err)
	}
	if tokens.AccessToken == "" {
		t.Error("empty access token")
	}
}
