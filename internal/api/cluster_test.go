// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/joinsider/uptimecluster/internal/api/gen"
	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/clusterpki"
	"github.com/joinsider/uptimecluster/internal/membership"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/pki"
	"github.com/joinsider/uptimecluster/internal/replication"
	"github.com/joinsider/uptimecluster/internal/store"
)

// fakeClusterAdmin records the calls and returns the configured results.
type fakeClusterAdmin struct {
	token    string
	expires  time.Time
	tokenErr error

	gotRole   pki.Role
	gotTenant string
	gotTTL    time.Duration
	tokenCall int

	removal    membership.Removal
	removeErr  error
	removedID  string
	removedBy  string
	removeCall int

	members    []cluster.Member
	membersErr error
	leaderID   string
	selfID     string
}

func (f *fakeClusterAdmin) CreateJoinToken(_ context.Context, role pki.Role, tenantID string, ttl time.Duration) (string, time.Time, error) {
	f.tokenCall++
	f.gotRole, f.gotTenant, f.gotTTL = role, tenantID, ttl
	return f.token, f.expires, f.tokenErr
}

func (f *fakeClusterAdmin) Remove(_ context.Context, nodeID, requestedBy string) (membership.Removal, error) {
	f.removeCall++
	f.removedID = nodeID
	f.removedBy = requestedBy
	return f.removal, f.removeErr
}

func (f *fakeClusterAdmin) Members() ([]cluster.Member, uint64, error) {
	return f.members, 1, f.membersErr
}

func (f *fakeClusterAdmin) Leader() (string, string) { return "n1:9443", f.leaderID }

func (f *fakeClusterAdmin) ID() string { return f.selfID }

const (
	platformAdmin = "default:platform-admin"
	tenantAdmin   = "default:tenant-admin"
	tenantEditor  = "default:tenant-editor"
	tenantViewer  = "default:tenant-viewer"
)

// clusterEnv is an env whose cluster administration is the given fake.
func clusterEnv(t *testing.T, admin ClusterAdmin) *env {
	t.Helper()
	return newEnvTweaked(t, false, func(d *Deps) { d.ClusterAdmin = admin })
}

func problemDetail(t *testing.T, r response) string {
	t.Helper()
	p := decode[gen.Problem](t, r)
	if p.Detail == nil {
		return ""
	}
	return *p.Detail
}

func TestClusterAdminRequiresPlatformAdmin(t *testing.T) {
	fake := &fakeClusterAdmin{token: "t", expires: time.Now().Add(time.Hour), selfID: "n1", leaderID: "n1"}
	e := clusterEnv(t, fake)
	ops := []struct {
		name, method, path string
		body               any
	}{
		{"create join token", "POST", "/api/v1/admin/join-tokens", map[string]any{"role": "voter"}},
		{"list nodes", "GET", "/api/v1/admin/nodes", nil},
		{"remove node", "DELETE", "/api/v1/admin/nodes/n2", nil},
	}
	for _, o := range ops {
		for _, p := range []string{tenantAdmin, tenantEditor, tenantViewer} {
			res := e.do(o.method, o.path, p, o.body)
			if res.code != http.StatusForbidden {
				t.Errorf("%s as %s = %d, want 403: %s", o.name, p, res.code, res.body)
			}
		}
	}
	if fake.tokenCall+fake.removeCall != 0 {
		t.Errorf("forbidden callers reached the cluster: token=%d remove=%d", fake.tokenCall, fake.removeCall)
	}
	for _, o := range ops {
		res := e.do(o.method, o.path, platformAdmin, o.body)
		if res.code == http.StatusForbidden || res.code == http.StatusUnauthorized {
			t.Errorf("%s as platform-admin = %d: %s", o.name, res.code, res.body)
		}
	}
}

func TestCreateJoinTokenSuccess(t *testing.T) {
	expires := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	fake := &fakeClusterAdmin{token: "secret-token-value", expires: expires}
	e := clusterEnv(t, fake)

	res := e.do("POST", "/api/v1/admin/join-tokens", platformAdmin,
		map[string]any{"role": "probe", "tenantId": "default", "ttlMinutes": 15})
	if res.code != http.StatusCreated {
		t.Fatalf("create = %d %s", res.code, res.body)
	}
	got := decode[gen.JoinToken](t, res)
	if got.Token != "secret-token-value" || !got.ExpiresAt.Equal(expires) || got.Role != gen.JoinTokenRole("probe") {
		t.Errorf("body = %+v", got)
	}
	if got.TenantId == nil || *got.TenantId != "default" {
		t.Errorf("tenantId = %v, want default", got.TenantId)
	}
	if fake.gotRole != pki.RoleProbe || fake.gotTenant != "default" || fake.gotTTL != 15*time.Minute {
		t.Errorf("authority got role=%q tenant=%q ttl=%s", fake.gotRole, fake.gotTenant, fake.gotTTL)
	}
	if cc := res.hdr.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

func TestCreateJoinTokenDefaultTTL(t *testing.T) {
	fake := &fakeClusterAdmin{token: "t", expires: time.Now().Add(time.Hour)}
	e := clusterEnv(t, fake)
	res := e.do("POST", "/api/v1/admin/join-tokens", platformAdmin, map[string]any{"role": "voter"})
	if res.code != http.StatusCreated {
		t.Fatalf("create = %d %s", res.code, res.body)
	}
	if fake.gotTTL != time.Hour {
		t.Errorf("ttl = %s, want 1h", fake.gotTTL)
	}
	if got := decode[gen.JoinToken](t, res); got.TenantId != nil {
		t.Errorf("voter token has tenantId %v", *got.TenantId)
	}
}

func TestCreateJoinTokenValidation(t *testing.T) {
	cases := []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"unknown role", map[string]any{"role": "admin"}, "/role"},
		{"ttl zero", map[string]any{"role": "voter", "ttlMinutes": 0}, "/ttlMinutes"},
		{"ttl above one day", map[string]any{"role": "voter", "ttlMinutes": 1441}, "/ttlMinutes"},
		{"tenant on voter", map[string]any{"role": "voter", "tenantId": "default"}, "/tenantId"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeClusterAdmin{token: "t", expires: time.Now()}
			e := clusterEnv(t, fake)
			res := e.do("POST", "/api/v1/admin/join-tokens", platformAdmin, c.body)
			if res.code != http.StatusUnprocessableEntity {
				t.Fatalf("= %d, want 422: %s", res.code, res.body)
			}
			p := decode[gen.Problem](t, res)
			found := false
			if p.Errors != nil {
				for _, f := range *p.Errors {
					found = found || f.Field == c.field
				}
			}
			if !found {
				t.Errorf("no field error for %s: %s", c.field, res.body)
			}
			if fake.tokenCall != 0 {
				t.Errorf("invalid request reached the authority")
			}
		})
	}
}

func TestCreateJoinTokenErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
	}{
		{"no CA yet", clusterpki.ErrNoCA, http.StatusServiceUnavailable},
		{"not leader", membership.ErrNotLeader, http.StatusServiceUnavailable},
		{"not applied", fmt.Errorf("wrapped: %w", cluster.ErrNotApplied), http.StatusServiceUnavailable},
		{"unknown outcome", cluster.ErrUnknownOutcome, http.StatusServiceUnavailable},
		{"unexpected", errors.New("disk on fire"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := clusterEnv(t, &fakeClusterAdmin{tokenErr: c.err})
			res := e.do("POST", "/api/v1/admin/join-tokens", platformAdmin, map[string]any{"role": "voter"})
			if res.code != c.code {
				t.Fatalf("= %d, want %d: %s", res.code, c.code, res.body)
			}
			if c.code == http.StatusServiceUnavailable && res.hdr.Get("Retry-After") != "1" {
				t.Errorf("Retry-After = %q, want 1", res.hdr.Get("Retry-After"))
			}
			if strings.Contains(string(res.body), "disk on fire") {
				t.Errorf("internal error leaked: %s", res.body)
			}
		})
	}
}

func TestRemoveClusterNode(t *testing.T) {
	t.Run("removed with warning", func(t *testing.T) {
		fake := &fakeClusterAdmin{removal: membership.Removal{VotersLeft: 2, Warning: "even number of voters"}}
		e := clusterEnv(t, fake)
		res := e.do("DELETE", "/api/v1/admin/nodes/n2", platformAdmin, nil)
		if res.code != http.StatusOK {
			t.Fatalf("= %d %s", res.code, res.body)
		}
		got := decode[gen.NodeRemoval](t, res)
		if got.VotersLeft != 2 || got.Warning == nil || *got.Warning != "even number of voters" {
			t.Errorf("body = %+v", got)
		}
		if fake.removedID != "n2" {
			t.Errorf("removed %q, want n2", fake.removedID)
		}
		if fake.removedBy == "" {
			t.Error("the removal does not name the requesting admin")
		}
	})
	t.Run("removed without warning", func(t *testing.T) {
		e := clusterEnv(t, &fakeClusterAdmin{removal: membership.Removal{VotersLeft: 3}})
		res := e.do("DELETE", "/api/v1/admin/nodes/n2", platformAdmin, nil)
		if res.code != http.StatusOK {
			t.Fatalf("= %d %s", res.code, res.body)
		}
		if got := decode[gen.NodeRemoval](t, res); got.Warning != nil {
			t.Errorf("warning = %q, want none", *got.Warning)
		}
	})
	errCases := []struct {
		name string
		err  error
		code int
	}{
		{"unknown node", membership.ErrUnknownNode, http.StatusNotFound},
		{"leader itself", membership.ErrRemoveLeader, http.StatusConflict},
		{"last voter", membership.ErrLastVoter, http.StatusConflict},
		{"not leader", membership.ErrNotLeader, http.StatusServiceUnavailable},
	}
	for _, c := range errCases {
		t.Run(c.name, func(t *testing.T) {
			e := clusterEnv(t, &fakeClusterAdmin{removeErr: c.err})
			res := e.do("DELETE", "/api/v1/admin/nodes/n2", platformAdmin, nil)
			if res.code != c.code {
				t.Fatalf("= %d, want %d: %s", res.code, c.code, res.body)
			}
			if c.code == http.StatusServiceUnavailable && res.hdr.Get("Retry-After") != "1" {
				t.Errorf("Retry-After = %q, want 1", res.hdr.Get("Retry-After"))
			}
			if c.code == http.StatusConflict && problemDetail(t, res) == "" {
				t.Errorf("409 without explanation")
			}
		})
	}
}

// voters builds n voters plus one non-voter; n1 leads and answers.
func voters(n int) []cluster.Member {
	var out []cluster.Member
	for i := 1; i <= n; i++ {
		out = append(out, cluster.Member{ID: fmt.Sprintf("n%d", i), Address: fmt.Sprintf("n%d:9443", i), Voter: true})
	}
	return append(out, cluster.Member{ID: "p1", Address: "p1:9443", Voter: false})
}

func TestListClusterNodes(t *testing.T) {
	t.Run("two voters warn about an even count", func(t *testing.T) {
		e := clusterEnv(t, &fakeClusterAdmin{members: voters(2), leaderID: "n1", selfID: "n2"})
		res := e.do("GET", "/api/v1/admin/nodes", platformAdmin, nil)
		if res.code != http.StatusOK {
			t.Fatalf("= %d %s", res.code, res.body)
		}
		got := decode[gen.ClusterNodeList](t, res)
		if got.LeaderId != "n1" || len(got.Nodes) != 3 {
			t.Fatalf("list = %+v", got)
		}
		for _, n := range got.Nodes {
			if n.Leader != (n.Id == "n1") || n.Self != (n.Id == "n2") {
				t.Errorf("node %s: leader=%v self=%v", n.Id, n.Leader, n.Self)
			}
		}
		if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "even") {
			t.Errorf("warnings = %v, want one even-count warning", got.Warnings)
		}
	})
	t.Run("six voters warn twice", func(t *testing.T) {
		e := clusterEnv(t, &fakeClusterAdmin{members: voters(6), leaderID: "n1", selfID: "n1"})
		got := decode[gen.ClusterNodeList](t, e.do("GET", "/api/v1/admin/nodes", platformAdmin, nil))
		if len(got.Warnings) != 2 {
			t.Fatalf("warnings = %v, want even and more-than-five", got.Warnings)
		}
		if !strings.Contains(got.Warnings[0], "even") || !strings.Contains(got.Warnings[1], "more than 5") {
			t.Errorf("warnings = %v", got.Warnings)
		}
	})
	t.Run("three voters have no warnings", func(t *testing.T) {
		e := clusterEnv(t, &fakeClusterAdmin{members: voters(3), leaderID: "n1", selfID: "n1"})
		res := e.do("GET", "/api/v1/admin/nodes", platformAdmin, nil)
		if !strings.Contains(string(res.body), `"warnings":[]`) {
			t.Errorf("warnings must be an empty array: %s", res.body)
		}
	})
}

func TestClusterAdminUnavailableWithoutHost(t *testing.T) {
	e := newEnv(t) // no cluster administration in these deps
	for _, o := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/v1/admin/join-tokens", map[string]any{"role": "voter"}},
		{"GET", "/api/v1/admin/nodes", nil},
		{"DELETE", "/api/v1/admin/nodes/n2", nil},
	} {
		res := e.do(o.method, o.path, platformAdmin, o.body)
		if res.code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503: %s", o.method, o.path, res.code, res.body)
		}
		if d := problemDetail(t, res); d != "cluster administration is not available" {
			t.Errorf("detail = %q", d)
		}
	}
}

// fakeReplication returns the configured peer states.
type fakeReplication []replication.PeerStatus

func (f fakeReplication) Status() []replication.PeerStatus { return f }

// FA-REP-05: the cluster listing shows how far each peer's heartbeats lag and
// the last replication error; the answering node and peers that were never
// caught up carry neither.
func TestListClusterNodesReplicationLag(t *testing.T) {
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	admin := &fakeClusterAdmin{members: voters(3), leaderID: "n1", selfID: "n1"}
	statuses := fakeReplication{
		{ID: "n1", Address: "n1:9443", CaughtUpAt: now.Add(-time.Second)}, // self: ignored
		{ID: "n2", Address: "n2:9443", Connected: true, CaughtUpAt: now.Add(-5 * time.Second)},
		{ID: "n3", Address: "n3:9443", LastError: "dial: connection refused"}, // never caught up
	}
	tests := []struct {
		name       string
		replicator fakeReplication
		wantLag    map[string]int64
		wantErr    map[string]string
	}{
		{
			name:       "lag and errors per peer",
			replicator: statuses,
			wantLag:    map[string]int64{"n2": 5000},
			wantErr:    map[string]string{"n3": "dial: connection refused"},
		},
		{
			name:       "without replication nothing is reported",
			replicator: nil,
			wantLag:    map[string]int64{},
			wantErr:    map[string]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnvTweaked(t, false, func(d *Deps) {
				d.ClusterAdmin = admin
				d.Now = func() time.Time { return now }
				if tt.replicator != nil {
					d.Replication = tt.replicator
				}
			})
			got := decode[gen.ClusterNodeList](t, e.do("GET", "/api/v1/admin/nodes", platformAdmin, nil))
			if len(got.Nodes) != 4 {
				t.Fatalf("nodes = %d, want 4", len(got.Nodes))
			}
			for _, n := range got.Nodes {
				lag, hasLag := tt.wantLag[n.Id]
				if hasLag != (n.ReplicationLagMs != nil) || (hasLag && *n.ReplicationLagMs != lag) {
					t.Errorf("node %s: lag = %v, want %v", n.Id, n.ReplicationLagMs, lag)
				}
				msg, hasErr := tt.wantErr[n.Id]
				if hasErr != (n.ReplicationError != nil) || (hasErr && *n.ReplicationError != msg) {
					t.Errorf("node %s: error = %v, want %q", n.Id, n.ReplicationError, msg)
				}
			}
		})
	}
}

// ADR 0020: the listing shows the clock offset of each peer with a fresh
// sample; the answering node and peers without one carry none.
func TestListClusterNodesClockOffset(t *testing.T) {
	admin := &fakeClusterAdmin{members: voters(3), leaderID: "n1", selfID: "n1"}
	offsets := map[string]int64{"n1": 5, "n2": 90_000, "n3": -2_300}
	e := newEnvTweaked(t, false, func(d *Deps) {
		d.ClusterAdmin = admin
		d.ClockOffsets = func() map[string]int64 { return offsets }
	})
	got := decode[gen.ClusterNodeList](t, e.do("GET", "/api/v1/admin/nodes", platformAdmin, nil))
	byID := map[string]gen.ClusterNode{}
	for _, n := range got.Nodes {
		byID[n.Id] = n
	}
	if n := byID["n1"]; n.ClockOffsetMs != nil {
		t.Errorf("answering node has offset %d, want none", *n.ClockOffsetMs)
	}
	if n := byID["n2"]; n.ClockOffsetMs == nil || *n.ClockOffsetMs != 90_000 {
		t.Errorf("n2 offset = %v, want 90000", n.ClockOffsetMs)
	}
	if n := byID["n3"]; n.ClockOffsetMs == nil || *n.ClockOffsetMs != -2_300 {
		t.Errorf("n3 offset = %v, want -2300", n.ClockOffsetMs)
	}

	// Without a fresh sample for n2 it is absent.
	delete(offsets, "n2")
	got = decode[gen.ClusterNodeList](t, e.do("GET", "/api/v1/admin/nodes", platformAdmin, nil))
	for _, n := range got.Nodes {
		if n.Id == "n2" && n.ClockOffsetMs != nil {
			t.Errorf("n2 offset = %d without a sample, want none", *n.ClockOffsetMs)
		}
	}
}

// TestListClusterNodesRegistry checks that the registry fields are shown for
// registered nodes only (ADR 0017).
func TestListClusterNodesRegistry(t *testing.T) {
	e := clusterEnv(t, &fakeClusterAdmin{members: voters(2), leaderID: "n1", selfID: "n1"})
	cmd := cluster.NewRegisterNode("register-n1", store.Node{ID: "n1", Name: "node-one", Role: store.NodeRoleVoter,
		Version: "1.2.3", Capabilities: []string{"icmp"}})
	if _, err := e.node.Apply(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	got := decode[gen.ClusterNodeList](t, e.do("GET", "/api/v1/admin/nodes", platformAdmin, nil))
	byID := map[string]gen.ClusterNode{}
	for _, n := range got.Nodes {
		byID[n.Id] = n
	}

	n1 := byID["n1"]
	if n1.Name == nil || *n1.Name != "node-one" || n1.Version == nil || *n1.Version != "1.2.3" {
		t.Errorf("n1 name/version = %v/%v", n1.Name, n1.Version)
	}
	if n1.Capabilities == nil || !slices.Equal(*n1.Capabilities, []string{"icmp"}) {
		t.Errorf("n1 capabilities = %v", n1.Capabilities)
	}
	if n1.Healthy == nil || !*n1.Healthy || n1.HealthChangedAt == nil {
		t.Errorf("n1 health = %v at %v", n1.Healthy, n1.HealthChangedAt)
	}
	n2 := byID["n2"]
	if n2.Name != nil || n2.Version != nil || n2.Capabilities != nil || n2.Healthy != nil || n2.HealthChangedAt != nil {
		t.Errorf("unregistered n2 has registry fields: %+v", n2)
	}
}

// TestListClusterNodesCommandVersions covers ADR 0018 section 7: the registry
// version of each node, the active command level and whether voters differ.
func TestListClusterNodesCommandVersions(t *testing.T) {
	ctx := context.Background()
	register := func(e *env, id string, maxVersion uint32) {
		t.Helper()
		cmd := cluster.NewRegisterNode("register-"+id, store.Node{ID: id, Name: id, Role: store.NodeRoleVoter,
			Version: "1.0.0", MaxCommandVersion: maxVersion})
		if _, err := e.node.Apply(ctx, cmd); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name        string
		register    map[string]uint32 // voter ID -> max command version
		level       uint32            // stored command level, 0 = unset
		wantLevel   int32
		wantPending bool
	}{
		{
			name:        "all voters on the same version",
			register:    map[string]uint32{"n1": 2, "n2": 2},
			wantLevel:   2,
			wantPending: false,
		},
		{
			name:        "one voter still on version 2",
			register:    map[string]uint32{"n1": 3, "n2": 2},
			wantLevel:   2,
			wantPending: true,
		},
		{
			name:        "stored level is shown",
			register:    map[string]uint32{"n1": 3, "n2": 3},
			level:       3,
			wantLevel:   3,
			wantPending: false,
		},
		{
			name:        "no registered voter means no pending upgrade",
			wantLevel:   2,
			wantPending: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := clusterEnv(t, &fakeClusterAdmin{members: voters(2), leaderID: "n1", selfID: "n1"})
			for _, id := range []string{"n1", "n2"} {
				if v, ok := tt.register[id]; ok {
					register(e, id, v)
				}
			}
			if tt.level > 0 {
				if err := e.state.Update(ctx, func(tx store.StateTx) error { return tx.SetCommandLevel(ctx, tt.level) }); err != nil {
					t.Fatal(err)
				}
			}
			got := decode[gen.ClusterNodeList](t, e.do("GET", "/api/v1/admin/nodes", platformAdmin, nil))
			if got.CommandLevel == nil || *got.CommandLevel != tt.wantLevel {
				t.Errorf("commandLevel = %v, want %d", got.CommandLevel, tt.wantLevel)
			}
			if got.UpgradePending == nil || *got.UpgradePending != tt.wantPending {
				t.Errorf("upgradePending = %v, want %v", got.UpgradePending, tt.wantPending)
			}
			for _, n := range got.Nodes {
				want, registered := tt.register[n.Id]
				if !registered {
					if n.MaxCommandVersion != nil {
						t.Errorf("unregistered node %s has maxCommandVersion %d", n.Id, *n.MaxCommandVersion)
					}
					continue
				}
				if n.MaxCommandVersion == nil || int64(*n.MaxCommandVersion) != int64(want) {
					t.Errorf("node %s maxCommandVersion = %v, want %d", n.Id, n.MaxCommandVersion, want)
				}
			}
		})
	}
}

// TestCommandNotEnabledIs503 covers ADR 0018 section 4: a command that waits
// for a rolling update to finish is answered with 503 and a retry hint.
func TestCommandNotEnabledIs503(t *testing.T) {
	notEnabled := &cluster.CommandError{Code: cluster.CodeNotEnabled, Message: "requires command version 3; upgrade all voters first"}

	t.Run("apply", func(t *testing.T) {
		e := newEnvTweaked(t, false, func(d *Deps) { d.Cluster = fakeApplier{err: notEnabled} })
		res := e.do("POST", "/api/v1/monitors", tenantEditor, httpMonitor("late", "https://example.com"))
		if res.code != http.StatusServiceUnavailable {
			t.Fatalf("= %d, want 503: %s", res.code, res.body)
		}
		if res.hdr.Get("Retry-After") != "30" {
			t.Errorf("Retry-After = %q, want 30", res.hdr.Get("Retry-After"))
		}
		if d := problemDetail(t, res); d != notEnabled.Message {
			t.Errorf("detail = %q, want %q", d, notEnabled.Message)
		}
	})

	t.Run("cluster administration", func(t *testing.T) {
		e := clusterEnv(t, &fakeClusterAdmin{tokenErr: notEnabled, selfID: "n1", leaderID: "n1"})
		res := e.do("POST", "/api/v1/admin/join-tokens", platformAdmin, map[string]any{"role": "voter"})
		if res.code != http.StatusServiceUnavailable {
			t.Fatalf("= %d, want 503: %s", res.code, res.body)
		}
		if res.hdr.Get("Retry-After") != "30" {
			t.Errorf("Retry-After = %q, want 30", res.hdr.Get("Retry-After"))
		}
	})
}

// fakeApplier returns the configured error for every command.
type fakeApplier struct{ err error }

func (f fakeApplier) Apply(context.Context, *pb.Command) (cluster.ApplyResult, error) {
	return cluster.ApplyResult{}, f.err
}

// TestListClusterNodesSSRFProtection checks that each node shows its SSRF
// setting (ADR 0030) and that a node without a recent answer shows none.
func TestListClusterNodesSSRFProtection(t *testing.T) {
	admin := &fakeClusterAdmin{members: voters(3), leaderID: "n1", selfID: "n1"}
	ssrf := map[string]bool{"n1": true, "n2": false}
	e := newEnvTweaked(t, false, func(d *Deps) {
		d.ClusterAdmin = admin
		d.SSRFProtectionByNode = func() map[string]bool { return ssrf }
	})
	got := decode[gen.ClusterNodeList](t, e.do("GET", "/api/v1/admin/nodes", platformAdmin, nil))
	byID := map[string]gen.ClusterNode{}
	for _, n := range got.Nodes {
		byID[n.Id] = n
	}
	if n := byID["n1"]; n.SsrfProtection == nil || !*n.SsrfProtection {
		t.Errorf("n1 ssrfProtection = %v, want true", n.SsrfProtection)
	}
	if n := byID["n2"]; n.SsrfProtection == nil || *n.SsrfProtection {
		t.Errorf("n2 ssrfProtection = %v, want false", n.SsrfProtection)
	}
	if n := byID["n3"]; n.SsrfProtection != nil {
		t.Errorf("n3 ssrfProtection = %v without an answer, want none", *n.SsrfProtection)
	}
}
