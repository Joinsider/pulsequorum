// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package clusterhost

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/pulsequorum/internal/admincli"
	"github.com/joinsider/pulsequorum/internal/auth"
	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/clusternet"
	"github.com/joinsider/pulsequorum/internal/node"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/store"
)

// writeNodeIDFile stores the node ID in dir, as the binary does at first start;
// the admin CLI reads it from there.
func writeNodeIDFile(t *testing.T, dir, id string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, node.IDFile), []byte(id+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// joinToken creates a voter join token on leader.
func joinToken(ctx context.Context, t *testing.T, leader *testNode) string {
	t.Helper()
	tok, _, err := leader.host.Authority.CreateJoinToken(ctx, pki.RoleVoter, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// runAdmin runs "pulsequorum admin <args>" with the environment vars and
// stdin, and returns the exit code and the outputs.
func runAdmin(t *testing.T, vars map[string]string, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	env := admincli.Env{
		Getenv: func(key string) string { return vars[key] },
		Stdin:  strings.NewReader(stdin),
		Stdout: &stdout,
		Stderr: &stderr,
	}
	code := admincli.Run(context.Background(), args, env)
	return code, stdout.String(), stderr.String()
}

// TestRecoverAfterQuorumLoss covers ADR 0019 (FA-CLU-08, FA-PKI-04, PF-06): two
// of three voters are lost, "admin recover" makes the survivor the only voter,
// the old members are revoked once it leads, and a returning old node is
// refused while a fresh node can join.
func TestRecoverAfterQuorumLoss(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	a, err := startNode(ctx, t, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a leader", 10*time.Second, a.host.Node.IsLeader)
	b, err := startNode(ctx, t, []string{a.addr}, joinToken(ctx, t, a))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b voter", 30*time.Second, func() bool { return isVoter(a, b.id) })
	c, err := startNode(ctx, t, []string{a.addr}, joinToken(ctx, t, a))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "c voter", 30*time.Second, func() bool { return isVoter(a, c.id) })

	// Data that must survive the recovery, replicated to all three voters.
	before, _ := cluster.NewPutTenant("t-before-loss", "before", cluster.TenantDocument{Name: "Before", Plan: "default"})
	if _, err := a.host.Node.Apply(ctx, before); err != nil {
		t.Fatal(err)
	}
	for _, n := range []*testNode{b, c} {
		eventually(t, "tenant replicated before the loss", 15*time.Second, func() bool {
			_, err := n.state.GetTenant(ctx, "before")
			return err == nil
		})
	}

	// B and C are lost for good: without a quorum a has no leader.
	stop(t, b)
	stop(t, c)
	eventually(t, "a without leader", 10*time.Second, func() bool {
		addr, _ := a.host.Node.Leader()
		return addr == ""
	})

	// The survivor is stopped and recovered offline with the operator command.
	stop(t, a)
	writeNodeIDFile(t, a.dir, a.id)
	vars := map[string]string{"DATA_DIR": a.dir, "NODE_NAME": "a"}
	code, out, errOut := runAdmin(t, vars, "", "recover", "--yes")
	if code != admincli.ExitOK {
		t.Fatalf("recover: exit %d, want 0; stdout %q, stderr %q", code, out, errOut)
	}
	backups, err := filepath.Glob(filepath.Join(a.dir, "recovery-backup-*"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("backup directories = %v, %v; want exactly one", backups, err)
	}
	revoke, ok, err := cluster.ReadRecoveryRevoke(a.dir)
	if err != nil || !ok {
		t.Fatalf("recovery revocation list: ok=%v, err=%v", ok, err)
	}
	wantRevoked := []string{b.id, c.id}
	slices.Sort(wantRevoked)
	if !slices.Equal(revoke.NodeIDs, wantRevoked) {
		t.Fatalf("revoke list = %v, want %v", revoke.NodeIDs, wantRevoked)
	}

	// The recovered node starts again on a new address and is the only voter.
	a = relaunch(ctx, t, a)
	eventually(t, "a leader alone", 30*time.Second, func() bool {
		return a.host.Node.IsLeader() && memberCount(a) == 1
	})
	if got := tenantCount(t, a); got < 2 {
		t.Errorf("tenants after recovery = %d, want the default tenant and %q", got, "before")
	}
	if _, err := a.state.GetTenant(ctx, "before"); err != nil {
		t.Errorf("data written before the loss is gone: %v", err)
	}
	after, _ := cluster.NewPutTenant("t-after-recovery", "after", cluster.TenantDocument{Name: "After", Plan: "default"})
	if _, err := a.host.Node.Apply(ctx, after); err != nil {
		t.Fatalf("write after recovery: %v", err)
	}

	// Once a leads, it revokes B and C and removes the list (ADR 0019).
	eventually(t, "revocation list removed", 60*time.Second, func() bool {
		_, ok, err := cluster.ReadRecoveryRevoke(a.dir)
		return err == nil && !ok
	})
	revoked, err := a.state.ListRevokedNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{b.id, c.id} {
		if !containsRevokedNode(revoked, id) {
			t.Errorf("node %s not in the revoked nodes of a: %+v", id, revoked)
		}
	}
	reg := registry(t, a)
	for _, id := range []string{b.id, c.id} {
		if _, ok := reg[id]; ok {
			t.Errorf("registry entry of revoked node %s still exists", id)
		}
	}

	// A fresh node joins through a and becomes a voter.
	d, err := startNode(ctx, t, []string{a.addr}, joinToken(ctx, t, a))
	if err != nil {
		t.Fatalf("fresh node join: %v", err)
	}
	eventually(t, "d voter", 60*time.Second, func() bool { return isVoter(a, d.id) })

	// d learns the leader at a's current address; a stale address would make
	// it forward writes to a dead port.
	eventually(t, "d sees a at its current address", 15*time.Second, func() bool {
		addr, _ := d.host.Node.Leader()
		return addr == a.addr
	})

	// The old B comes back with its old data directory. Its certificate is
	// revoked, so the cluster port of a refuses it, and the configuration of
	// a stays a and d.
	oldB, err := launch(ctx, t, &testNode{id: b.id, addr: freeAddr(t), dir: b.dir}, nil, "")
	if err != nil {
		t.Fatalf("restart of the old B: %v", err)
	}
	defer stop(t, oldB)
	eventually(t, "cluster port refuses the revoked B", 20*time.Second, func() bool {
		conn, err := clusternet.DialVoter(a.addr, oldB.host.sec)
		if err != nil {
			return true
		}
		defer func() { _ = conn.Close() }()
		callCtx, callCancel := context.WithTimeout(ctx, 3*time.Second)
		defer callCancel()
		_, err = pb.NewClusterServiceClient(conn).GetStatus(callCtx, &pb.GetStatusRequest{})
		return err != nil
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if !a.host.Node.IsLeader() {
			t.Fatal("a lost leadership after the old B came back")
		}
		members, _, err := a.host.Node.Members()
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(members))
		for _, m := range members {
			ids = append(ids, m.ID)
		}
		slices.Sort(ids)
		want := []string{a.id, d.id}
		slices.Sort(want)
		if !slices.Equal(ids, want) {
			t.Fatalf("configuration = %v, want %v", ids, want)
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// relaunch starts a stopped node again from its data directory under a new
// address. Unlike restart it does not stop the node first.
func relaunch(ctx context.Context, t *testing.T, n *testNode) *testNode {
	t.Helper()
	moved, err := launch(ctx, t, &testNode{id: n.id, addr: freeAddr(t), dir: n.dir, replInterval: n.replInterval, caps: n.caps}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	return moved
}

func containsRevokedNode(revoked []store.RevokedNode, id string) bool {
	for _, r := range revoked {
		if r.NodeID == id {
			return true
		}
	}
	return false
}

// TestResetPasswordThroughFollower covers ADR 0019 (FA-AUTH-03, FA-DAT-01): the
// CLI resets a password next to a follower; the leader applies it, every
// replica stores the new hash, and the sessions of the user end.
func TestResetPasswordThroughFollower(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	a, err := startNode(ctx, t, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a leader", 10*time.Second, a.host.Node.IsLeader)
	b, err := startNode(ctx, t, []string{a.addr}, joinToken(ctx, t, a))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b voter", 30*time.Second, func() bool { return isVoter(a, b.id) })

	// A local platform admin with a refresh session.
	const (
		oldPassword = "initial-admin-pass-1"
		newPassword = "rotated-admin-pass-2"
		username    = "admin"
	)
	userID := uuid.NewString()
	hash, err := auth.HashPassword(oldPassword)
	if err != nil {
		t.Fatal(err)
	}
	put, err := cluster.NewPutUser(uuid.NewString(), cluster.DefaultTenantID, userID, cluster.UserDocument{
		Username: username, DisplayName: "Admin", PasswordHash: hash, Role: "platform-admin",
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.host.Node.Apply(ctx, put); err != nil {
		t.Fatal(err)
	}
	tokenHash := strings.Repeat("ab", 32)
	session := cluster.NewCreateSession(uuid.NewString(), cluster.DefaultTenantID, userID, tokenHash, uuid.NewString(),
		time.Now().Add(time.Hour).UnixMilli())
	if _, err := a.host.Node.Apply(ctx, session); err != nil {
		t.Fatal(err)
	}
	eventually(t, "user and session replicated to b", 15*time.Second, func() bool {
		u, err := b.state.GetUser(ctx, cluster.DefaultTenantID, userID)
		if err != nil {
			return false
		}
		_, err = b.state.GetSession(ctx, tokenHash)
		return err == nil && u.Revision == 1
	})
	oldHash := userHash(ctx, t, b, userID)

	passwordFile := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(passwordFile, []byte(newPassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// b is a follower: the command reaches the leader through a.
	writeNodeIDFile(t, b.dir, b.id)
	follower := map[string]string{"DATA_DIR": b.dir, "CLUSTER_ADVERTISE": b.addr, "AUTH_MODE": "local", "NODE_NAME": "b"}
	code, out, errOut := runAdmin(t, follower, "", "reset-password", "--username", username, "--password-file", passwordFile)
	if code != admincli.ExitOK {
		t.Fatalf("reset-password: exit %d, want 0; stdout %q, stderr %q", code, out, errOut)
	}
	if !strings.Contains(out, "Password of \"admin\" reset") {
		t.Errorf("stdout does not report the reset: %q", out)
	}
	if strings.Contains(out+errOut, newPassword) || strings.Contains(out+errOut, oldPassword) {
		t.Errorf("output contains a password: stdout %q, stderr %q", out, errOut)
	}

	for _, n := range []*testNode{a, b} {
		eventually(t, "new password on "+n.id, 15*time.Second, func() bool {
			ok, _, err := auth.VerifyPassword(newPassword, userHash(ctx, t, n, userID))
			return err == nil && ok
		})
		if ok, _, _ := auth.VerifyPassword(oldPassword, userHash(ctx, t, n, userID)); ok {
			t.Errorf("old password still verifies on %s", n.id)
		}
		eventually(t, "session revoked on "+n.id, 15*time.Second, func() bool {
			sess, err := n.state.GetSession(ctx, tokenHash)
			return err == nil && sess.RevokedMs != 0
		})
	}

	// Error cases leave the user unchanged.
	current := userHash(ctx, t, a, userID)
	if current == oldHash {
		t.Fatal("the password hash did not change after the reset")
	}
	short := filepath.Join(t.TempDir(), "short")
	if err := os.WriteFile(short, []byte("too-short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unknown := map[string]string{"DATA_DIR": b.dir, "CLUSTER_ADVERTISE": b.addr, "AUTH_MODE": "local", "NODE_NAME": "b"}
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantErr  string
	}{
		{"unknown user", []string{"reset-password", "--username", "nobody", "--password-file", passwordFile}, admincli.ExitError, `no local user "nobody"`},
		{"password too short", []string{"reset-password", "--username", username, "--password-file", short}, admincli.ExitError, "12 to 256"},
		{"missing username", []string{"reset-password", "--password-file", passwordFile}, admincli.ExitUsage, "--username is required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errOut := runAdmin(t, unknown, "", tc.args...)
			if code != tc.wantCode {
				t.Errorf("exit %d, want %d; stderr %q", code, tc.wantCode, errOut)
			}
			if !strings.Contains(errOut, tc.wantErr) {
				t.Errorf("stderr lacks %q: %q", tc.wantErr, errOut)
			}
		})
	}
	for _, n := range []*testNode{a, b} {
		if got := userHash(ctx, t, n, userID); got != current {
			t.Errorf("hash on %s changed by a failed reset", n.id)
		}
	}
	// A disabled user stays disabled unless --enable is given (runbook).
	const locked = "locked"
	lockedID := uuid.NewString()
	lockedHash, err := auth.HashPassword(oldPassword)
	if err != nil {
		t.Fatal(err)
	}
	lockedPut, err := cluster.NewPutUser(uuid.NewString(), cluster.DefaultTenantID, lockedID, cluster.UserDocument{
		Username: locked, DisplayName: "Locked", PasswordHash: lockedHash, Role: "tenant-viewer", Disabled: true,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.host.Node.Apply(ctx, lockedPut); err != nil {
		t.Fatal(err)
	}
	eventually(t, "disabled user replicated to b", 15*time.Second, func() bool {
		_, err := b.state.GetUserByUsername(ctx, locked)
		return err == nil
	})
	code, out, errOut = runAdmin(t, follower, "", "reset-password", "--username", locked, "--password-file", passwordFile)
	if code != admincli.ExitOK || !strings.Contains(out, "still disabled") {
		t.Fatalf("reset of a disabled user: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if u := userState(ctx, t, a, lockedID); !u.Disabled {
		t.Error("disabled user was enabled without --enable")
	}
	// The CLI reads the revision from its local replica, so the second reset
	// waits until b has replicated the first one (otherwise: a conflict).
	eventually(t, "first reset replicated to b", 15*time.Second, func() bool {
		return userState(ctx, t, b, lockedID).Revision == userState(ctx, t, a, lockedID).Revision
	})
	code, out, errOut = runAdmin(t, follower, "", "reset-password", "--username", locked, "--password-file", passwordFile, "--enable")
	if code != admincli.ExitOK || !strings.Contains(out, "enabled again") {
		t.Fatalf("reset with --enable: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	eventually(t, "user enabled on a", 15*time.Second, func() bool { return !userState(ctx, t, a, lockedID).Disabled })
}

// userState returns the stored user on n.
func userState(ctx context.Context, t *testing.T, n *testNode, userID string) store.User {
	t.Helper()
	u, err := n.state.GetUser(ctx, cluster.DefaultTenantID, userID)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// userHash returns the password hash of the user as stored on n.
func userHash(ctx context.Context, t *testing.T, n *testNode, userID string) string {
	t.Helper()
	u, err := n.state.GetUser(ctx, cluster.DefaultTenantID, userID)
	if err != nil {
		t.Fatal(err)
	}
	return u.PasswordHash
}
