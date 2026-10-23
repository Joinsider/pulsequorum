// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package replication

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	grpcbackoff "google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/pki"
)

var errUnreachable = errors.New("node unreachable")

// fixedIdentity returns a verified caller with the given node ID, as mTLS would.
func fixedIdentity(nodeID string) func(context.Context) (pki.Identity, bool) {
	return func(context.Context) (pki.Identity, bool) {
		return pki.Identity{NodeID: nodeID, Role: pki.RoleVoter}, true
	}
}

// serveServer runs srv on a bufconn listener and returns a client for it.
func serveServer(t *testing.T, srv *Server) pb.ReplicationServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	pb.RegisterReplicationServiceServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return pb.NewReplicationServiceClient(conn)
}

// testNet connects test nodes through in-memory gRPC servers. Each (from, to)
// pair gets its own server, so the server knows the caller's identity. Nodes
// can be partitioned, and pushes towards a node can be stalled.
type testNet struct {
	t    *testing.T
	base Options

	mu      sync.Mutex
	nodes   map[string]*testNode
	pairs   map[[2]string]*pairServer
	down    map[string]bool
	stalled map[string]bool
}

type testNode struct {
	id      string
	metrics *fakeMetrics
	rep     *Replicator
	peers   []string // guarded by testNet.mu
}

type pairServer struct {
	lis *bufconn.Listener
	srv *grpc.Server
}

// newTestNet creates a network whose nodes use base as replicator options.
func newTestNet(t *testing.T, base Options) *testNet {
	t.Helper()
	if base.PeerRefresh == 0 {
		base.PeerRefresh = 5 * time.Millisecond
	}
	if base.AntiEntropyInterval == 0 {
		base.AntiEntropyInterval = time.Hour
	}
	if base.backoffMin == 0 {
		base.backoffMin = 5 * time.Millisecond
		base.backoffMax = 20 * time.Millisecond
	}
	n := &testNet{
		t:       t,
		base:    base,
		nodes:   make(map[string]*testNode),
		pairs:   make(map[[2]string]*pairServer),
		down:    make(map[string]bool),
		stalled: make(map[string]bool),
	}
	t.Cleanup(n.stopPairs)
	return n
}

// add creates a node with the given stored metrics that pulls from peers.
func (n *testNet) add(id string, m *fakeMetrics, peers ...string) *testNode {
	node := &testNode{id: id, metrics: m, peers: peers}
	opts := n.base
	opts.NodeID = id
	opts.Metrics = m
	opts.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	opts.Peers = func() []Peer {
		n.mu.Lock()
		defer n.mu.Unlock()
		out := make([]Peer, 0, len(node.peers))
		for _, p := range node.peers {
			out = append(out, Peer{ID: p, Address: p})
		}
		return out
	}
	opts.Dial = n.dialer(id)
	node.rep = New(opts)

	n.mu.Lock()
	n.nodes[id] = node
	n.mu.Unlock()
	return node
}

// setPeers changes the peer set of a node at runtime.
func (n *testNet) setPeers(id string, peers ...string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.nodes[id].peers = peers
}

// start runs the replicators of all nodes until the test ends.
func (n *testNet) start() {
	n.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	n.mu.Lock()
	for _, node := range n.nodes {
		wg.Add(1)
		go func(r *Replicator) {
			defer wg.Done()
			r.Run(ctx)
		}(node.rep)
	}
	n.mu.Unlock()
	n.t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
}

// setDown partitions a node: it can neither be dialled nor dial out. Its
// existing servers are stopped, so open streams break.
func (n *testNet) setDown(id string, down bool) {
	var stop []*grpc.Server
	n.mu.Lock()
	n.down[id] = down
	if down {
		for key, ps := range n.pairs {
			if key[0] == id || key[1] == id {
				stop = append(stop, ps.srv)
				delete(n.pairs, key)
			}
		}
	}
	n.mu.Unlock()
	for _, s := range stop {
		s.Stop()
	}
}

// stallPushes makes every push stream towards id block on send forever. The
// sender's buffer fills up, as with a slow peer.
func (n *testNet) stallPushes(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.stalled[id] = true
}

func (n *testNet) dialer(from string) func(string) (*grpc.ClientConn, error) {
	return func(to string) (*grpc.ClientConn, error) {
		n.mu.Lock()
		stalled := n.stalled[to]
		n.mu.Unlock()
		opts := []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithConnectParams(grpc.ConnectParams{
				Backoff: grpcbackoff.Config{BaseDelay: 5 * time.Millisecond, Multiplier: 1.5, MaxDelay: 20 * time.Millisecond},
			}),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return n.dialPair(ctx, from, to)
			}),
		}
		if stalled {
			opts = append(opts, grpc.WithStreamInterceptor(stallStream))
		}
		return grpc.NewClient("passthrough:///"+to, opts...)
	}
}

// stallStream blocks every message sent on a stream until the stream ends.
func stallStream(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	cs, err := streamer(ctx, desc, cc, method, opts...)
	if err != nil {
		return nil, err
	}
	return &stalledStream{ClientStream: cs, ctx: ctx}, nil
}

type stalledStream struct {
	grpc.ClientStream
	ctx context.Context
}

func (s *stalledStream) SendMsg(any) error {
	<-s.ctx.Done()
	return s.ctx.Err()
}

func (n *testNet) dialPair(ctx context.Context, from, to string) (net.Conn, error) {
	n.mu.Lock()
	if n.down[from] || n.down[to] {
		n.mu.Unlock()
		return nil, errUnreachable
	}
	key := [2]string{from, to}
	ps := n.pairs[key]
	if ps == nil {
		ps = n.startPair(from, to)
		n.pairs[key] = ps
	}
	n.mu.Unlock()
	return ps.lis.DialContext(ctx)
}

// startPair must be called with n.mu held.
func (n *testNet) startPair(from, to string) *pairServer {
	node := n.nodes[to]
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pb.RegisterReplicationServiceServer(srv, &Server{
		NodeID:   to,
		Metrics:  node.metrics,
		Triggers: node.rep,
		identity: fixedIdentity(from),
	})
	go func() { _ = srv.Serve(lis) }()
	return &pairServer{lis: lis, srv: srv}
}

func (n *testNet) stopPairs() {
	n.mu.Lock()
	servers := make([]*grpc.Server, 0, len(n.pairs))
	for key, ps := range n.pairs {
		servers = append(servers, ps.srv)
		delete(n.pairs, key)
	}
	n.mu.Unlock()
	for _, s := range servers {
		s.Stop()
	}
}

// waitFor polls cond until it holds or the test times out.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// statusOf returns the status of one peer, or a zero value if it is unknown.
func statusOf(r *Replicator, id string) PeerStatus {
	for _, s := range r.Status() {
		if s.ID == id {
			return s
		}
	}
	return PeerStatus{}
}
