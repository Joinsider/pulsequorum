// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/joinsider/uptimecluster/internal/api/gen"
	"github.com/joinsider/uptimecluster/internal/config"

	"github.com/joinsider/uptimecluster/internal/auth"
	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/store"
)

// TestStreamRevalidateWithoutLocalUsers covers the OIDC mode (ADR 0027): the
// stream stays open while the tenant exists and is active.
func TestStreamRevalidateWithoutLocalUsers(t *testing.T) {
	e := newEnvTweaked(t, false, func(d *Deps) { d.Local = nil })
	ctx := context.Background()
	h := &Handlers{deps: Deps{State: e.state}}
	viewer := func(tenant string) auth.Principal {
		return auth.Principal{UserID: "312345678901234567", TenantID: tenant, Roles: []auth.Role{auth.RoleTenantViewer}}
	}
	if !h.revalidate(ctx, viewer(cluster.DefaultTenantID)) {
		t.Fatal("active tenant: stream closed")
	}
	if h.revalidate(ctx, viewer("no-such-tenant")) {
		t.Fatal("missing tenant: stream kept open")
	}
	if h.revalidate(ctx, viewer(store.SystemTenantID)) {
		t.Fatal("system tenant: stream kept open")
	}
	cmd, _ := cluster.NewPutTenant("suspend-tenant-b", "tenant-b", cluster.TenantDocument{Name: "B", Plan: "default", Suspended: true})
	if _, err := e.node.Apply(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	if h.revalidate(ctx, viewer("tenant-b")) {
		t.Fatal("suspended tenant: stream kept open")
	}
}

// TestLocalEndpointsInOIDCMode: local login and user management answer 409
// while an identity provider manages the users (ADR 0027, option 4A).
func TestLocalEndpointsInOIDCMode(t *testing.T) {
	h := &Handlers{cfg: config.Config{AuthMode: config.AuthOIDC}}
	if _, err := h.localAuth(); !isConflict(err) {
		t.Fatalf("localAuth: err = %v, want 409", err)
	}
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{UserID: "u1", TenantID: cluster.DefaultTenantID,
		Roles: []auth.Role{auth.RolePlatformAdmin}})
	calls := map[string]func() error{
		"ListUsers":  func() error { _, err := h.ListUsers(ctx, gen.ListUsersRequestObject{}); return err },
		"CreateUser": func() error { _, err := h.CreateUser(ctx, gen.CreateUserRequestObject{}); return err },
		"UpdateUser": func() error { _, err := h.UpdateUser(ctx, gen.UpdateUserRequestObject{}); return err },
		"DeleteUser": func() error { _, err := h.DeleteUser(ctx, gen.DeleteUserRequestObject{}); return err },
		"Login":      func() error { _, err := h.Login(ctx, gen.LoginRequestObject{}); return err },
		"Setup":      func() error { _, err := h.Setup(ctx, gen.SetupRequestObject{}); return err },
		"Refresh":    func() error { _, err := h.Refresh(ctx, gen.RefreshRequestObject{}); return err },
	}
	for name, call := range calls {
		if err := call(); !isConflict(err) {
			t.Errorf("%s: err = %v, want 409", name, err)
		}
	}
}

func isConflict(err error) bool {
	var p *Problem
	return errors.As(err, &p) && p.Status == http.StatusConflict
}
