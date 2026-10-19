// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package tcpcheck

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/joinsider/uptimecluster/internal/checker"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func run(t *testing.T, c *Checker, env *checker.Env, cfg map[string]any) checker.Result {
	t.Helper()
	raw, _ := json.Marshal(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.Check(ctx, env, raw)
}

func hostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, p, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(p)
	return host, port
}

func TestTCP(t *testing.T) {
	env := &checker.Env{Dialer: &checker.Dialer{}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	host, port := hostPort(t, ln.Addr().String())

	res := run(t, &Checker{}, env, map[string]any{"host": host, "port": port})
	if res.Status != checker.StatusUp || res.Details["resolvedIp"] != "127.0.0.1" {
		t.Fatalf("open port = %+v", res)
	}
	_ = ln.Close()
	res = run(t, &Checker{}, env, map[string]any{"host": host, "port": port})
	if res.Status != checker.StatusDown || res.ErrorClass != checker.ClassConnect {
		t.Fatalf("closed port = %s/%s %s", res.Status, res.ErrorClass, res.Message)
	}

	guarded := &checker.Env{Dialer: &checker.Dialer{Policy: checker.BlockPrivate{}}}
	if res := run(t, &Checker{}, guarded, map[string]any{"host": host, "port": port}); res.ErrorClass != checker.ClassSSRFBlocked {
		t.Errorf("blocked target = %s/%s", res.Status, res.ErrorClass)
	}
}

func TestTCPWithTLS(t *testing.T) {
	srv := httptest.NewTLSServer(nil) // self-signed certificate for 127.0.0.1 and example.com
	defer srv.Close()
	host, port := hostPort(t, srv.Listener.Addr().String())
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	env := &checker.Env{Dialer: &checker.Dialer{}}

	res := run(t, &Checker{RootCAs: pool}, env, map[string]any{"host": host, "port": port, "tls": true})
	if res.Status != checker.StatusUp || res.Details["tlsVersion"] == nil {
		t.Fatalf("trusted TLS = %+v", res)
	}
	res = run(t, &Checker{RootCAs: pool}, env, map[string]any{"host": host, "port": port, "tls": true, "tlsServerName": "wrong.example"})
	if res.Status != checker.StatusDown || res.ErrorClass != checker.ClassTLS {
		t.Errorf("wrong server name = %s/%s", res.Status, res.ErrorClass)
	}
	res = run(t, &Checker{}, env, map[string]any{"host": host, "port": port, "tls": true})
	if res.Status != checker.StatusDown || res.ErrorClass != checker.ClassTLS {
		t.Errorf("untrusted TLS = %s/%s", res.Status, res.ErrorClass)
	}
	res = run(t, &Checker{}, env, map[string]any{"host": host, "port": port, "tls": true, "ignoreTlsErrors": true})
	if res.Status != checker.StatusUp {
		t.Errorf("ignored TLS errors = %s: %s", res.Status, res.Message)
	}
}

func TestValidation(t *testing.T) {
	reg := checker.NewRegistry()
	reg.Register(&Checker{})
	if err := reg.Validate(Type, json.RawMessage(`{"host":"db.internal","port":5432,"tls":true}`)); err != nil {
		t.Errorf("valid config: %v", err)
	}
	for _, cfg := range []string{`{"host":"x"}`, `{"host":"x","port":70000}`, `{"host":"0x7f000001","port":1}`, `{"host":"x","port":1,"foo":1}`} {
		if err := reg.Validate(Type, json.RawMessage(cfg)); err == nil {
			t.Errorf("%s accepted", cfg)
		}
	}
}
