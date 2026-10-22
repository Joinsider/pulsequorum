// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package clusterhost

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/clusternet"
	"github.com/joinsider/uptimecluster/internal/membership"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/pki"
	"github.com/joinsider/uptimecluster/internal/secrets"
	"github.com/joinsider/uptimecluster/internal/store/sqlite"
)

type testNode struct {
	id      string
	addr    string
	dir     string
	state   *sqlite.StateStore
	kek     *secrets.KEK
	keyring *secrets.Keyring
	host    *Host
	cancel  context.CancelFunc
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

// startNode assembles a voter like the binary does (cmd/uptimecluster).
func startNode(ctx context.Context, t *testing.T, seeds []string, token string) (*testNode, error) {
	t.Helper()
	return launch(ctx, t, &testNode{id: uuid.NewString(), addr: freeAddr(t), dir: t.TempDir()}, seeds, token)
}

// restart stops n and starts it again from the same data directory under a
// new address (FA-CLU-10).
func restart(ctx context.Context, t *testing.T, n *testNode) *testNode {
	t.Helper()
	n.cancel()
	if err := n.host.Close(); err != nil {
		t.Logf("close: %v", err)
	}
	_ = n.state.Close()
	moved, err := launch(ctx, t, &testNode{id: n.id, addr: freeAddr(t), dir: n.dir}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	return moved
}

func launch(ctx context.Context, t *testing.T, n *testNode, seeds []string, token string) (*testNode, error) {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	existing, err := cluster.HasState(n.dir)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare(ctx, n.dir, n.id, "n", n.addr, "test", JoinOptions{Seeds: seeds, Token: token}, log)
	if err != nil {
		return nil, err
	}
	if n.kek, err = secrets.LoadKEK(n.dir, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := prepared.CheckKEK(n.kek); err != nil {
		t.Fatal(err)
	}
	if n.state, err = sqlite.OpenState(ctx, n.dir); err != nil {
		t.Fatal(err)
	}
	n.keyring = secrets.NewKeyring(n.kek, n.state.ListSecretKeys)
	n.host, err = Start(ctx, Options{
		NodeID: n.id, ListenAddr: n.addr, Advertise: n.addr, DataDir: n.dir, Timeout: 300 * time.Millisecond,
		State: n.state, KEK: n.kek, Keyring: n.keyring, Prepared: prepared, Log: log, RaftLogLevel: "error",
		OnFatal: func(err error) { t.Errorf("node halted: %v", err) },
	})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	n.cancel = cancel
	go n.host.Run(runCtx)
	t.Cleanup(func() {
		cancel()
		_ = n.host.Close()
		_ = n.state.Close()
	})
	if !prepared.Joining && !existing {
		if err := cluster.EnsureDefaultTenant(ctx, n.host.Node, n.state); err != nil {
			t.Fatal(err)
		}
		if err := cluster.EnsureSecretKey(ctx, n.host.Node, n.state, n.kek); err != nil {
			t.Fatal(err)
		}
		if err := n.keyring.Reload(ctx); err != nil {
			t.Fatal(err)
		}
		if err := n.host.Bootstrap(ctx); err != nil {
			t.Fatal(err)
		}
	}
	return n, nil
}

func eventually(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func isVoter(n *testNode, id string) bool {
	members, _, err := n.host.Node.Members()
	if err != nil {
		return false
	}
	for _, m := range members {
		if m.ID == id {
			return m.Voter
		}
	}
	return false
}

func memberCount(n *testNode) int {
	members, _, _ := n.host.Node.Members()
	return len(members)
}

// FA-CLU-03/04/08/10, FA-PKI-01..04: 1 → 3 voters over mTLS, removal with
// revocation.
func TestThreeVotersJoinOverMTLS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	n1, err := startNode(ctx, t, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "n1 leader", 10*time.Second, n1.host.Node.IsLeader)

	token := func(role pki.Role) string {
		tok, _, err := n1.host.Authority.CreateJoinToken(ctx, role, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}

	// An invalid token is rejected at once, without retries.
	bad := token(pki.RoleVoter)
	tok, _ := pki.ParseJoinToken(bad)
	forged := pki.JoinToken{ID: tok.ID, Secret: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Fingerprint: tok.Fingerprint}
	if _, err := startNode(ctx, t, []string{n1.addr}, forged.String()); !errors.Is(err, membership.ErrJoinRejected) {
		t.Fatalf("forged token: err = %v", err)
	}

	// n2 joins through the leader, n3 through the follower n2 (forwarding).
	n2, err := startNode(ctx, t, []string{n1.addr}, token(pki.RoleVoter))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "n2 promoted to voter", 30*time.Second, func() bool { return isVoter(n1, n2.id) })
	n3, err := startNode(ctx, t, []string{n2.addr}, token(pki.RoleVoter))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "n3 promoted to voter", 30*time.Second, func() bool { return isVoter(n1, n3.id) })

	// Every voter has the cluster's master key and the replicated state.
	for _, n := range []*testNode{n2, n3} {
		if n.kek.Fingerprint != n1.kek.Fingerprint {
			t.Errorf("node %s has another master key", n.id)
		}
	}
	cmd, _ := cluster.NewPutTenant("t-acme", "acme", cluster.TenantDocument{Name: "ACME", Plan: "default"})
	if _, err := n1.host.Node.Apply(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	for _, n := range []*testNode{n2, n3} {
		eventually(t, "tenant replicated", 10*time.Second, func() bool {
			_, err := n.state.GetTenant(ctx, "acme")
			return err == nil
		})
		eventually(t, "data keys replicated", 10*time.Second, func() bool { return n.keyring.Reload(ctx) == nil && n.keyring.ActiveKeyID() != "" })
	}

	// n2 moves to a new address; the cluster follows and n2 stays a voter.
	n2 = restart(ctx, t, n2)
	eventually(t, "n2 address updated", 30*time.Second, func() bool {
		members, _, _ := n1.host.Node.Members()
		for _, m := range members {
			if m.ID == n2.id {
				return m.Address == n2.addr && m.Voter
			}
		}
		return false
	})
	cmd2, _ := cluster.NewPutTenant("t-moved", "moved", cluster.TenantDocument{Name: "Moved", Plan: "default"})
	if _, err := n1.host.Node.Apply(ctx, cmd2); err != nil {
		t.Fatal(err)
	}
	eventually(t, "replication after the move", 15*time.Second, func() bool {
		_, err := n2.state.GetTenant(ctx, "moved")
		return err == nil
	})

	// A follower renews its certificate through the leader (FA-PKI-03).
	first := pki.SerialString(n2.host.Cert.Leaf())
	if err := n2.host.Cert.Renew(ctx, n2.host.Cert.Leaf().NotAfter, n2.host.issuer()); err != nil {
		t.Fatalf("renew via leader: %v", err)
	}
	if pki.SerialString(n2.host.Cert.Leaf()) == first {
		t.Error("follower certificate not renewed")
	}

	// A new node under n3's address is rejected with n3's ID (FA-CLU-08).
	conflict := token(pki.RoleVoter)
	_, err = membership.Join(ctx, membership.JoinOptions{
		Seeds: []string{n1.addr}, Token: conflict, NodeID: uuid.NewString(), Address: n3.addr,
		Cert: n3.host.Cert, Log: slog.New(slog.DiscardHandler),
	})
	if !errors.Is(err, membership.ErrJoinRejected) || !containsID(err, n3.id) {
		t.Errorf("address conflict: err = %v", err)
	}

	// Removal is manual and revokes the node (FA-CLU-08, FA-PKI-04).
	if _, err := n1.host.Manager.Remove(ctx, n1.id); !errors.Is(err, membership.ErrRemoveLeader) {
		t.Errorf("leader removing itself: err = %v", err)
	}
	removal, err := n1.host.Manager.Remove(ctx, n3.id)
	if err != nil {
		t.Fatal(err)
	}
	if removal.VotersLeft != 2 || removal.Warning == "" {
		t.Errorf("removal = %+v, want a warning for 2 voters", removal)
	}
	eventually(t, "n3 out of the configuration", 10*time.Second, func() bool { return memberCount(n1) == 2 })
	if err := n1.host.Revocations.Reload(ctx, n1.state); err != nil {
		t.Fatal(err)
	}
	conn, err := clusternet.DialVoter(n1.addr, n3.host.sec)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	callCtx, callCancel := context.WithTimeout(ctx, 5*time.Second)
	defer callCancel()
	if _, err := pb.NewClusterServiceClient(conn).GetStatus(callCtx, &pb.GetStatusRequest{}); err == nil {
		t.Error("removed node still reaches the cluster port")
	}
}

func containsID(err error, id string) bool {
	return err != nil && id != "" && strings.Contains(err.Error(), id)
}
