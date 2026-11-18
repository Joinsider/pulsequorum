// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package clusterhost assembles the cluster side of a voter: join, master
// key hand-over, the mTLS cluster port with Raft and ClusterService, the CA
// and the node certificate (ADR 0005, 0006, 0011). The binary and the
// multi-node tests use the same code.
package clusterhost

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/grpc"

	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/clusternet"
	"github.com/joinsider/uptimecluster/internal/clusterpki"
	"github.com/joinsider/uptimecluster/internal/membership"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/pki"
	"github.com/joinsider/uptimecluster/internal/replication"
	"github.com/joinsider/uptimecluster/internal/secrets"
	"github.com/joinsider/uptimecluster/internal/selfcheck"
	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/store/sqlite"
)

// Intervals of the background jobs.
const (
	// RevocationReload bounds how long a revocation takes to reach the
	// handshakes of every node (FA-PKI-04).
	RevocationReload = 10 * time.Second
	renewalCheck     = time.Hour
	leaderWatch      = time.Second
	// grpcStopGrace bounds the graceful stop of the cluster port. Peers keep
	// their replication push streams to this node open while it is a voter,
	// so a graceful stop alone could wait for them indefinitely.
	grpcStopGrace = 5 * time.Second
)

// JoinOptions describe how this node joins, if it does.
type JoinOptions struct {
	Seeds []string
	Token string
	// ExternalKEK is set when MASTER_KEY or MASTER_KEY_FILE is configured;
	// then the cluster's key is never installed, only compared.
	ExternalKEK bool
	// MaxCommandVersion is the command version announced at join. Zero means
	// cluster.CommandVersion; a test override (ADR 0018).
	MaxCommandVersion uint32
	// Clock is the wall clock of the joining node; nil means time.Now (ADR 0020).
	Clock func() time.Time
}

// Prepared is the result of Prepare.
type Prepared struct {
	Cert *clusterpki.NodeCert
	// Joining is true if this node must not bootstrap (FA-CLU-02).
	Joining bool
	// KEKFingerprint is the cluster's key fingerprint received at join.
	KEKFingerprint string
}

// Prepare loads the node certificate and, for a new node with join seeds,
// joins the cluster and installs the generated master key. It runs before
// the master key is loaded, so a joining node never generates its own.
//
// A node bootstraps only without Raft state, without join seeds and without
// a cluster certificate: a node that joined before but crashed before Raft
// stored anything must not found a cluster of its own.
func Prepare(ctx context.Context, dataDir, nodeID, name, advertise, version string, join JoinOptions, log *slog.Logger) (Prepared, error) {
	cert := &clusterpki.NodeCert{DataDir: dataDir, NodeID: nodeID, Now: join.Clock}
	if err := cert.Load(); err != nil {
		return Prepared{}, err
	}
	hasState, err := cluster.HasState(dataDir)
	if err != nil {
		return Prepared{}, err
	}
	p := Prepared{Cert: cert, Joining: !hasState && (len(join.Seeds) > 0 || cert.Leaf() != nil)}
	if hasState || cert.Leaf() != nil || len(join.Seeds) == 0 {
		return p, nil
	}
	if join.Token == "" {
		return Prepared{}, errors.New("CLUSTER_JOIN is set but no join token (JOIN_TOKEN or JOIN_TOKEN_FILE)")
	}
	log.Info("joining the cluster", "seeds", join.Seeds, "node_id", nodeID, "advertise", advertise)
	joined, err := membership.Join(ctx, membership.JoinOptions{
		Seeds: join.Seeds, Token: join.Token, NodeID: nodeID, Name: name, Address: advertise, Version: version, Cert: cert, Log: log,
		MaxCommandVersion: join.MaxCommandVersion, Clock: join.Clock,
	})
	if err != nil {
		return Prepared{}, err
	}
	p.KEKFingerprint = joined.KEKFingerprint
	switch {
	case join.ExternalKEK:
		// Compared after the key is loaded (CheckKEK).
	case len(joined.KEK) > 0:
		if err := secrets.InstallGenerated(dataDir, joined.KEK, joined.KEKFingerprint); err != nil {
			return Prepared{}, err
		}
		clear(joined.KEK)
	default:
		return Prepared{}, errors.New("the cluster uses an external master key: configure MASTER_KEY or MASTER_KEY_FILE with the same key on this node")
	}
	log.Info("joined the cluster", "cluster_id", joined.ClusterID, "certificate_serial", pki.SerialString(cert.Leaf()))
	return p, nil
}

// CheckKEK compares the loaded master key with the one the cluster reported
// at join (a voter with an external key, ADR 0011).
func (p Prepared) CheckKEK(kek *secrets.KEK) error {
	if p.KEKFingerprint != "" && p.KEKFingerprint != kek.Fingerprint {
		return fmt.Errorf("%w: this node's master key differs from the cluster's", secrets.ErrKEKMismatch)
	}
	return nil
}

// Options configure Start.
type Options struct {
	NodeID     string
	ListenAddr string
	Advertise  string
	DataDir    string
	Timeout    time.Duration
	State      *sqlite.StateStore
	// Metrics holds the heartbeats that the replication exchanges (FA-REP-01).
	Metrics store.Metrics
	// OnHeartbeatsStored is called with the heartbeats that replication stored
	// (pushes and anti-entropy pages), see replication.Options.OnStored. It
	// must not block (ADR 0021). Nil means no callback.
	OnHeartbeatsStored func([]store.Heartbeat)
	// ReplicationInterval is the period of the anti-entropy pass per peer.
	// Zero means the default of the replication package; tests use short ones.
	ReplicationInterval time.Duration
	KEK                 *secrets.KEK
	Keyring             *secrets.Keyring
	Prepared            Prepared
	Log                 *slog.Logger
	// RaftLogLevel and RaftLogJSON configure the Raft library logger.
	RaftLogLevel string
	RaftLogJSON  bool
	// OnFatal stops the node when the FSM halts (FA-CLU-13).
	OnFatal func(error)
	// Name, Version and Capabilities describe this node in the node registry
	// (ADR 0017). Capabilities are sorted and deduplicated on registration.
	Name         string
	Version      string
	Capabilities []string
	// MaxCommandVersion overrides cluster.CommandVersion, the highest command
	// version this node applies and announces (ADR 0018). Zero means
	// cluster.CommandVersion. Tests use it to simulate an older binary.
	MaxCommandVersion uint32
	// Clock is the wall clock of this node for certificate validity, command
	// timestamps, heartbeat observations and replication decisions. Nil means
	// time.Now. The multi-node tests give each node its own skewed clock
	// (ADR 0020).
	Clock func() time.Time
	// ClockInterval is the period of the clock offset measurement; zero means
	// clockSampleInterval. Tests shorten it.
	ClockInterval time.Duration
	// SelfCheck returns the latest result of this node's reference targets
	// (ADR 0022); it is reported in GetStatus. Nil means not configured.
	SelfCheck func() selfcheck.Status
	// DiskFreeRatio returns the free share of the data directory, negative if
	// unknown; it is reported in GetStatus (ADR 0025). Nil means unknown.
	DiskFreeRatio func() float64
	// WALBytes returns the WAL sizes of state.db and metrics.db; it is reported
	// in GetStatus (ADR 0025). Nil means unknown.
	WALBytes func() (state, metrics int64)
	// SSRFProtection is whether SSRF protection is on for this node (ADR 0030);
	// it is reported in GetStatus and shown per node in the cluster view.
	SSRFProtection bool
}

// Host is the running cluster side of a voter.
type Host struct {
	Node        *cluster.Node
	Authority   *clusterpki.Authority
	Cert        *clusterpki.NodeCert
	Revocations *clusterpki.Revocations
	Manager     *membership.Manager
	// Replication pushes and repairs heartbeats between the voters (ADR 0006).
	Replication *replication.Replicator
	// Health turns the heartbeat failures of the leader into the health of
	// the nodes in the registry (ADR 0017).
	Health *membership.HealthWatcher
	// Level raises the cluster command level once all voters support it
	// (ADR 0018).
	Level *membership.LevelWatcher

	state store.State
	self  store.Node
	// clock is the wall clock of this node (Options.Clock).
	clock      func() time.Time
	clocks     clockTracker
	selfChecks selfCheckTracker
	ssrf       ssrfTracker
	// ssrfProtection is this node's own SSRF setting (Options.SSRFProtection).
	ssrfProtection bool
	peerViews      peerViewTracker
	signals        signalTracker
	// service answers GetStatus; SelfSignals asks it for this node's signals.
	service       *membership.Service
	clockInterval time.Duration
	mux           *clusternet.Mux
	grpc          *grpc.Server
	advertise     string
	dataDir       string
	sec           clusternet.Security
	log           *slog.Logger
	wg            sync.WaitGroup

	// leaderMu guards the cached connection to the current leader, which
	// write forwarding reuses instead of dialing per command.
	leaderMu     sync.Mutex
	leaderAddr   string
	leaderConn   *grpc.ClientConn
	leaderClient pb.ClusterServiceClient
}

// Start opens the cluster port and starts Raft on it with mTLS.
func Start(ctx context.Context, opts Options) (*Host, error) {
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	h := &Host{
		Cert:           opts.Prepared.Cert,
		ssrfProtection: opts.SSRFProtection,
		Revocations:    &clusterpki.Revocations{},
		Authority:      &clusterpki.Authority{State: opts.State, Keyring: opts.Keyring, Now: clock},
		log:            opts.Log,
		advertise:      opts.Advertise,
		dataDir:        opts.DataDir,
		state:          opts.State,
		self:           selfNode(opts),
		clock:          clock,
		clockInterval:  opts.ClockInterval,
	}
	if h.clockInterval <= 0 {
		h.clockInterval = clockSampleInterval
	}
	h.Cert.Now = clock
	if err := h.Revocations.Reload(ctx, opts.State); err != nil {
		return nil, err
	}
	verifier := &pki.Verifier{
		Roots:         h.roots,
		ClusterIDFunc: h.clusterID,
		Revoked:       h.Revocations.Revoked,
		RevokedNode:   h.Revocations.RevokedNode,
		Now:           clock,
	}
	h.sec = clusternet.Security{Holder: &h.Cert.Holder, Verifier: verifier}
	mux, err := clusternet.Listen(opts.ListenAddr, opts.Advertise, h.sec, opts.Log)
	if err != nil {
		return nil, err
	}
	h.mux = mux

	transport := raft.NewNetworkTransportWithConfig(&raft.NetworkTransportConfig{
		Stream: mux.RaftLayer(), MaxPool: 3, Timeout: 10 * time.Second,
	})
	node, err := cluster.Start(ctx, cluster.Config{
		NodeID: opts.NodeID, Advertise: opts.Advertise, DataDir: opts.DataDir, Timeout: opts.Timeout,
		Transport: transport, Join: opts.Prepared.Joining, Logger: opts.Log,
		Forwarder:         &membership.Forwarder{Client: h.leaderClientFor},
		RaftLogLevel:      opts.RaftLogLevel,
		RaftLogJSON:       opts.RaftLogJSON,
		MaxCommandVersion: opts.MaxCommandVersion,
		Clock:             clock,
	}, opts.State, opts.OnFatal)
	if err != nil {
		_ = mux.Close()
		return nil, err
	}
	h.Node = node
	h.Authority.Cluster = node
	h.Manager = &membership.Manager{Raft: node, Status: h.status, DialLeader: h.dial, Log: opts.Log}
	h.Health = &membership.HealthWatcher{Node: node, State: opts.State, Log: opts.Log, Now: clock}
	h.Level = &membership.LevelWatcher{Node: node, State: opts.State, Log: opts.Log}
	node.OnHeartbeat(h.Health.Observe)
	h.Replication = replication.New(replication.Options{
		NodeID:              opts.NodeID,
		Metrics:             opts.Metrics,
		Peers:               h.replicationPeers,
		Dial:                func(address string) (*grpc.ClientConn, error) { return clusternet.DialVoter(address, h.sec) },
		Log:                 opts.Log,
		AntiEntropyInterval: opts.ReplicationInterval,
		Now:                 clock,
		OnStored:            opts.OnHeartbeatsStored,
	})
	// A peer that resumes heartbeats or joins the configuration gets an
	// immediate anti-entropy pass (ADR 0012 B6). Followers do not see these
	// observations; their push streams reconnect instead.
	node.OnPeerResumed(h.Replication.Trigger)

	h.grpc = grpc.NewServer(clusternet.ServerOptions(membership.Access)...)
	h.service = &membership.Service{
		Node: node, State: opts.State, Authority: h.Authority, Manager: h.Manager,
		KEK: opts.KEK, KEKFingerprint: opts.KEK.Fingerprint, DialLeader: h.dial, Log: opts.Log, Clock: clock,
		SelfCheck: opts.SelfCheck, Own: h.ownSignals(opts), SSRFProtection: opts.SSRFProtection,
	}
	pb.RegisterClusterServiceServer(h.grpc, h.service)
	pb.RegisterReplicationServiceServer(h.grpc, &replication.Server{
		NodeID: opts.NodeID, Metrics: opts.Metrics, Triggers: h.Replication, OnStored: opts.OnHeartbeatsStored, Log: opts.Log,
	})
	h.wg.Go(func() {
		if err := mux.Serve(); err != nil {
			opts.Log.Error("cluster port stopped", "err", err)
		}
	})
	h.wg.Go(func() { _ = h.grpc.Serve(mux.GRPCListener()) })
	opts.Log.Info("cluster port listening", "addr", mux.Addr().String(), "advertise", opts.Advertise, "joining", opts.Prepared.Joining)
	return h, nil
}

// Addr is the bound cluster port address.
func (h *Host) Addr() string { return h.mux.Addr().String() }

// Bootstrap creates the CA on the leader of a new cluster and gives the
// leader its certificate (FA-PKI-01); a joined node already has one. It
// runs after the data keys are loaded.
func (h *Host) Bootstrap(ctx context.Context) error {
	if err := h.Authority.Ensure(ctx, h.Node.IsLeader()); err != nil {
		return fmt.Errorf("cluster CA: %w", err)
	}
	if err := h.Cert.EnsureSelf(ctx, h.Authority, h.Node.IsLeader(), h.clock()); err != nil {
		return fmt.Errorf("node certificate: %w", err)
	}
	if leaf := h.Cert.Leaf(); leaf != nil {
		h.log.Info("node certificate ready", "serial", pki.SerialString(leaf), "renew_at", pki.RenewAt(leaf).UTC())
	}
	return nil
}

// Run starts the background jobs: revocation reload, certificate renewal,
// resuming promotions on a new leader, address announcement, heartbeat
// replication, node registration and node health. It returns when ctx ends.
func (h *Host) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { h.Revocations.Run(ctx, h.Authority.State, RevocationReload, h.log) })
	wg.Go(func() { h.Cert.RunRenewal(ctx, h.issuer, renewalCheck, h.log) })
	wg.Go(func() { h.Manager.Run(ctx, leaderWatch) })
	wg.Go(func() { h.announceAddress(ctx) })
	wg.Go(func() { h.Replication.Run(ctx) })
	wg.Go(func() { h.registerSelf(ctx) })
	wg.Go(func() { h.Health.Run(ctx) })
	wg.Go(func() { h.Level.Run(ctx) })
	wg.Go(func() { h.watchClocks(ctx) })
	wg.Go(func() { h.revokeRecovered(ctx) })
	wg.Wait()
}

// replicationPeers returns the current Raft voters except this node. A
// configuration that cannot be read yields no peers; the next refresh retries.
func (h *Host) replicationPeers() []replication.Peer {
	members, _, err := h.Node.Members()
	if err != nil {
		return nil
	}
	var peers []replication.Peer
	for _, m := range members {
		if m.Voter && m.ID != h.Node.ID() {
			peers = append(peers, replication.Peer{ID: m.ID, Address: m.Address})
		}
	}
	return peers
}

// announceAddress reports a changed CLUSTER_ADVERTISE to the leader
// (FA-CLU-10): the node's entry in the Raft configuration still has the old
// address, so the leader cannot reach it. The node authenticates with its
// certificate and calls the leader directly; voters keep their vote.
func (h *Host) announceAddress(ctx context.Context) {
	backoff := time.Second
	for {
		done, err := h.tryAnnounce(ctx)
		if done {
			return
		}
		if err != nil {
			h.log.Warn("announcing the new cluster address failed, retrying", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (h *Host) tryAnnounce(ctx context.Context) (bool, error) {
	members, _, err := h.Node.Members()
	if err != nil {
		return false, err
	}
	var self *cluster.Member
	for i, m := range members {
		if m.ID == h.Node.ID() {
			self = &members[i]
		}
	}
	if self == nil || self.Address == h.advertise || h.Cert.Leaf() == nil {
		return true, nil // not a member yet, unchanged, or no certificate
	}
	// A node that leads under its old address changes the configuration
	// itself; the others would name it as leader, and its old address is not
	// reachable (FA-CLU-10).
	if h.Node.IsLeader() {
		if err := h.Manager.AddVoter(ctx, h.Node.ID(), h.advertise); err != nil {
			return false, err
		}
		h.log.Info("cluster address updated", "from", self.Address, "to", h.advertise)
		return true, nil
	}
	// Ask the other voters who leads, then call the leader directly.
	addrByID := map[string]string{}
	for _, m := range members {
		addrByID[m.ID] = m.Address
	}
	var lastErr error
	for _, m := range members {
		if m.ID == h.Node.ID() || !m.Voter {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		st, err := h.status(callCtx, m.Address)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		if st.GetLeaderId() == h.Node.ID() {
			// This node won the election meanwhile; the next round updates locally.
			lastErr = errors.New("this node became leader")
			continue
		}
		leader := addrByID[st.GetLeaderId()]
		if leader == "" {
			lastErr = errors.New("no leader known")
			continue
		}
		client, closeConn, err := h.dial(leader)
		if err != nil {
			lastErr = err
			continue
		}
		callCtx, cancel = context.WithTimeout(ctx, 15*time.Second)
		_, err = client.Join(callCtx, &pb.JoinRequest{Node: &pb.NodeInfo{
			Id: h.Node.ID(), Role: pb.NodeInfo_ROLE_VOTER, AdvertiseAddr: h.advertise,
		}})
		cancel()
		closeConn()
		if err != nil {
			lastErr = err
			continue
		}
		h.log.Info("cluster address updated", "from", self.Address, "to", h.advertise)
		return true, nil
	}
	return false, lastErr
}

// issuer renews on the leader directly, on followers through the leader.
func (h *Host) issuer() clusterpki.IssueFunc {
	if h.Node.IsLeader() {
		return func(ctx context.Context, csr []byte) (clusterpki.Issued, error) {
			return h.Authority.IssueSelf(ctx, h.Node.ID(), csr)
		}
	}
	return membership.RenewViaLeader(func() string { a, _ := h.Node.Leader(); return a }, h.dial)
}

// Close stops gRPC, Raft and the cluster port.
func (h *Host) Close() error {
	h.stopGRPC()
	err := h.Node.Shutdown()
	_ = h.mux.Close()
	h.wg.Wait()
	h.closeLeaderConn()
	return err
}

// stopGRPC stops the cluster port gracefully and cuts the streams that are
// still open after grpcStopGrace, such as the replication push streams of
// the peers.
func (h *Host) stopGRPC() {
	done := make(chan struct{})
	go func() {
		h.grpc.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(grpcStopGrace):
		h.log.Warn("cluster port still has open streams, closing them", "grace", grpcStopGrace)
		h.grpc.Stop()
		<-done
	}
}

// leaderClientFor returns a client for the leader at address. The connection
// is kept while the leader stays the same; a new leader replaces and closes
// it. grpc.NewClient connects lazily, so dialing is cheap.
func (h *Host) leaderClientFor(address string) (pb.ClusterServiceClient, error) {
	h.leaderMu.Lock()
	defer h.leaderMu.Unlock()
	if h.leaderClient != nil && h.leaderAddr == address {
		return h.leaderClient, nil
	}
	conn, err := clusternet.DialVoter(address, h.sec)
	if err != nil {
		return nil, err
	}
	if h.leaderConn != nil {
		_ = h.leaderConn.Close()
	}
	h.leaderConn = conn
	h.leaderAddr = address
	h.leaderClient = pb.NewClusterServiceClient(conn)
	return h.leaderClient, nil
}

func (h *Host) closeLeaderConn() {
	h.leaderMu.Lock()
	defer h.leaderMu.Unlock()
	if h.leaderConn != nil {
		_ = h.leaderConn.Close()
	}
	h.leaderConn, h.leaderClient, h.leaderAddr = nil, nil, ""
}

// roots are the CA bundle from the replicated state, or the CA of the node
// certificate on a joined node until the state arrived.
func (h *Host) roots() []*x509.Certificate {
	if b, _, err := h.Authority.Bundle(context.Background()); err == nil {
		return b
	}
	if ca := h.Cert.CA(); ca != nil {
		return []*x509.Certificate{ca}
	}
	return nil
}

func (h *Host) clusterID() string {
	if _, id, err := h.Authority.Bundle(context.Background()); err == nil {
		return id
	}
	if leaf := h.Cert.Leaf(); leaf != nil {
		if id, err := pki.IdentityOf(leaf); err == nil {
			return id.ClusterID
		}
	}
	return ""
}

func (h *Host) dial(address string) (pb.ClusterServiceClient, func(), error) {
	conn, err := clusternet.DialVoter(address, h.sec)
	if err != nil {
		return nil, nil, err
	}
	return pb.NewClusterServiceClient(conn), func() { _ = conn.Close() }, nil
}

func (h *Host) status(ctx context.Context, address string) (*pb.GetStatusResponse, error) {
	client, done, err := h.dial(address)
	if err != nil {
		return nil, err
	}
	defer done()
	return client.GetStatus(ctx, &pb.GetStatusRequest{})
}
