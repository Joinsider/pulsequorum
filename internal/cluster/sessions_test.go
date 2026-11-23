// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/store/sqlite"
)

// Fixtures only: the FSM checks the prefix, never the content of a hash.
const fakeArgon2Hash = "$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNo"

// replica is one FSM on its own SQLite store, applying a fixed log.
type replica struct {
	t     *testing.T
	fsm   *FSM
	st    *sqlite.StateStore
	index uint64
}

func newReplica(t *testing.T) *replica {
	t.Helper()
	st := openStore(t, t.TempDir())
	fsm, err := NewFSM(t.Context(), st, slog.New(slog.DiscardHandler), func(err error) { t.Errorf("unexpected halt: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	r := &replica{t: t, fsm: fsm, st: st}
	r.apply(must(NewPutTenant("tenant-default", DefaultTenantID, TenantDocument{Name: "Default", Plan: "default"})), 1000)
	return r
}

// must unwraps the builders of commands; they only fail on programming errors.
func must(cmd *pb.Command, err error) *pb.Command {
	if err != nil {
		panic(err)
	}
	return cmd
}

// apply applies cmd with the given command time and returns the FSM result.
func (r *replica) apply(cmd *pb.Command, issuedAt int64) ApplyResult {
	r.t.Helper()
	r.index++
	c := proto.Clone(cmd).(*pb.Command)
	c.IssuedAtMs = issuedAt
	data, err := proto.Marshal(c)
	if err != nil {
		r.t.Fatal(err)
	}
	return r.fsm.Apply(&raft.Log{Index: r.index, Type: raft.LogCommand, Data: data}).(ApplyResult)
}

// sha256Hex stands in for a token hash: 64 hex characters.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func userDoc(name, role string) UserDocument {
	return UserDocument{Username: name, PasswordHash: fakeArgon2Hash, Role: role}
}

// createUser stores a user and returns it with its revision.
func (r *replica) createUser(id string, doc UserDocument) store.User {
	r.t.Helper()
	if res := r.apply(must(NewPutUser("create-"+id, DefaultTenantID, id, doc, 0)), 1100); res.Err != nil {
		r.t.Fatalf("create user %s: %v", id, res.Err)
	}
	u, err := r.st.GetUser(r.t.Context(), DefaultTenantID, id)
	if err != nil {
		r.t.Fatal(err)
	}
	return u
}

func (r *replica) session(t *testing.T, id string) store.Session {
	t.Helper()
	s, err := r.st.GetSession(t.Context(), sha256Hex(id))
	if err != nil {
		t.Fatalf("session %s: %v", id, err)
	}
	return s
}

func TestUserDocumentValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		tenant  string
		doc     UserDocument
		wantErr error
	}{
		{"valid user", DefaultTenantID, userDoc("alice", "tenant-admin"), nil},
		{"plaintext password", DefaultTenantID, UserDocument{Username: "bob", PasswordHash: "hunter2hunter2", Role: "tenant-viewer"}, ErrInvalidCommand},
		{"unknown role", DefaultTenantID, userDoc("carol", "root"), ErrInvalidCommand},
		{"empty role", DefaultTenantID, userDoc("carol", ""), ErrInvalidCommand},
		{"blank username", DefaultTenantID, userDoc("   ", "tenant-viewer"), ErrInvalidCommand},
		{"username too long", DefaultTenantID, userDoc(strings.Repeat("u", 65), "tenant-viewer"), ErrInvalidCommand},
		{"username of 64 characters", DefaultTenantID, userDoc(strings.Repeat("u", 64), "tenant-viewer"), nil},
		{"missing tenant", "", userDoc("dave", "tenant-viewer"), ErrInvalidCommand},
	}
	r := newReplica(t)
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := "user-" + strconv.Itoa(i)
			res := r.apply(must(NewPutUser("req-"+id, tt.tenant, id, tt.doc, 0)), 2000+int64(i))
			if (tt.wantErr == nil) != (res.Err == nil) || (tt.wantErr != nil && !errors.Is(res.Err, tt.wantErr)) {
				t.Errorf("Apply error = %v, want %v", res.Err, tt.wantErr)
			}
		})
	}
}

func TestSetupFlagAllowsOnlyTheFirstUser(t *testing.T) {
	t.Parallel()
	r := newReplica(t)
	setup := userDoc("admin", "platform-admin")
	setup.Setup = true

	if res := r.apply(must(NewPutUser("setup-1", DefaultTenantID, "admin", setup, 0)), 2000); res.Err != nil {
		t.Fatalf("first setup: %v", res.Err)
	}
	second := userDoc("admin2", "platform-admin")
	second.Setup = true
	if res := r.apply(must(NewPutUser("setup-2", DefaultTenantID, "admin2", second, 0)), 2001); !errors.Is(res.Err, store.ErrConflict) {
		t.Errorf("second setup = %v, want ErrConflict", res.Err)
	}
	if res := r.apply(must(NewPutUser("plain", DefaultTenantID, "viewer", userDoc("viewer", "tenant-viewer"), 0)), 2002); res.Err != nil {
		t.Errorf("ordinary user after setup: %v", res.Err)
	}

	// On an empty store a setup with a revision is not a fresh setup either.
	fresh := newReplica(t)
	if res := fresh.apply(must(NewPutUser("setup-rev", DefaultTenantID, "admin", setup, 1)), 2000); !errors.Is(res.Err, store.ErrConflict) {
		t.Errorf("setup with revision 1 = %v, want ErrConflict", res.Err)
	}
}

func TestSigningKeyValidation(t *testing.T) {
	t.Parallel()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		doc     SigningKeyDocument
		wantErr bool
	}{
		{"Ed25519 with encrypted private part", SigningKeyDocument{Alg: "EdDSA", PublicKey: pub, PrivateKeyEnc: "enc:v1:AAAA"}, false},
		{"RSA algorithm", SigningKeyDocument{Alg: "RS256", PublicKey: pub, PrivateKeyEnc: "enc:v1:AAAA"}, true},
		{"HMAC algorithm", SigningKeyDocument{Alg: "HS256", PublicKey: pub, PrivateKeyEnc: "enc:v1:AAAA"}, true},
		{"public key too short", SigningKeyDocument{Alg: "EdDSA", PublicKey: pub[:31], PrivateKeyEnc: "enc:v1:AAAA"}, true},
		{"no public key", SigningKeyDocument{Alg: "EdDSA", PrivateKeyEnc: "enc:v1:AAAA"}, true},
		{"unencrypted private key", SigningKeyDocument{Alg: "EdDSA", PublicKey: pub, PrivateKeyEnc: "plain-private-key"}, true},
		{"missing private key", SigningKeyDocument{Alg: "EdDSA", PublicKey: pub}, true},
	}
	r := newReplica(t)
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kid := "kid-" + strconv.Itoa(i)
			res := r.apply(must(NewPutSigningKey("key-"+kid, kid, tt.doc)), 3000)
			switch {
			case tt.wantErr && !errors.Is(res.Err, ErrInvalidCommand):
				t.Errorf("error = %v, want ErrInvalidCommand", res.Err)
			case !tt.wantErr && res.Err != nil:
				t.Errorf("error = %v, want nil", res.Err)
			}
		})
	}
}

func TestCreateSessionRules(t *testing.T) {
	t.Parallel()
	r := newReplica(t)
	active := r.createUser("active", userDoc("active", "tenant-viewer"))
	disabled := userDoc("disabled", "tenant-viewer")
	disabled.Disabled = true
	r.createUser("disabled", disabled)

	const now = int64(4000)
	const later = now + 3_600_000
	tests := []struct {
		name    string
		tenant  string
		user    string
		hash    string
		family  string
		expires int64
		wantErr error
	}{
		{"short hash", DefaultTenantID, active.ID, sha256Hex("s")[:63], "fam", later, ErrInvalidCommand},
		{"hash not hex length", DefaultTenantID, active.ID, strings.Repeat("x", 65), "fam", later, ErrInvalidCommand},
		{"already expired", DefaultTenantID, active.ID, sha256Hex("expired"), "fam", now, ErrInvalidCommand},
		{"missing family", DefaultTenantID, active.ID, sha256Hex("nofam"), "", later, ErrInvalidCommand},
		{"unknown user", DefaultTenantID, "nobody", sha256Hex("unknown"), "fam", later, store.ErrNotFound},
		{"disabled user", DefaultTenantID, "disabled", sha256Hex("disabled"), "fam", later, ErrInvalidCommand},
		{"missing tenant", "", active.ID, sha256Hex("notenant"), "fam", later, ErrInvalidCommand},
		{"valid", DefaultTenantID, active.ID, sha256Hex("valid"), "fam-1", later, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := NewCreateSession("create-"+tt.name, tt.tenant, tt.user, tt.hash, tt.family, tt.expires)
			res := r.apply(cmd, now)
			if (tt.wantErr == nil) != (res.Err == nil) || (tt.wantErr != nil && !errors.Is(res.Err, tt.wantErr)) {
				t.Fatalf("error = %v, want %v", res.Err, tt.wantErr)
			}
		})
	}

	u, err := r.st.GetUser(t.Context(), DefaultTenantID, active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.LastLoginMs != now {
		t.Errorf("LastLoginMs = %d, want %d", u.LastLoginMs, now)
	}
	if s := r.session(t, "valid"); s.FamilyID != "fam-1" || s.UserID != active.ID || s.RevokedMs != 0 {
		t.Errorf("session = %+v", s)
	}
}

func TestPasswordOrDisableRevokesSessions(t *testing.T) {
	t.Parallel()
	r := newReplica(t)
	u := r.createUser("u1", userDoc("pw-user", "tenant-editor"))
	other := r.createUser("u2", userDoc("disable-user", "tenant-editor"))
	role := r.createUser("u3", userDoc("role-user", "tenant-editor"))
	for _, tok := range []struct{ id, user string }{{"pw-session", u.ID}, {"dis-session", other.ID}, {"role-session", role.ID}} {
		fam := tok.id + "-fam"
		if res := r.apply(NewCreateSession("cs-"+tok.id, DefaultTenantID, tok.user, sha256Hex(tok.id), fam, 9_000_000), 5000); res.Err != nil {
			t.Fatal(res.Err)
		}
	}

	// A new password ends the sessions of the user.
	doc := userDoc("pw-user", "tenant-editor")
	doc.PasswordHash = fakeArgon2Hash + "x"
	if res := r.apply(must(NewPutUser("pw", DefaultTenantID, u.ID, doc, u.Revision)), 6000); res.Err != nil {
		t.Fatalf("password change: %v", res.Err)
	}
	if s := r.session(t, "pw-session"); s.RevokedMs != 6000 {
		t.Errorf("session after password change: revoked at %d, want 6000", s.RevokedMs)
	}

	// Disabling ends the sessions of the user.
	doc = userDoc("disable-user", "tenant-editor")
	doc.Disabled = true
	if res := r.apply(must(NewPutUser("dis", DefaultTenantID, other.ID, doc, other.Revision)), 6001); res.Err != nil {
		t.Fatalf("disable: %v", res.Err)
	}
	if s := r.session(t, "dis-session"); s.RevokedMs != 6001 {
		t.Errorf("session after disable: revoked at %d, want 6001", s.RevokedMs)
	}

	// A role change keeps the sessions.
	doc = userDoc("role-user", "tenant-admin")
	if res := r.apply(must(NewPutUser("role", DefaultTenantID, role.ID, doc, role.Revision)), 6002); res.Err != nil {
		t.Fatalf("role change: %v", res.Err)
	}
	if s := r.session(t, "role-session"); s.RevokedMs != 0 {
		t.Errorf("session after role change revoked at %d, want none", s.RevokedMs)
	}
}

func TestRotateSessionResults(t *testing.T) {
	t.Parallel()
	r := newReplica(t)
	u := r.createUser("rot-user", userDoc("rot", "tenant-viewer"))

	create := func(id string, at, expires int64) {
		t.Helper()
		if res := r.apply(NewCreateSession("cs-"+id, DefaultTenantID, u.ID, sha256Hex(id), "fam-"+id, expires), at); res.Err != nil {
			t.Fatal(res.Err)
		}
	}
	rotate := func(old, next string, at, expires int64) SessionResult {
		t.Helper()
		res := r.apply(NewRotateSession(sha256Hex(old), sha256Hex(next), expires), at)
		if res.Err != nil {
			t.Fatalf("rotate %s: %v", old, res.Err)
		}
		var out SessionResult
		if err := json.Unmarshal(res.Result, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	create("A", 5000, far)
	if got := rotate("A", "B", 5100, far); got.Status != SessionRotated || got.UserID != u.ID || got.TenantID != DefaultTenantID {
		t.Errorf("rotate A->B = %+v, want rotated", got)
	}
	if got := rotate("A", "C", 5200, far); got.Status != SessionReused {
		t.Errorf("reuse of A = %+v, want reused", got)
	}
	if s := r.session(t, "B"); s.RevokedMs == 0 {
		t.Error("family not revoked after reuse")
	}
	if got := rotate("B", "D", 5300, far); got.Status != SessionRevoked {
		t.Errorf("rotate of revoked B = %+v, want revoked", got)
	}

	create("E", 6000, 7000)
	if got := rotate("E", "F", 8000, far); got.Status != SessionExpired {
		t.Errorf("rotate of expired E = %+v, want expired", got)
	}

	if res := r.apply(NewRotateSession(sha256Hex("unknown"), sha256Hex("next"), far), 9000); !errors.Is(res.Err, store.ErrNotFound) {
		t.Errorf("rotate of unknown token = %v, want ErrNotFound", res.Err)
	}
	if res := r.apply(NewRotateSession(sha256Hex("A"), "short", far), 9001); !errors.Is(res.Err, ErrInvalidCommand) {
		t.Errorf("rotate to short hash = %v, want ErrInvalidCommand", res.Err)
	}
	if res := r.apply(NewRotateSession(sha256Hex("A"), sha256Hex("G"), 9000), 9002); !errors.Is(res.Err, ErrInvalidCommand) {
		t.Errorf("rotate to expired successor = %v, want ErrInvalidCommand", res.Err)
	}
}

func TestRevokeSessions(t *testing.T) {
	t.Parallel()
	r := newReplica(t)
	u := r.createUser("rev-user", userDoc("rev", "tenant-viewer"))
	other := r.createUser("rev-other", userDoc("rev-other", "tenant-viewer"))
	for _, s := range []struct{ id, user, fam string }{
		{"s1", u.ID, "fam-1"}, {"s2", u.ID, "fam-2"}, {"s3", other.ID, "fam-3"},
	} {
		if res := r.apply(NewCreateSession("cs-"+s.id, DefaultTenantID, s.user, sha256Hex(s.id), s.fam, far), 7000); res.Err != nil {
			t.Fatal(res.Err)
		}
	}

	if res := r.apply(NewRevokeFamily("rf", "fam-1"), 7100); res.Err != nil {
		t.Fatalf("revoke family: %v", res.Err)
	}
	if s := r.session(t, "s1"); s.RevokedMs != 7100 {
		t.Errorf("s1 after family revoke: revoked at %d, want 7100", s.RevokedMs)
	}
	if s := r.session(t, "s2"); s.RevokedMs != 0 {
		t.Errorf("s2 revoked by a revoke of fam-1")
	}

	if res := r.apply(NewRevokeUserSessions("ru", DefaultTenantID, u.ID), 7200); res.Err != nil {
		t.Fatalf("revoke user: %v", res.Err)
	}
	if s := r.session(t, "s2"); s.RevokedMs != 7200 {
		t.Errorf("s2 after user revoke: revoked at %d, want 7200", s.RevokedMs)
	}
	if s := r.session(t, "s3"); s.RevokedMs != 0 {
		t.Errorf("session of another user revoked")
	}

	invalid := []struct {
		name string
		cmd  *pb.Command
	}{
		{"neither family nor user", &pb.Command{Version: CommandVersion, RequestId: "rv-1", TenantId: DefaultTenantID,
			Payload: &pb.Command_RevokeSessions{RevokeSessions: &pb.RevokeSessions{}}}},
		{"family and user", &pb.Command{Version: CommandVersion, RequestId: "rv-2", TenantId: DefaultTenantID,
			Payload: &pb.Command_RevokeSessions{RevokeSessions: &pb.RevokeSessions{FamilyId: "fam-3", UserId: other.ID}}}},
		{"user without tenant", NewRevokeUserSessions("rv-3", "", other.ID)},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			if res := r.apply(tt.cmd, 7300); !errors.Is(res.Err, ErrInvalidCommand) {
				t.Errorf("error = %v, want ErrInvalidCommand", res.Err)
			}
		})
	}
	if s := r.session(t, "s3"); s.RevokedMs != 0 {
		t.Error("rejected revoke changed a session")
	}
}

// TestSessionCommandsAreDeterministic applies the same log to two replicas and
// compares their state (FA-CLU-02).
func TestSessionCommandsAreDeterministic(t *testing.T) {
	t.Parallel()
	var prints []string
	for range 2 {
		r := newReplica(t)
		u := r.createUser("det-user", userDoc("det", "tenant-admin"))
		steps := []struct {
			cmd *pb.Command
			at  int64
		}{
			{NewCreateSession("d1", DefaultTenantID, u.ID, sha256Hex("d1"), "fd", far), 8000},
			{NewRotateSession(sha256Hex("d1"), sha256Hex("d2"), far), 8100},
			{NewRotateSession(sha256Hex("d1"), sha256Hex("d3"), far), 8200},
			{NewRotateSession(sha256Hex("d2"), sha256Hex("d4"), far), 8300},
			{NewCreateSession("d5", DefaultTenantID, "missing", sha256Hex("d5"), "fx", far), 8400},
			{NewRevokeUserSessions("d6", DefaultTenantID, u.ID), 8500},
		}
		for _, s := range steps {
			r.apply(s.cmd, s.at)
		}
		fp, err := r.st.Fingerprint(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		prints = append(prints, fp)
	}
	if prints[0] != prints[1] {
		t.Fatalf("replicas diverged: %s vs %s", prints[0], prints[1])
	}
}

const far = int64(10_000_000)

// lastAdminAction removes or weakens a user in the way the guard covers.
type lastAdminAction struct {
	name  string
	build func(u store.User) *pb.Command
}

func adminActions() []lastAdminAction {
	return []lastAdminAction{
		{"demote", func(u store.User) *pb.Command {
			return must(NewPutUser("demote-"+u.ID, DefaultTenantID, u.ID, userDoc(u.Username, "tenant-admin"), u.Revision))
		}},
		{"disable", func(u store.User) *pb.Command {
			doc := userDoc(u.Username, platformAdmin)
			doc.Disabled = true
			return must(NewPutUser("disable-"+u.ID, DefaultTenantID, u.ID, doc, u.Revision))
		}},
		{"delete", func(u store.User) *pb.Command {
			return NewDeleteUser("delete-"+u.ID, DefaultTenantID, u.ID)
		}},
	}
}

// TestLastPlatformAdminIsKept: the last enabled platform admin of a tenant
// cannot be demoted, disabled or deleted (FA-AUTH-09).
func TestLastPlatformAdminIsKept(t *testing.T) {
	t.Parallel()
	for _, act := range adminActions() {
		t.Run("single admin "+act.name, func(t *testing.T) {
			t.Parallel()
			r := newReplica(t)
			only := r.createUser("only", userDoc("only", platformAdmin))
			res := r.apply(act.build(only), 2000)
			if !errors.Is(res.Err, store.ErrConflict) {
				t.Fatalf("%s of last admin = %v, want ErrConflict", act.name, res.Err)
			}
			got := r.user(t, only.ID)
			if got.Role != platformAdmin || got.Disabled || got.Revision != only.Revision {
				t.Errorf("user changed by rejected %s: %+v", act.name, got)
			}
		})
	}

	t.Run("second admin keeps the tenant covered", func(t *testing.T) {
		for _, act := range adminActions() {
			r := newReplica(t)
			first := r.createUser("first", userDoc("first", platformAdmin))
			second := r.createUser("second", userDoc("second", platformAdmin))
			if res := r.apply(act.build(first), 2000); res.Err != nil {
				t.Fatalf("%s of first admin with a second one: %v", act.name, res.Err)
			}
			// The first admin is gone for this action; the second one is now the last.
			if res := r.apply(act.build(second), 2001); !errors.Is(res.Err, store.ErrConflict) {
				t.Errorf("%s of now-last admin = %v, want ErrConflict", act.name, res.Err)
			}
			if got := r.user(t, second.ID); got.Role != platformAdmin || got.Disabled {
				t.Errorf("last admin changed by rejected %s: %+v", act.name, got)
			}
		}
	})

	t.Run("disabled admin does not count", func(t *testing.T) {
		t.Parallel()
		r := newReplica(t)
		enabled := r.createUser("enabled", userDoc("enabled", platformAdmin))
		disabledDoc := userDoc("disabled", platformAdmin)
		disabledDoc.Disabled = true
		r.createUser("disabled", disabledDoc)
		if res := r.apply(adminActions()[2].build(enabled), 2000); !errors.Is(res.Err, store.ErrConflict) {
			t.Errorf("delete of only enabled admin = %v, want ErrConflict", res.Err)
		}
		// A disabled admin is not the last one, so it may be removed.
		disabled := r.user(t, "disabled")
		if res := r.apply(adminActions()[2].build(disabled), 2001); res.Err != nil {
			t.Errorf("delete of disabled admin = %v, want nil", res.Err)
		}
	})

	t.Run("tenant admin is not affected", func(t *testing.T) {
		t.Parallel()
		r := newReplica(t)
		r.createUser("root", userDoc("root", platformAdmin))
		ta := r.createUser("ta", userDoc("ta", "tenant-admin"))
		if res := r.apply(adminActions()[0].build(ta), 2000); res.Err != nil {
			t.Errorf("demote of tenant admin = %v, want nil", res.Err)
		}
		ta = r.user(t, ta.ID)
		if res := r.apply(adminActions()[2].build(ta), 2001); res.Err != nil {
			t.Errorf("delete of tenant admin = %v, want nil", res.Err)
		}
	})

	t.Run("deleting a missing user", func(t *testing.T) {
		t.Parallel()
		r := newReplica(t)
		r.createUser("root", userDoc("root", platformAdmin))
		if res := r.apply(NewDeleteUser("del-missing", DefaultTenantID, "nobody"), 2000); !errors.Is(res.Err, store.ErrNotFound) {
			t.Errorf("delete of missing user = %v, want ErrNotFound", res.Err)
		}
	})
}

// user returns a user of the default tenant or fails the test.
func (r *replica) user(t *testing.T, id string) store.User {
	t.Helper()
	u, err := r.st.GetUser(t.Context(), DefaultTenantID, id)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
