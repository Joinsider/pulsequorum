// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package clusternet serves the cluster port (ADR 0006): one TLS 1.3 listener
// with mutual authentication against the cluster CA, multiplexed by ALPN into
// Raft ("uc-raft/1", voters only) and gRPC ("h2", authorized per RPC).
package clusternet

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/raft"

	"github.com/joinsider/pulsequorum/internal/pki"
)

// ALPN protocols of the cluster port.
const (
	ALPNRaft = "uc-raft/1"
	ALPNGRPC = "h2"
)

const (
	handshakeTimeout = 10 * time.Second
	// MaxConnectionAge closes Raft connections after this time, so renewed
	// and revoked certificates take effect on long-lived peers (FA-PKI-03).
	MaxConnectionAge = time.Hour
	acceptQueue      = 64
)

// Security is the node's TLS material for the cluster port.
type Security struct {
	Holder   *pki.Holder
	Verifier *pki.Verifier
}

// Mux accepts TLS connections and hands them to Raft or gRPC.
type Mux struct {
	ln        net.Listener
	sec       Security
	advertise string
	log       *slog.Logger

	raft chan net.Conn
	grpc chan net.Conn
	done chan struct{}
	once sync.Once
}

// Listen opens the cluster port. advertise is the address other nodes use
// for this node; Raft announces it.
func Listen(addr, advertise string, sec Security, log *slog.Logger) (*Mux, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on cluster port %s: %w", addr, err)
	}
	return &Mux{
		ln: ln, sec: sec, advertise: advertise, log: log,
		raft: make(chan net.Conn, acceptQueue), grpc: make(chan net.Conn, acceptQueue), done: make(chan struct{}),
	}, nil
}

// Addr is the bound address (useful with port 0 in tests).
func (m *Mux) Addr() net.Addr { return m.ln.Addr() }

// Serve accepts connections until Close.
func (m *Mux) Serve() error {
	// Client certificates are optional at the TLS level so that Join works
	// without one; a presented certificate is always verified, Raft requires
	// a voter certificate below and gRPC authorizes per RPC.
	cfg := pki.ServerConfig(m.sec.Holder, m.sec.Verifier, false, nil, ALPNGRPC, ALPNRaft)
	for {
		c, err := m.ln.Accept()
		if err != nil {
			select {
			case <-m.done:
				return nil
			default:
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return fmt.Errorf("accept on cluster port: %w", err)
		}
		go m.dispatch(tls.Server(c, cfg))
	}
}

func (m *Mux) dispatch(conn *tls.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer cancel()
	if err := conn.HandshakeContext(ctx); err != nil {
		m.log.Debug("cluster port handshake failed", "remote", conn.RemoteAddr().String(), "err", err)
		_ = conn.Close()
		return
	}
	state := conn.ConnectionState()
	var target chan net.Conn
	switch state.NegotiatedProtocol {
	case ALPNRaft:
		if err := requireVoter(state); err != nil {
			m.log.Warn("rejected raft connection", "remote", conn.RemoteAddr().String(), "err", err)
			_ = conn.Close()
			return
		}
		target = m.raft
	case ALPNGRPC:
		target = m.grpc
	default:
		_ = conn.Close()
		return
	}
	select {
	case target <- conn:
	case <-m.done:
		_ = conn.Close()
	}
}

// requireVoter accepts a connection whose verified peer is a voter. The
// chain was verified in VerifyConnection; this only reads the identity.
func requireVoter(state tls.ConnectionState) error {
	if len(state.PeerCertificates) == 0 {
		return pki.ErrNoPeerCertificate
	}
	id, err := pki.IdentityOf(state.PeerCertificates[0])
	if err != nil {
		return err
	}
	return pki.OnlyVoters(id)
}

// Close stops accepting and closes the listener.
func (m *Mux) Close() error {
	var err error
	m.once.Do(func() {
		close(m.done)
		err = m.ln.Close()
	})
	return err
}

// GRPCListener returns the connections negotiated for gRPC, unwrapped
// *tls.Conn as gRPC needs them (spike finding, ADR 0006).
func (m *Mux) GRPCListener() net.Listener {
	return &chanListener{ch: m.grpc, done: m.done, closed: make(chan struct{}), addr: m.ln.Addr()}
}

// chanListener hands out the gRPC connections. Closing it (grpc.Server.Stop)
// ends Accept without closing the shared port; closing the Mux ends it too.
type chanListener struct {
	ch     chan net.Conn
	done   chan struct{}
	closed chan struct{}
	once   sync.Once
	addr   net.Addr
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return l.addr }

// RaftLayer returns the raft.StreamLayer of this port.
func (m *Mux) RaftLayer() raft.StreamLayer { return &raftLayer{m: m} }

type raftLayer struct{ m *Mux }

var _ raft.StreamLayer = (*raftLayer)(nil)

func (l *raftLayer) Accept() (net.Conn, error) {
	select {
	case c := <-l.m.raft:
		return withMaxAge(c), nil
	case <-l.m.done:
		return nil, net.ErrClosed
	}
}

func (l *raftLayer) Close() error { return l.m.Close() }

// Addr is the advertised address: Raft stores it in the configuration.
func (l *raftLayer) Addr() net.Addr { return advertiseAddr(l.m.advertise) }

// Dial connects to another voter; only a voter of this cluster is accepted
// as server.
func (l *raftLayer) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	cfg := pki.ClientConfig(l.m.sec.Holder, l.m.sec.Verifier, "", pki.OnlyVoters, ALPNRaft)
	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: timeout}, Config: cfg}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c, err := d.DialContext(ctx, "tcp", string(address))
	if err != nil {
		return nil, err
	}
	if p := c.(*tls.Conn).ConnectionState().NegotiatedProtocol; p != ALPNRaft {
		_ = c.Close()
		return nil, fmt.Errorf("peer %s did not negotiate %s", address, ALPNRaft)
	}
	return withMaxAge(c), nil
}

type advertiseAddr string

func (a advertiseAddr) Network() string { return "tcp" }
func (a advertiseAddr) String() string  { return string(a) }

// withMaxAge closes a connection after MaxConnectionAge; Raft dials again
// and the new handshake uses current certificates and revocations.
func withMaxAge(c net.Conn) net.Conn {
	t := time.AfterFunc(MaxConnectionAge, func() { _ = c.Close() })
	return &agedConn{Conn: c, timer: t}
}

type agedConn struct {
	net.Conn
	timer *time.Timer
}

func (c *agedConn) Close() error {
	c.timer.Stop()
	return c.Conn.Close()
}
