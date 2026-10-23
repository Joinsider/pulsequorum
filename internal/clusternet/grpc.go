// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package clusternet

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/joinsider/uptimecluster/internal/pki"
)

// handshaked are server credentials for connections the Mux has already
// handshaked: they expose the TLS state to gRPC without a second handshake.
type handshaked struct{}

func (handshaked) ClientHandshake(context.Context, string, net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("clusternet: server-only credentials")
}

func (handshaked) ServerHandshake(c net.Conn) (net.Conn, credentials.AuthInfo, error) {
	tc, ok := c.(*tls.Conn)
	if !ok {
		return nil, nil, errors.New("clusternet: connection is not TLS")
	}
	return c, credentials.TLSInfo{
		State:          tc.ConnectionState(),
		CommonAuthInfo: credentials.CommonAuthInfo{SecurityLevel: credentials.PrivacyAndIntegrity},
	}, nil
}

func (handshaked) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "tls"}
}
func (h handshaked) Clone() credentials.TransportCredentials { return h }
func (handshaked) OverrideServerName(string) error           { return nil }

// ServerOptions returns the gRPC server options of the cluster port
// (ADR 0006: keepalive, maximum connection age). levels says who may call
// each method, unary and streaming alike; unknown methods are denied.
func ServerOptions(levels map[string]Access) []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.Creds(handshaked{}),
		grpc.UnaryInterceptor(Authorize(levels)),
		grpc.StreamInterceptor(AuthorizeStream(levels)),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionAge:      MaxConnectionAge,
			MaxConnectionAgeGrace: 30 * time.Second,
			Time:                  15 * time.Second,
			Timeout:               10 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
		grpc.MaxRecvMsgSize(4 << 20),
	}
}

// Peer returns the verified identity of the caller, or ok=false if it
// presented no certificate. Certificates were verified during the handshake
// (chain, expiry, revocation, cluster).
func Peer(ctx context.Context) (pki.Identity, bool) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return pki.Identity{}, false
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.PeerCertificates) == 0 {
		return pki.Identity{}, false
	}
	id, err := pki.IdentityOf(info.State.PeerCertificates[0])
	if err != nil {
		return pki.Identity{}, false
	}
	return id, true
}

// Access says who may call an RPC.
type Access int

// Access levels of cluster RPCs.
const (
	// Anonymous allows callers without certificate (Join only).
	Anonymous Access = iota
	// AnyNode requires a valid certificate of any role (RenewCertificate).
	AnyNode
	// VoterOnly requires a voter certificate (Apply, GetStatus, replication).
	VoterOnly
)

// Authorize returns a unary interceptor that enforces the access level of
// each method; unknown methods are denied.
func Authorize(levels map[string]Access) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := authorize(ctx, levels, info.FullMethod); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// AuthorizeStream is Authorize for streaming methods: the caller is checked
// once when the stream opens, before the handler runs.
func AuthorizeStream(levels map[string]Access) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := authorize(ss.Context(), levels, info.FullMethod); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

func authorize(ctx context.Context, levels map[string]Access, method string) error {
	level, known := levels[method]
	if !known {
		return status.Error(codes.PermissionDenied, "method not allowed on the cluster port")
	}
	if level == Anonymous {
		return nil
	}
	id, ok := Peer(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "client certificate required")
	}
	if level == VoterOnly && id.Role != pki.RoleVoter {
		return status.Error(codes.PermissionDenied, "voter certificate required")
	}
	return nil
}

// DialVoter opens a gRPC connection to another voter with this node's
// certificate; the server must be a voter of this cluster.
func DialVoter(address string, sec Security) (*grpc.ClientConn, error) {
	cfg := pki.ClientConfig(sec.Holder, sec.Verifier, "", pki.OnlyVoters, ALPNGRPC)
	return grpc.NewClient(address,
		grpc.WithTransportCredentials(credentials.NewTLS(cfg)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 15 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: true}),
	)
}
