// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package clusterhost

import (
	"context"
	"crypto/x509"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/clusternet"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/pki"
)

// skewClock is a node clock that runs a fixed offset ahead of the system
// clock and can be moved while the node runs (ADR 0020).
type skewClock struct{ offset atomic.Int64 } // nanoseconds

func newSkewClock(d time.Duration) *skewClock {
	c := &skewClock{}
	c.set(d)
	return c
}

func (c *skewClock) set(d time.Duration) { c.offset.Store(int64(d)) }

func (c *skewClock) now() time.Time { return time.Now().Add(time.Duration(c.offset.Load())) }

// startNodeWithClock is startNode for a voter whose wall clock is clock. The
// clock offsets are measured every 200 ms, so the test does not wait 15 s.
func startNodeWithClock(ctx context.Context, t *testing.T, seeds []string, token string, clock func() time.Time) (*testNode, error) {
	t.Helper()
	return launch(ctx, t, &testNode{id: uuid.NewString(), addr: freeAddr(t), dir: t.TempDir(), clock: clock,
		clockInterval: 200 * time.Millisecond}, seeds, token)
}

// withinMs reports whether got is within tolerance of want, in milliseconds.
func withinMs(got, want, tolerance int64) bool {
	d := got - want
	return d >= -tolerance && d <= tolerance
}

// ADR 0020, point 3: three voters with clocks at +0 s, +90 s and -90 s form a
// cluster, every voter measures the others' offsets, and the cluster keeps
// working when one clock moves to +6 min. The test also records which
// certificates still verify at which clock.
//
// Findings (asserted below):
//   - the issuer backdates NotBefore by pki.Backdate (5 min) on its own clock;
//     a verifier rejects a certificate while its own clock is before NotBefore,
//     that is when it is more than 5 min behind the issuer. A verifier ahead of
//     the issuer accepts it;
//   - so a voter at +6 min cannot be a problem for existing certificates or for
//     certificates issued by a voter at 0 (the leader in this test): both verify;
//   - a certificate issued by the +6 min voter is rejected by the voter at 0
//     ("not yet valid"), while the voter at +90 s accepts it. A cluster with
//     such a leader would therefore lose the voter at 0 on the mTLS port for
//     certificates issued by that leader. The test verifies the certificates
//     directly with the verifier; no leadership change is forced.
func TestClockSkewAcrossVoters(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	clockA, clockB, clockC := newSkewClock(0), newSkewClock(90*time.Second), newSkewClock(-90*time.Second)
	n1, err := startNodeWithClock(ctx, t, nil, "", clockA.now)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "n1 leader", 10*time.Second, n1.host.Node.IsLeader)
	token := func() string {
		tok, _, err := n1.host.Authority.CreateJoinToken(ctx, pki.RoleVoter, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}

	// n2 (+90 s) joins through the leader, n3 (-90 s) through n2, so the join
	// of n3 is also forwarded.
	n2, err := startNodeWithClock(ctx, t, []string{n1.addr}, token(), clockB.now)
	if err != nil {
		t.Fatalf("join with +90 s: %v", err)
	}
	eventually(t, "n2 promoted to voter", 30*time.Second, func() bool { return isVoter(n1, n2.id) })
	n3, err := startNodeWithClock(ctx, t, []string{n2.addr}, token(), clockC.now)
	if err != nil {
		t.Fatalf("join with -90 s: %v", err)
	}
	eventually(t, "n3 promoted to voter", 30*time.Second, func() bool { return isVoter(n1, n3.id) })
	nodes := []*testNode{n1, n2, n3}

	// Writes through each voter are visible on every voter.
	for i, n := range nodes {
		tenant := fmt.Sprintf("skew-%d", i)
		cmd, _ := cluster.NewPutTenant("write-"+tenant, tenant, cluster.TenantDocument{Name: tenant, Plan: "default"})
		if _, err := n.host.Node.Apply(ctx, cmd); err != nil {
			t.Fatalf("write via voter %d: %v", i+1, err)
		}
		for _, m := range nodes {
			eventually(t, tenant+" replicated", 10*time.Second, func() bool {
				_, err := m.state.GetTenant(ctx, tenant)
				return err == nil
			})
		}
	}

	// The offsets measured by n1 for n2 (+90 s) and n3 (-90 s), as the API
	// reports them (Host.ClockOffsets feeds ListClusterNodes).
	eventually(t, "offsets of n2 and n3 measured on n1", 20*time.Second, func() bool {
		off := n1.host.ClockOffsets()
		b, okB := off[n2.id]
		c, okC := off[n3.id]
		return okB && okC && withinMs(b, 90_000, 1_000) && withinMs(c, -90_000, 1_000)
	})
	off := n1.host.ClockOffsets()
	t.Logf("offsets on n1 (leader): n2 %d ms, n3 %d ms", off[n2.id], off[n3.id])

	// Every voter measures, not only the leader: n3 sees n1 at +90 s and n2
	// at +180 s relative to its own clock.
	eventually(t, "offsets measured on n3", 20*time.Second, func() bool {
		off := n3.host.ClockOffsets()
		a, okA := off[n1.id]
		b, okB := off[n2.id]
		return okA && okB && withinMs(a, 90_000, 1_000) && withinMs(b, 180_000, 1_000)
	})
	t.Logf("offsets on n3: n1 %d ms, n2 %d ms", n3.host.ClockOffsets()[n1.id], n3.host.ClockOffsets()[n2.id])

	// n3's clock moves to +6 min while the cluster runs. Its existing
	// certificate and new connections keep working; n1 sees the new offset.
	clockC.set(6 * time.Minute)
	eventually(t, "n1 sees n3 at +6 min", 20*time.Second, func() bool {
		c, ok := n1.host.ClockOffsets()[n3.id]
		return ok && withinMs(c, 360_000, 1_000)
	})
	t.Logf("after the move: n1 measures n3 at %d ms", n1.host.ClockOffsets()[n3.id])

	// Fresh connections in both directions authenticate with the existing
	// certificates; the status answer carries n3's clock.
	for _, pair := range [][2]*testNode{{n3, n1}, {n1, n3}} {
		from, to := pair[0], pair[1]
		conn, err := clusternet.DialVoter(to.addr, from.host.sec)
		if err != nil {
			t.Fatalf("%s -> %s: %v", from.id, to.id, err)
		}
		callCtx, callCancel := context.WithTimeout(ctx, 5*time.Second)
		st, err := pb.NewClusterServiceClient(conn).GetStatus(callCtx, &pb.GetStatusRequest{})
		callCancel()
		_ = conn.Close()
		if err != nil {
			t.Fatalf("GetStatus %s -> %s with the existing certificate: %v", from.id, to.id, err)
		}
		if to.id == n3.id {
			t.Logf("GetStatus answered by n3 reports its clock at %s", time.UnixMilli(st.GetWallClockUnixMs()).UTC().Format(time.RFC3339Nano))
		}
	}

	// Writes through the skewed voter still reach the cluster.
	cmd, _ := cluster.NewPutTenant("write-after-skew", "skew-after", cluster.TenantDocument{Name: "after", Plan: "default"})
	if _, err := n3.host.Node.Apply(ctx, cmd); err != nil {
		t.Fatalf("write via the +6 min voter: %v", err)
	}
	for _, m := range []*testNode{n1, n2} {
		eventually(t, "write of the +6 min voter replicated", 10*time.Second, func() bool {
			_, err := m.state.GetTenant(ctx, "skew-after")
			return err == nil
		})
	}

	// A new certificate of n3 issued by the leader n1 (renewal through the
	// leader, FA-PKI-03). n1 backdates by 5 min on its own clock.
	_, csr, err := n3.host.Cert.NewCSR()
	if err != nil {
		t.Fatal(err)
	}
	issued, err := n3.host.issuer()(ctx, csr)
	if err != nil {
		t.Fatalf("renewal of the +6 min voter through the leader: %v", err)
	}
	// A follower's answer carries the chain only; the leaf is its first entry.
	leaf, err := x509.ParseCertificate(issued.ChainDER[0])
	if err != nil {
		t.Fatal(err)
	}
	nb := leaf.NotBefore
	if d := nb.Sub(clockA.now().Add(-pki.Backdate)); d < -2*time.Second || d > 2*time.Second {
		t.Errorf("NotBefore %s is not the leader clock minus %s", nb, pki.Backdate)
	}
	usage := []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	if _, err := n3.host.sec.Verifier.Verify([]*x509.Certificate{leaf}, usage[0]); err != nil {
		t.Errorf("leader-issued certificate at the +6 min voter: %v", err)
	}
	if _, err := n1.host.sec.Verifier.Verify([]*x509.Certificate{leaf}, usage[0]); err != nil {
		t.Errorf("leader-issued certificate at the leader: %v", err)
	}

	// A certificate issued by a voter whose clock is 6 min ahead of the
	// leader's is not yet valid for the leader (NotBefore = issuer -5 min).
	ahead, err := n3.host.Authority.IssueSelf(ctx, n3.id, csr)
	if err != nil {
		t.Fatalf("issue at +6 min: %v", err)
	}
	if _, err := n1.host.sec.Verifier.Verify([]*x509.Certificate{ahead.Leaf}, usage[0]); err == nil || !strings.Contains(err.Error(), "not yet valid") {
		t.Errorf("certificate of a voter 6 min ahead at the leader: err = %v, want not yet valid", err)
	} else {
		t.Logf("certificate issued at +6 min is rejected by the leader: %v", err)
	}
	if _, err := n2.host.sec.Verifier.Verify([]*x509.Certificate{ahead.Leaf}, usage[0]); err != nil {
		t.Errorf("certificate of a voter 6 min ahead at n2 (+90 s): %v", err)
	}

	// The reverse case: the leader's certificate verified by a voter 6 min
	// behind the leader is not yet valid, as NotBefore is leader -5 min.
	behind := *n1.host.sec.Verifier
	behind.Now = func() time.Time { return time.Now().Add(-6 * time.Minute) }
	if _, err := behind.Verify([]*x509.Certificate{leaf}, usage[0]); err == nil || !strings.Contains(err.Error(), "not yet valid") {
		t.Errorf("certificate of the leader at a clock 6 min behind: err = %v, want not yet valid", err)
	}
}
