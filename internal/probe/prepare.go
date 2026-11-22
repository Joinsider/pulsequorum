// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package probe implements the client-side probe runtime (ADR 0034, ADR 0035):
// joining a cluster, managing the voter candidate list, maintaining the local
// heartbeat buffer, streaming monitor assignments, and uploading heartbeats
// with keepalive, tick-timeout failover, and CA bundle verification.
package probe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/joinsider/pulsequorum/internal/clusterpki"
	"github.com/joinsider/pulsequorum/internal/membership"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/pki"
)

// PrepareOptions configure probe identity loading and initial cluster join.
type PrepareOptions struct {
	DataDir      string
	NodeID       string
	Name         string
	Seeds        []string
	Token        string
	Region       string
	Labels       map[string]string
	Capabilities []string
	Version      string
	Incarnation  string
	Log          *slog.Logger
	Clock        func() time.Time
}

// Prepare loads the probe's certificate and voter list, joining the cluster on
// the first start when no certificate exists yet (ADR 0034 section 2).
func Prepare(ctx context.Context, opts PrepareOptions) (*clusterpki.NodeCert, *VoterList, error) {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	voters, err := NewVoterList(opts.DataDir, opts.Seeds, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("probe voters: %w", err)
	}
	cert := &clusterpki.NodeCert{DataDir: opts.DataDir, NodeID: opts.NodeID, Now: opts.Clock}
	if err := cert.Load(); err != nil {
		return nil, nil, err
	}
	if cert.Leaf() != nil {
		if opts.Token != "" {
			log.Info("join token ignored: probe certificate already exists")
		}
		return cert, voters, nil
	}
	if opts.Token == "" {
		return nil, nil, errors.New("probe has no certificate: configure JOIN_TOKEN, CLUSTER_JOIN_TOKEN or JOIN_TOKEN_FILE")
	}
	candidates := voters.Candidates()
	if len(candidates) == 0 {
		return nil, nil, errors.New("probe has no certificate and no cluster seeds: configure CLUSTER_SEEDS")
	}

	log.Info("joining cluster as probe", "seeds", candidates, "node_id", opts.NodeID)
	joined, err := membership.Join(ctx, membership.JoinOptions{
		Seeds:        candidates,
		Token:        opts.Token,
		NodeID:       opts.NodeID,
		Name:         opts.Name,
		Role:         pki.RoleProbe,
		Region:       opts.Region,
		Labels:       opts.Labels,
		Capabilities: opts.Capabilities,
		Version:      opts.Version,
		Incarnation:  opts.Incarnation,
		Cert:         cert,
		Log:          log,
		Clock:        opts.Clock,
	})
	if err != nil {
		return nil, nil, err
	}
	var voterNodes []*pb.NodeInfo
	for _, addr := range joined.Voters {
		voterNodes = append(voterNodes, &pb.NodeInfo{AdvertiseAddr: addr, Role: pb.NodeInfo_ROLE_VOTER})
	}
	if len(voterNodes) > 0 {
		if err := voters.Update(voterNodes); err != nil {
			log.Warn("persisting learned voters failed", "err", err)
		}
	}
	log.Info("probe joined cluster", "cluster_id", joined.ClusterID, "certificate_serial", pki.SerialString(cert.Leaf()))
	return cert, voters, nil
}
