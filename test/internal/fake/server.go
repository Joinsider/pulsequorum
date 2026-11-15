// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package fake

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Server is an HTTP server on a host port, started in the background.
type Server struct {
	srv *http.Server
	ln  net.Listener
}

// Listen binds addr (for example ":18950") and serves h in the background.
func Listen(addr string, h http.Handler) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return &Server{srv: srv, ln: ln}, nil
}

// Port returns the bound TCP port.
func (s *Server) Port() int {
	if addr, ok := s.ln.Addr().(*net.TCPAddr); ok {
		return addr.Port
	}
	return 0
}

// Close stops the server and waits up to five seconds for open requests.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}
