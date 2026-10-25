// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package membership

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/clusterpki"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/pki"
)

// JoinOptions describe a node joining an existing cluster.
type JoinOptions struct {
	Seeds   []string
	Token   string
	NodeID  string
	Name    string
	Address string
	Version string
	// MaxCommandVersion is announced to the cluster at join (ADR 0018). Zero
	// means cluster.CommandVersion; tests override it.
	MaxCommandVersion uint32
	Cert              *clusterpki.NodeCert
	Log               *slog.Logger
}

// Joined is the result of a successful join.
type Joined struct {
	ClusterID string
	// KEK is the cluster's generated master key, if it uses one (ADR 0011
	// option B); empty if the cluster uses an external key.
	KEK            []byte
	KEKFingerprint string
}

// Permanent join errors: retrying with the same token cannot succeed.
var (
	ErrJoinRejected = errors.New("join rejected")
)

// Join enrolls this node as voter through one of the seeds (FA-CLU-03). It
// trusts only a server whose CA matches the fingerprint in the token and
// retries with backoff and jitter until ctx ends; a rejected token or an
// address conflict ends it at once.
func Join(ctx context.Context, opts JoinOptions) (Joined, error) {
	tok, err := pki.ParseJoinToken(opts.Token)
	if err != nil {
		return Joined{}, fmt.Errorf("%w: %w", ErrJoinRejected, err)
	}
	key, csr, err := opts.Cert.NewCSR()
	if err != nil {
		return Joined{}, err
	}
	maxVersion := opts.MaxCommandVersion
	if maxVersion == 0 {
		maxVersion = cluster.CommandVersion
	}
	req := &pb.JoinRequest{
		TokenId: tok.ID, TokenSecret: tok.Secret, CsrDer: csr,
		Node: &pb.NodeInfo{
			Id: opts.NodeID, Name: opts.Name, Role: pb.NodeInfo_ROLE_VOTER, AdvertiseAddr: opts.Address, Version: opts.Version,
			MaxCommandVersion: maxVersion,
		},
	}
	backoff := time.Second
	for attempt := 0; ; attempt++ {
		seed := opts.Seeds[attempt%len(opts.Seeds)]
		resp, pinned, err := joinOnce(ctx, seed, tok.Fingerprint, req)
		if err == nil {
			if err := install(opts, key, resp, pinned); err != nil {
				return Joined{}, err
			}
			return Joined{ClusterID: resp.GetClusterId(), KEK: resp.GetKek(), KEKFingerprint: resp.GetKekFingerprint()}, nil
		}
		if permanentJoinError(err) {
			return Joined{}, fmt.Errorf("%w by %s: %s", ErrJoinRejected, seed, status.Convert(err).Message())
		}
		opts.Log.Warn("join attempt failed, retrying", "seed", seed, "err", err)
		select {
		case <-ctx.Done():
			return Joined{}, fmt.Errorf("join: %w", ctx.Err())
		case <-time.After(backoff/2 + rand.N(backoff/2+1)): //nolint:gosec // G404: jitter, not security
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// permanentJoinError reports whether retrying the same join cannot succeed: a
// rejected token, an address conflict, an invalid request, or a binary too old
// for the cluster's command level (ADR 0018). Other FailedPrecondition errors,
// such as "not the leader", are retried.
func permanentJoinError(err error) bool {
	switch status.Code(err) {
	case codes.PermissionDenied, codes.AlreadyExists, codes.InvalidArgument:
		return true
	case codes.FailedPrecondition:
		return strings.Contains(status.Convert(err).Message(), upgradeRequiredText)
	}
	return false
}

func joinOnce(ctx context.Context, seed, fingerprint string, req *pb.JoinRequest) (*pb.JoinResponse, *x509.Certificate, error) {
	var pinned atomic.Pointer[x509.Certificate]
	conn, err := grpc.NewClient(seed, grpc.WithTransportCredentials(credentials.NewTLS(pki.JoinClientConfig(fingerprint, &pinned))))
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = conn.Close() }()
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := pb.NewClusterServiceClient(conn).Join(callCtx, req)
	if err != nil {
		return nil, nil, err
	}
	return resp, pinned.Load(), nil
}

// install checks what the cluster sent before storing it: the chain must end
// in the pinned CA and carry this node's identity as voter.
func install(opts JoinOptions, key *ecdsa.PrivateKey, resp *pb.JoinResponse, pinned *x509.Certificate) error {
	if pinned == nil || len(resp.GetCertificateChainDer()) < 2 {
		return errors.New("join: incomplete certificate from the cluster")
	}
	ca, err := x509.ParseCertificate(resp.GetCertificateChainDer()[1])
	if err != nil || pki.Fingerprint(ca) != pki.Fingerprint(pinned) {
		return errors.New("join: certificate is not issued by the pinned cluster CA")
	}
	leaf, err := x509.ParseCertificate(resp.GetCertificateChainDer()[0])
	if err != nil {
		return fmt.Errorf("join: %w", err)
	}
	v := pki.Verifier{ClusterID: resp.GetClusterId(), Roots: func() []*x509.Certificate { return []*x509.Certificate{ca} }}
	id, err := v.Verify([]*x509.Certificate{leaf}, x509.ExtKeyUsageClientAuth)
	if err != nil {
		return fmt.Errorf("join: %w", err)
	}
	if id.NodeID != opts.NodeID || id.Role != pki.RoleVoter {
		return fmt.Errorf("join: certificate is for %s/%s, not this voter", id.Role, id.NodeID)
	}
	// Install also checks that the certificate belongs to this node's key.
	return opts.Cert.Install(key, clusterpki.Issued{
		Identity: id, Leaf: leaf, ChainDER: resp.GetCertificateChainDer(), BundleDER: resp.GetCaBundleDer(),
	})
}

// RenewViaLeader returns an IssueFunc that renews this node's certificate
// through RenewCertificate on the leader (followers, FA-PKI-03).
func RenewViaLeader(leader func() string, dial func(address string) (pb.ClusterServiceClient, func(), error)) clusterpki.IssueFunc {
	return func(ctx context.Context, csr []byte) (clusterpki.Issued, error) {
		addr := leader()
		if addr == "" {
			return clusterpki.Issued{}, errors.New("no leader known")
		}
		client, done, err := dial(addr)
		if err != nil {
			return clusterpki.Issued{}, err
		}
		defer done()
		callCtx, cancel := context.WithTimeout(ctx, renewTimeout)
		defer cancel()
		resp, err := client.RenewCertificate(callCtx, &pb.RenewCertificateRequest{CsrDer: csr})
		if err != nil {
			return clusterpki.Issued{}, err
		}
		return clusterpki.Issued{ChainDER: resp.GetCertificateChainDer(), BundleDER: resp.GetCaBundleDer()}, nil
	}
}
