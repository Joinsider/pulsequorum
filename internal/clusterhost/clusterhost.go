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
	"github.com/joinsider/uptimecluster/internal/secrets"
	"github.com/joinsider/uptimecluster/internal/store/sqlite"
)

// Intervals of the background jobs.
const (
	// RevocationReload bounds how long a revocation takes to reach the
	// handshakes of every node (FA-PKI-04).
	RevocationReload = 10 * time.Second
	renewalCheck     = time.Hour
	leaderWatch      = time.Second
)

// JoinOptions describe how this node joins, if it does.
type JoinOptions struct {
	Seeds []string
	Token string
	// ExternalKEK is set when MASTER_KEY or MASTER_KEY_FILE is configured;
	// then the cluster's key is never installed, only compared.
	ExternalKEK bool
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
	cert := &clusterpki.NodeCert{DataDir: dataDir, NodeID: nodeID}
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
	KEK        *secrets.KEK
	Keyring    *secrets.Keyring
	Prepared   Prepared
	Log        *slog.Logger
	// RaftLogLevel and RaftLogJSON configure the Raft library logger.
	RaftLogLevel string
	RaftLogJSON  bool
	// OnFatal stops the node when the FSM halts (FA-CLU-13).
	OnFatal func(error)
}

// Host is the running cluster side of a voter.
type Host struct {
	Node        *cluster.Node
	Authority   *clusterpki.Authority
	Cert        *clusterpki.NodeCert
	Revocations *clusterpki.Revocations
	Manager     *membership.Manager

	mux  *clusternet.Mux
	grpc *grpc.Server
	sec  clusternet.Security
	log  *slog.Logger
	wg   sync.WaitGroup
}

// Start opens the cluster port and starts Raft on it with mTLS.
func Start(ctx context.Context, opts Options) (*Host, error) {
	h := &Host{
		Cert:        opts.Prepared.Cert,
		Revocations: &clusterpki.Revocations{},
		Authority:   &clusterpki.Authority{State: opts.State, Keyring: opts.Keyring},
		log:         opts.Log,
	}
	if err := h.Revocations.Reload(ctx, opts.State); err != nil {
		return nil, err
	}
	verifier := &pki.Verifier{
		Roots:         h.roots,
		ClusterIDFunc: h.clusterID,
		Revoked:       h.Revocations.Revoked,
		RevokedNode:   h.Revocations.RevokedNode,
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
		RaftLogLevel: opts.RaftLogLevel, RaftLogJSON: opts.RaftLogJSON,
	}, opts.State, opts.OnFatal)
	if err != nil {
		_ = mux.Close()
		return nil, err
	}
	h.Node = node
	h.Authority.Cluster = node
	h.Manager = &membership.Manager{Raft: node, Status: h.status, Log: opts.Log}

	h.grpc = grpc.NewServer(clusternet.ServerOptions(clusternet.Authorize(membership.Access))...)
	pb.RegisterClusterServiceServer(h.grpc, &membership.Service{
		Node: node, State: opts.State, Authority: h.Authority, Manager: h.Manager,
		KEK: opts.KEK, KEKFingerprint: opts.KEK.Fingerprint, DialLeader: h.dial, Log: opts.Log,
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
	if err := h.Cert.EnsureSelf(ctx, h.Authority, h.Node.IsLeader(), time.Now()); err != nil {
		return fmt.Errorf("node certificate: %w", err)
	}
	if leaf := h.Cert.Leaf(); leaf != nil {
		h.log.Info("node certificate ready", "serial", pki.SerialString(leaf), "renew_at", pki.RenewAt(leaf).UTC())
	}
	return nil
}

// Run starts the background jobs: revocation reload, certificate renewal
// and resuming promotions on a new leader. It returns when ctx ends.
func (h *Host) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { h.Revocations.Run(ctx, h.Authority.State, RevocationReload, h.log) })
	wg.Go(func() { h.Cert.RunRenewal(ctx, h.issuer, renewalCheck, h.log) })
	wg.Go(func() { h.Manager.Run(ctx, leaderWatch) })
	wg.Wait()
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
	h.grpc.GracefulStop()
	err := h.Node.Shutdown()
	_ = h.mux.Close()
	h.wg.Wait()
	return err
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
