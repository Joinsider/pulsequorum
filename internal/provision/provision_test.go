// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package provision

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/uptimecluster/internal/auth"
	"github.com/joinsider/uptimecluster/internal/cluster"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
)

// fakeApplier records the proposed commands and answers with fn.
type fakeApplier struct {
	calls []*pb.Command
	fn    func(cmd *pb.Command) (cluster.ApplyResult, error)
}

func (f *fakeApplier) Apply(_ context.Context, cmd *pb.Command) (cluster.ApplyResult, error) {
	f.calls = append(f.calls, cmd)
	return f.fn(cmd)
}

// created answers every command with a success for the tenant ID of the command.
func created(cmd *pb.Command) (cluster.ApplyResult, error) {
	b, err := json.Marshal(cluster.TenantResult{TenantID: cmd.GetCreateTenant().GetTenantId()})
	return cluster.ApplyResult{Result: b}, err
}

// fixedClock is a fake clock for the rate limit and the block of an organization.
type fixedClock struct{ now time.Time }

func (c *fixedClock) Now() time.Time          { return c.now }
func (c *fixedClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestProvisioner(t *testing.T, app *fakeApplier, clk *fixedClock) *Provisioner {
	t.Helper()
	p, err := NewProvisioner(ProvisionerOptions{
		Cluster: app, PlanID: "starter", Max: DefaultMax,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: clk.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewProvisionerValidation(t *testing.T) {
	app := &fakeApplier{fn: created}
	tests := []struct {
		name string
		opts ProvisionerOptions
		ok   bool
	}{
		{"valid", ProvisionerOptions{Cluster: app, PlanID: "starter", Max: 1}, true},
		{"nil cluster", ProvisionerOptions{PlanID: "starter", Max: 1}, false},
		{"system plan", ProvisionerOptions{Cluster: app, PlanID: "system", Max: 1}, false},
		{"empty plan", ProvisionerOptions{Cluster: app, PlanID: "", Max: 1}, false},
		{"invalid plan ID", ProvisionerOptions{Cluster: app, PlanID: "Starter", Max: 1}, false},
		{"maximum zero", ProvisionerOptions{Cluster: app, PlanID: "starter", Max: 0}, false},
		{"maximum negative", ProvisionerOptions{Cluster: app, PlanID: "starter", Max: -1}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewProvisioner(tt.opts)
			if (err == nil) != tt.ok {
				t.Fatalf("err = %v, want success %v", err, tt.ok)
			}
		})
	}
}

func TestProvisionRejectsInvalidOrganization(t *testing.T) {
	for _, org := range []string{"", "org 1", "org/1", "äb", strings.Repeat("x", 129)} {
		t.Run(org, func(t *testing.T) {
			app := &fakeApplier{fn: created}
			p := newTestProvisioner(t, app, &fixedClock{now: time.Unix(0, 0)})
			_, err := p.Provision(context.Background(), org, "Name")
			if !errors.Is(err, auth.ErrForbidden) {
				t.Fatalf("err = %v, want ErrForbidden", err)
			}
			if len(app.calls) != 0 {
				t.Errorf("%d commands proposed for an invalid organization", len(app.calls))
			}
		})
	}
}

func TestProvisionCreatesTenant(t *testing.T) {
	app := &fakeApplier{fn: created}
	p := newTestProvisioner(t, app, &fixedClock{now: time.Unix(0, 0)})
	id, err := p.Provision(context.Background(), "org-1", "Acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(app.calls) != 1 {
		t.Fatalf("%d commands proposed, want 1", len(app.calls))
	}
	cmd := app.calls[0]
	if cmd.GetActor().GetKind() != pb.Actor_KIND_SYSTEM {
		t.Errorf("actor = %v, want system", cmd.GetActor().GetKind())
	}
	ct := cmd.GetCreateTenant()
	if ct == nil {
		t.Fatalf("payload = %T, want CreateTenant", cmd.GetPayload())
	}
	if ct.GetSource() != "auto" || ct.GetPlanId() != "starter" || ct.GetMaxAutoTenants() != DefaultMax || ct.GetOrgId() != "org-1" {
		t.Errorf("command = %+v; want source auto, plan starter, max %d, org org-1", ct, DefaultMax)
	}
	if _, err := uuid.Parse(ct.GetTenantId()); err != nil {
		t.Errorf("tenant ID %q is not a UUID: %v", ct.GetTenantId(), err)
	}
	if id != ct.GetTenantId() {
		t.Errorf("returned %q, want the created tenant %q", id, ct.GetTenantId())
	}
}

func TestProvisionReturnsTenantFromResult(t *testing.T) {
	app := &fakeApplier{fn: func(*pb.Command) (cluster.ApplyResult, error) {
		b, _ := json.Marshal(cluster.TenantResult{TenantID: "t-new"})
		return cluster.ApplyResult{Result: b}, nil
	}}
	p := newTestProvisioner(t, app, &fixedClock{now: time.Unix(0, 0)})
	id, err := p.Provision(context.Background(), "org-1", "Acme")
	if err != nil || id != "t-new" {
		t.Fatalf("Provision = %q, %v; want t-new", id, err)
	}
}

func TestProvisionExistingOrganization(t *testing.T) {
	app := &fakeApplier{fn: func(*pb.Command) (cluster.ApplyResult, error) {
		b, _ := json.Marshal(cluster.TenantResult{TenantID: "t-existing", Existing: true})
		return cluster.ApplyResult{Result: b}, nil
	}}
	p := newTestProvisioner(t, app, &fixedClock{now: time.Unix(0, 0)})
	id, err := p.Provision(context.Background(), "org-1", "Acme")
	if err != nil || id != "t-existing" {
		t.Fatalf("Provision = %q, %v; want t-existing", id, err)
	}
}

func TestProvisionSanitizesName(t *testing.T) {
	tests := []struct {
		name, orgName, want string
	}{
		{"plain name", "Acme", "Acme"},
		{"control characters removed", "Acme\x07 GmbH", "Acme GmbH"},
		{"control characters inside a word removed", "Ac\x00me", "Acme"},
		{"format character removed", "Ac\u200bme", "Acme"},
		{"surrounding space collapsed", "  Acme \n  GmbH  ", "Acme GmbH"},
		{"empty name falls back to organization ID", "", "org-1"},
		{"only control characters fall back to organization ID", "\x00\x01", "org-1"},
		{"long name cut to 100 characters", strings.Repeat("a", 150), strings.Repeat("a", 100)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := &fakeApplier{fn: created}
			p := newTestProvisioner(t, app, &fixedClock{now: time.Unix(0, 0)})
			if _, err := p.Provision(context.Background(), "org-1", tt.orgName); err != nil {
				t.Fatal(err)
			}
			if got := app.calls[0].GetCreateTenant().GetName(); got != tt.want {
				t.Errorf("name = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProvisionCommandRejectionBlocksOrganization(t *testing.T) {
	outcomes := map[string]func(*pb.Command) (cluster.ApplyResult, error){
		"in the result": func(*pb.Command) (cluster.ApplyResult, error) {
			return cluster.ApplyResult{Err: &cluster.CommandError{Code: cluster.CodeLimitExceeded, Message: "automatic provisioning is limited"}}, nil
		},
		"as the error": func(*pb.Command) (cluster.ApplyResult, error) {
			return cluster.ApplyResult{}, &cluster.CommandError{Code: cluster.CodeConflict, Message: "bound"}
		},
	}
	for name, fn := range outcomes {
		t.Run("rejection "+name, func(t *testing.T) {
			clk := &fixedClock{now: time.Unix(0, 0)}
			app := &fakeApplier{fn: fn}
			p := newTestProvisioner(t, app, clk)
			ctx := context.Background()

			_, err := p.Provision(ctx, "org-1", "Acme")
			if !errors.Is(err, auth.ErrForbidden) || errors.Is(err, auth.ErrAuthUnavailable) {
				t.Fatalf("err = %v, want ErrForbidden", err)
			}
			if len(app.calls) != 1 {
				t.Fatalf("%d commands, want 1", len(app.calls))
			}

			// Blocked for 60 seconds: no command, no answer about the cause.
			clk.Advance(59 * time.Second)
			if _, err := p.Provision(ctx, "org-1", "Acme"); !errors.Is(err, auth.ErrForbidden) {
				t.Fatalf("blocked err = %v, want ErrForbidden", err)
			}
			if len(app.calls) != 1 {
				t.Fatalf("blocked organization proposed again (%d commands)", len(app.calls))
			}

			// Another organization is not blocked.
			app.fn = created
			if _, err := p.Provision(ctx, "org-2", "Other"); err != nil {
				t.Fatalf("other organization: %v", err)
			}

			// After 60 seconds the organization may be retried.
			clk.Advance(time.Second)
			if _, err := p.Provision(ctx, "org-1", "Acme"); err != nil {
				t.Fatalf("after the block: %v", err)
			}
		})
	}
}

func TestProvisionNotAppliedIsUnavailableAndNotBlocked(t *testing.T) {
	failures := map[string]func(*pb.Command) (cluster.ApplyResult, error){
		"not applied": func(*pb.Command) (cluster.ApplyResult, error) { return cluster.ApplyResult{}, cluster.ErrNotApplied },
		"fsm halted": func(*pb.Command) (cluster.ApplyResult, error) {
			return cluster.ApplyResult{Err: cluster.ErrFSMHalted}, nil
		},
		"no tenant ID": func(*pb.Command) (cluster.ApplyResult, error) { return cluster.ApplyResult{Result: []byte(`{}`)}, nil },
		"result is not JSON": func(*pb.Command) (cluster.ApplyResult, error) {
			return cluster.ApplyResult{Result: []byte("not json")}, nil
		},
	}
	for name, fn := range failures {
		t.Run(name, func(t *testing.T) {
			clk := &fixedClock{now: time.Unix(0, 0)}
			app := &fakeApplier{fn: fn}
			p := newTestProvisioner(t, app, clk)
			_, err := p.Provision(context.Background(), "org-1", "Acme")
			if !errors.Is(err, auth.ErrAuthUnavailable) || errors.Is(err, auth.ErrForbidden) {
				t.Fatalf("err = %v, want ErrAuthUnavailable", err)
			}
			// Not blocked: the next login retries at once.
			app.fn = created
			if _, err := p.Provision(context.Background(), "org-1", "Acme"); err != nil {
				t.Fatalf("retry: %v", err)
			}
			if len(app.calls) != 2 {
				t.Errorf("%d commands, want 2 (retry not blocked)", len(app.calls))
			}
		})
	}
}

func TestProvisionRateLimit(t *testing.T) {
	clk := &fixedClock{now: time.Unix(0, 0)}
	app := &fakeApplier{fn: created}
	p := newTestProvisioner(t, app, clk)
	ctx := context.Background()

	for i := range 10 {
		if _, err := p.Provision(ctx, "org-"+string(rune('a'+i)), "Org"); err != nil {
			t.Fatalf("provisioning %d: %v", i, err)
		}
	}
	if _, err := p.Provision(ctx, "org-k", "Org"); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("11th err = %v, want ErrForbidden", err)
	}
	if len(app.calls) != 10 {
		t.Fatalf("%d commands after the rate limit, want 10", len(app.calls))
	}

	clk.Advance(time.Minute)
	if _, err := p.Provision(ctx, "org-k", "Org"); err != nil {
		t.Fatalf("after one minute: %v", err)
	}
	if len(app.calls) != 11 {
		t.Errorf("%d commands, want 11", len(app.calls))
	}
}
