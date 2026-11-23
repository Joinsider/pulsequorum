// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/api/gen"
	"github.com/joinsider/pulsequorum/internal/auth"
	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/cluster"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/store"
)

func seedPrivateProbesTestNodes(t *testing.T, e *env) {
	t.Helper()
	ctx := context.Background()
	if err := e.state.Update(ctx, func(tx store.StateTx) error {
		return tx.SetCommandLevel(ctx, cluster.ProbeCommandLevel)
	}); err != nil {
		t.Fatal(err)
	}
	cmds := []*pb.Command{
		cluster.NewRegisterNode("reg-v1", store.Node{
			ID: "voter-1", Name: "v-eu", Role: store.NodeRoleVoter,
			Healthy: true, Capabilities: []string{"icmp"},
		}),
		cluster.NewRegisterProbe("reg-pub", store.Node{
			ID: "pub-1", Name: "pub-us", Role: store.NodeRoleProbe,
			Region: "us-east", Healthy: true, Capabilities: []string{"http"}, Version: "1.0.0",
		}),
		cluster.NewRegisterProbe("reg-priv-a", store.Node{
			ID: "priv-a1", Name: "corp-a", Role: store.NodeRoleProbe, OwnerTenantID: "default",
			Region: "dc-a", Healthy: false, HealthChangedMs: 1_700_000_000_000,
			Capabilities: []string{"http"}, Version: "1.2.3",
		}),
		cluster.NewRegisterProbe("reg-priv-b", store.Node{
			ID: "priv-b1", Name: "corp-b", Role: store.NodeRoleProbe, OwnerTenantID: "tenant-b",
			Region: "dc-b", Healthy: true, Capabilities: []string{"http"}, Version: "1.2.3",
		}),
	}
	for _, cmd := range cmds {
		if _, err := e.node.Apply(ctx, cmd); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPrivateProbeEndpointsAuthzAndAPITokenBlock(t *testing.T) {
	t.Parallel()
	fake := &fakeClusterAdmin{token: "secret-tok", expires: time.Now().Add(time.Hour), selfID: "voter-1", leaderID: "voter-1"}
	e := newEnvTweaked(t, true, func(d *Deps) {
		d.ClusterAdmin = fake
		d.Authenticator = auth.WithAPITokens(d.Authenticator, d.State, time.Now)
	})
	raiseCommandLevel(t, e, cluster.ProbeCommandLevel)

	setupRes := e.setup("root-admin", testPassword)
	if setupRes.code != http.StatusCreated {
		t.Fatalf("setup = %d %s", setupRes.code, setupRes.body)
	}
	adminBearer := bearer(decode[gen.TokenResponse](t, setupRes).AccessToken)
	tok := createTokenRes(t, e, adminBearer, "automation", "tenant-admin")

	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/v1/probes/join-tokens", nil},
		{"GET", "/api/v1/probes", nil},
		{"DELETE", "/api/v1/probes/priv-a1", nil},
	} {
		// API tokens are rejected with 403 on all /probes* endpoints.
		if res := e.do(tc.method, tc.path, "", tc.body, bearer(tok.Token)...); res.code != http.StatusForbidden {
			t.Errorf("API token %s %s = %d, want 403: %s", tc.method, tc.path, res.code, res.body)
		}
	}

	// API tokens CAN read GET /nodes (tagged under monitors).
	if res := e.do("GET", "/api/v1/nodes", "", nil, bearer(tok.Token)...); res.code != http.StatusOK {
		t.Errorf("API token GET /nodes = %d, want 200: %s", res.code, res.body)
	}

	// Interactive viewer & editor are rejected with 403 on /probes*.
	e2 := newEnvTweaked(t, false, func(d *Deps) { d.ClusterAdmin = fake })
	seedPrivateProbesTestNodes(t, e2)
	for _, principal := range []string{"default:tenant-viewer", "default:tenant-editor"} {
		for _, tc := range []struct{ method, path string }{
			{"POST", "/api/v1/probes/join-tokens"},
			{"GET", "/api/v1/probes"},
			{"DELETE", "/api/v1/probes/priv-a1"},
		} {
			if res := e2.do(tc.method, tc.path, principal, nil); res.code != http.StatusForbidden {
				t.Errorf("%s %s %s = %d, want 403", principal, tc.method, tc.path, res.code)
			}
		}
		if res := e2.do("GET", "/api/v1/nodes", principal, nil); res.code != http.StatusOK {
			t.Errorf("%s GET /nodes = %d, want 200", principal, res.code)
		}
	}
}

func TestPrivateProbeCRUDAndIsolation(t *testing.T) {
	t.Parallel()
	fake := &fakeClusterAdmin{token: "one-time-tok", expires: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)}
	var lastMsgMs int64 = 1_700_000_050_000
	e := newEnvTweaked(t, false, func(d *Deps) {
		d.ClusterAdmin = fake
		d.ProbeSessions = func() map[string]ProbeSessionView {
			return map[string]ProbeSessionView{
				"priv-a1": {
					ConnectedVoterID: "voter-1",
					LastMessageMs:    lastMsgMs,
					Buffer:           &pb.ProbeBufferStatus{Bytes: 512, Unacked: 3, DroppedTotal: 1},
				},
			}
		}
	})
	seedPrivateProbesTestNodes(t, e)
	ctx := context.Background()

	// Raise probe alert on priv-a1.
	if _, err := e.node.Apply(ctx, cluster.NewSetProbeAlert("default", "priv-a1", true, "voter-1")); err != nil {
		t.Fatal(err)
	}

	// GET /probes lists only default's private probe with alert + session info and no voter/address leak.
	res := e.do("GET", "/api/v1/probes", "default:tenant-admin", nil)
	if res.code != http.StatusOK {
		t.Fatalf("GET /probes = %d: %s", res.code, res.body)
	}
	probes := decode[[]gen.PrivateProbe](t, res)
	if len(probes) != 1 || probes[0].Id != "priv-a1" || probes[0].Name != "corp-a" {
		t.Fatalf("GET /probes = %+v, want [priv-a1]", probes)
	}
	if probes[0].OfflineAlert == nil || !*probes[0].OfflineAlert {
		t.Errorf("offlineAlert = %v, want true", probes[0].OfflineAlert)
	}
	if probes[0].LastContactAt == nil || probes[0].LastContactAt.UnixMilli() != lastMsgMs {
		t.Errorf("lastContactAt = %v, want %d", probes[0].LastContactAt, lastMsgMs)
	}
	if probes[0].ProbeBuffer == nil || probes[0].ProbeBuffer.Bytes != 512 || probes[0].ProbeBuffer.Unacked != 3 {
		t.Errorf("probeBuffer = %+v", probes[0].ProbeBuffer)
	}
	for _, forbiddenField := range []string{"connectedVoterId", "address", "ownerTenantId"} {
		if strings.Contains(string(res.body), forbiddenField) {
			t.Errorf("GET /probes leaked field %q in %s", forbiddenField, res.body)
		}
	}

	// GET /nodes exposes voters, public probes, and own private probes; never foreign private probes.
	resNodes := e.do("GET", "/api/v1/nodes", "default:tenant-viewer", nil)
	if resNodes.code != http.StatusOK {
		t.Fatalf("GET /nodes = %d: %s", resNodes.code, resNodes.body)
	}
	nodes := decode[[]gen.AssignableNode](t, resNodes)
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.Id)
		if n.Id == "priv-b1" {
			t.Fatalf("foreign private probe leaked in GET /nodes: %+v", n)
		}
		if n.Id == "priv-a1" && !n.Private {
			t.Errorf("own private probe has Private=false")
		}
		if (n.Id == "voter-1" || n.Id == "pub-1") && n.Private {
			t.Errorf("public node %s has Private=true", n.Id)
		}
	}
	for _, want := range []string{"voter-1", "pub-1", "priv-a1"} {
		if !slices.Contains(ids, want) {
			t.Errorf("GET /nodes missing %s in %v", want, ids)
		}
	}

	// POST /probes/join-tokens with empty body uses default TTL 60m, passes USER actor, and sets no-store.
	tokRes := e.do("POST", "/api/v1/probes/join-tokens", "default:tenant-admin", nil)
	if tokRes.code != http.StatusCreated {
		t.Fatalf("POST /probes/join-tokens = %d: %s", tokRes.code, tokRes.body)
	}
	if cc := tokRes.hdr.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if fake.gotRole != pki.RoleProbe || fake.gotTenant != "default" || fake.gotTTL != time.Hour {
		t.Errorf("fakeClusterAdmin got role=%q tenant=%q ttl=%v", fake.gotRole, fake.gotTenant, fake.gotTTL)
	}
	if fake.gotActor == nil || fake.gotActor.GetKind() != pb.Actor_KIND_USER {
		t.Errorf("fakeClusterAdmin got actor=%v, want KIND_USER", fake.gotActor)
	}

	// Validation of ttlMinutes out of bounds.
	badTTL := e.do("POST", "/api/v1/probes/join-tokens", "default:tenant-admin", map[string]any{"ttlMinutes": 0})
	if badTTL.code != http.StatusUnprocessableEntity {
		t.Errorf("ttlMinutes=0 status = %d, want 422", badTTL.code)
	}

	// DELETE /probes/{foreignOrPublic} -> 404 zero-oracle.
	for _, forbiddenID := range []string{"priv-b1", "pub-1", "voter-1", "does-not-exist"} {
		delRes := e.do("DELETE", "/api/v1/probes/"+forbiddenID, "default:tenant-admin", nil)
		if delRes.code != http.StatusNotFound {
			t.Errorf("DELETE /probes/%s = %d, want 404", forbiddenID, delRes.code)
		}
	}

	// DELETE /probes/{own} -> 204 and removes + revokes the probe.
	delOwn := e.do("DELETE", "/api/v1/probes/priv-a1", "default:tenant-admin", nil)
	if delOwn.code != http.StatusNoContent {
		t.Fatalf("DELETE /probes/priv-a1 = %d: %s", delOwn.code, delOwn.body)
	}
	after := e.do("GET", "/api/v1/probes", "default:tenant-admin", nil)
	if len(decode[[]gen.PrivateProbe](t, after)) != 0 {
		t.Fatalf("expected priv-a1 removed after DELETE")
	}
}

func TestPrivateProbeLimitAndUsage(t *testing.T) {
	t.Parallel()
	fake := &fakeClusterAdmin{token: "tok", expires: time.Now().Add(time.Hour)}
	e := newEnvTweaked(t, false, func(d *Deps) { d.ClusterAdmin = fake })
	seedPrivateProbesTestNodes(t, e)
	ctx := context.Background()

	// Set maxPrivateProbes = 2 on default tenant; priv-a1 already counts as 1.
	updCmd, err := cluster.NewPutTenant("upd-lim", "default", cluster.TenantDocument{
		Name:   "Default",
		Plan:   "default",
		Limits: []byte(`{"maxPrivateProbes":2}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.node.Apply(ctx, updCmd); err != nil {
		t.Fatal(err)
	}

	// Insert one open (unused, unexpired) private probe join token -> count becomes 2.
	putTok, err := cluster.NewPutJoinToken("req-tok-open", "tok-open", cluster.JoinTokenDocument{
		SecretHash: strings.Repeat("a", 64), Role: string(pki.RoleProbe), TenantID: "default",
		ExpiresMs: time.Now().Add(time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	putTok.Actor = &pb.Actor{Kind: pb.Actor_KIND_USER, Id: "u-admin", Name: "Admin"}
	if _, err := e.node.Apply(ctx, putTok); err != nil {
		t.Fatal(err)
	}

	// GET /tenant reports privateProbes = 2 and exceeded = empty.
	tenRes := e.do("GET", "/api/v1/tenant", "default:tenant-viewer", nil)
	if tenRes.code != http.StatusOK {
		t.Fatalf("GET /tenant = %d: %s", tenRes.code, tenRes.body)
	}
	own := decode[gen.OwnTenant](t, tenRes)
	if own.Usage.PrivateProbes == nil || *own.Usage.PrivateProbes != 2 {
		t.Fatalf("usage.privateProbes = %v, want 2", own.Usage.PrivateProbes)
	}
	if len(own.Exceeded) != 0 {
		t.Errorf("exceeded = %+v, want empty", own.Exceeded)
	}

	// Creating another token must fail with 403 (limit pre-check).
	over := e.do("POST", "/api/v1/probes/join-tokens", "default:tenant-admin", nil)
	if over.code != http.StatusForbidden {
		t.Fatalf("over limit POST /probes/join-tokens = %d, want 403: %s", over.code, over.body)
	}

	// Lower limit to 1 -> GET /tenant reports LimitExcess for maxPrivateProbes.
	lowCmd, _ := cluster.NewPutTenant("upd-low", "default", cluster.TenantDocument{
		Name: "Default", Plan: "default", Limits: []byte(`{"maxPrivateProbes":1}`),
	})
	if _, err := e.node.Apply(ctx, lowCmd); err != nil {
		t.Fatal(err)
	}
	tenLow := decode[gen.OwnTenant](t, e.do("GET", "/api/v1/tenant", "default:tenant-viewer", nil))
	if len(tenLow.Exceeded) != 1 || tenLow.Exceeded[0].Limit != gen.MaxPrivateProbes || tenLow.Exceeded[0].Allowed != 1 || tenLow.Exceeded[0].Actual != 2 {
		t.Errorf("exceeded = %+v, want maxPrivateProbes(1/2)", tenLow.Exceeded)
	}
}

func TestMonitorAssignmentD2AndSSRFRelaxation(t *testing.T) {
	t.Parallel()
	e := newEnvTweaked(t, false, func(d *Deps) { d.SSRFPolicy = checker.BlockPrivate{} })
	seedPrivateProbesTestNodes(t, e)

	// D2: foreign private probe gets the exact same "unknown node" validation error as a bogus ID.
	foreignRes := e.do("POST", "/api/v1/monitors", editor, map[string]any{
		"name": "m", "type": "http", "intervalSeconds": 60,
		"config":     map[string]any{"url": "https://example.com"},
		"assignment": map[string]any{"mode": "nodes", "nodes": []string{"priv-b1"}},
	})
	if foreignRes.code != http.StatusUnprocessableEntity {
		t.Fatalf("foreign probe assignment = %d, want 422", foreignRes.code)
	}
	errs := *decode[gen.Problem](t, foreignRes).Errors
	if len(errs) != 1 || errs[0].Field != "/assignment/nodes/0" || errs[0].Message != "unknown node" {
		t.Errorf("foreign probe error = %+v, want unknown node zero-oracle", errs)
	}

	// SSRF blocked when using internal URL with mode=all, count, voter, public probe, or mixed.
	internalCfg := map[string]any{"url": "http://127.0.0.1:8080/health"}
	for _, tc := range []struct {
		name string
		asgn map[string]any
	}{
		{"all", map[string]any{"mode": "all"}},
		{"count", map[string]any{"mode": "count", "count": 1}},
		{"voter", map[string]any{"mode": "nodes", "nodes": []string{"voter-1"}}},
		{"public-probe", map[string]any{"mode": "nodes", "nodes": []string{"pub-1"}}},
		{"mixed", map[string]any{"mode": "nodes", "nodes": []string{"priv-a1", "pub-1"}}},
	} {
		res := e.do("POST", "/api/v1/monitors", editor, map[string]any{
			"name": tc.name, "type": "http", "intervalSeconds": 60,
			"config": internalCfg, "assignment": tc.asgn,
		})
		if res.code != http.StatusUnprocessableEntity {
			t.Errorf("%s internal URL = %d, want 422: %s", tc.name, res.code, res.body)
		}
	}

	// Accepted ONLY when mode=nodes lists solely own private probes.
	okRes := e.do("POST", "/api/v1/monitors", editor, map[string]any{
		"name": "internal", "type": "http", "intervalSeconds": 60,
		"config":     internalCfg,
		"assignment": map[string]any{"mode": "nodes", "nodes": []string{"priv-a1"}},
	})
	if okRes.code != http.StatusCreated {
		t.Fatalf("own private probe internal URL = %d, want 201: %s", okRes.code, okRes.body)
	}
	created := decode[gen.Monitor](t, okRes)

	// PUT changing assignment from private-only to mode=all re-runs SSRF validation and rejects 422.
	updRes := e.do("PUT", "/api/v1/monitors/"+created.Id.String(), editor, map[string]any{
		"name": "internal", "type": "http", "intervalSeconds": 60,
		"config":     internalCfg,
		"assignment": map[string]any{"mode": "all"},
	})
	if updRes.code != http.StatusUnprocessableEntity {
		t.Errorf("PUT internal monitor to mode=all = %d, want 422: %s", updRes.code, updRes.body)
	}
}

func TestChannelRejectsProbeOfflineOnSystemTenant(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	enableSystemChannels(t, e)
	res := e.do("POST", "/api/v1/admin/system-channels", platformAdmin, map[string]any{
		"name": "sys", "kind": "shoutrrr", "url": shoutrrrURL,
		"events": []string{"system", "probe_offline"},
	})
	if res.code != http.StatusBadRequest {
		t.Errorf("system channel with probe_offline = %d, want 400: %s", res.code, res.body)
	}
}
