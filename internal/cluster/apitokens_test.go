// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import (
	"fmt"
	"strings"
	"testing"
	"time"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
)

// tokenIssuedMs is the command time of the API token tests.
const tokenIssuedMs = int64(1_000_000)

// tokenFSM returns an FSM at command level 9 with the system tenant and the
// tenants t1 and t2, both on the default plan.
func tokenFSM(t *testing.T) *alertFSM {
	t.Helper()
	return tokenFSMAt(t, AuditCommandLevel)
}

// tokenFSMAt is tokenFSM at the given command level. A level only grows, so a
// lower level needs a new FSM.
func tokenFSMAt(t *testing.T, level uint32) *alertFSM {
	t.Helper()
	h := newAlertFSM(t, t.TempDir())
	h.setLevel(level)
	h.putSystemTenant(1000)
	h.putTenant(1000, "t1")
	h.putTenant(1000, "t2")
	return h
}

// tokID returns a valid public token ID for i: "tok_" and 16 letters.
func tokID(i int) string {
	b := []byte("tok_aaaaaaaaaaaaaaaa")
	for p := len(b) - 1; i > 0 && p >= 4; p-- {
		b[p] = 'a' + byte(i%26)
		i /= 26
	}
	return string(b)
}

// validTokenPayload returns a valid CreateAPIToken number i, expiring one day
// after tokenIssuedMs.
func validTokenPayload(i int) *pb.CreateAPIToken {
	return &pb.CreateAPIToken{
		Id:          tokID(i),
		Scope:       APITokenScopeTenant,
		Name:        fmt.Sprintf("token %d", i),
		Role:        "tenant-editor",
		SecretHash:  sha256Hex(fmt.Sprintf("secret-%d", i)),
		ExpiresAtMs: tokenIssuedMs + (24 * time.Hour).Milliseconds(),
	}
}

// userActor is the signed-in user of the API token tests.
func userActor() *pb.Actor {
	return &pb.Actor{Kind: pb.Actor_KIND_USER, Id: "u-admin", Name: "Ada"}
}

// createTokenCmd builds a CreateAPIToken command of tenantID by the user.
func createTokenCmd(requestID, tenantID string, p *pb.CreateAPIToken) *pb.Command {
	return NewCreateAPIToken(requestID, tenantID, userActor(), p)
}

// tokens lists the API tokens of tenantID or fails the test.
func (h *alertFSM) tokens(tenantID string) []store.APIToken {
	h.t.Helper()
	list, err := h.st.ListAPITokens(h.ctx, tenantID)
	if err != nil {
		h.t.Fatal(err)
	}
	return list
}

// TestCreateAPIToken covers ADR 0031, section 1: every validation rule of a
// token, with a fresh FSM per case so that names and IDs never collide.
func TestCreateAPIToken(t *testing.T) {
	long := strings.Repeat("x", 64)
	tests := []struct {
		name   string
		mutate func(p *pb.CreateAPIToken)
		code   string
	}{
		{"valid", func(*pb.CreateAPIToken) {}, ""},
		{"name of 64 runes", func(p *pb.CreateAPIToken) { p.Name = long }, ""},
		{"name of 64 multi-byte runes", func(p *pb.CreateAPIToken) { p.Name = strings.Repeat("ü", 64) }, ""},
		{"role tenant-viewer", func(p *pb.CreateAPIToken) { p.Role = "tenant-viewer" }, ""},
		{"role tenant-admin", func(p *pb.CreateAPIToken) { p.Role = "tenant-admin" }, ""},
		{"lifetime of exactly one hour", func(p *pb.CreateAPIToken) { p.ExpiresAtMs = tokenIssuedMs + time.Hour.Milliseconds() }, ""},
		{"lifetime of exactly 366 days", func(p *pb.CreateAPIToken) {
			p.ExpiresAtMs = tokenIssuedMs + MaxAPITokenLifetime.Milliseconds()
		}, ""},

		{"ID without prefix", func(p *pb.CreateAPIToken) { p.Id = "abc_" + tokID(0)[4:] }, CodeInvalid},
		{"ID with 15 characters", func(p *pb.CreateAPIToken) { p.Id = tokID(0)[:19] }, CodeInvalid},
		{"ID with 17 characters", func(p *pb.CreateAPIToken) { p.Id = tokID(0) + "a" }, CodeInvalid},
		{"ID with upper-case letters", func(p *pb.CreateAPIToken) { p.Id = "tok_AAAAAAAAAAAAAAAA" }, CodeInvalid},
		{"ID with digits outside base32", func(p *pb.CreateAPIToken) { p.Id = "tok_aaaaaaaaaaaaaaa0" }, CodeInvalid},
		{"empty ID", func(p *pb.CreateAPIToken) { p.Id = "" }, CodeInvalid},
		{"scope platform", func(p *pb.CreateAPIToken) { p.Scope = "platform" }, CodeInvalid},
		{"empty scope", func(p *pb.CreateAPIToken) { p.Scope = "" }, CodeInvalid},
		{"role platform-admin", func(p *pb.CreateAPIToken) { p.Role = "platform-admin" }, CodeInvalid},
		{"role unknown", func(p *pb.CreateAPIToken) { p.Role = "owner" }, CodeInvalid},
		{"empty name", func(p *pb.CreateAPIToken) { p.Name = "" }, CodeInvalid},
		{"name of 65 runes", func(p *pb.CreateAPIToken) { p.Name = long + "x" }, CodeInvalid},
		{"name with a control character", func(p *pb.CreateAPIToken) { p.Name = "ci\tbot" }, CodeInvalid},
		{"name of spaces only", func(p *pb.CreateAPIToken) { p.Name = "   " }, CodeInvalid},
		{"secret hash in upper-case hex", func(p *pb.CreateAPIToken) { p.SecretHash = strings.ToUpper(p.SecretHash) }, CodeInvalid},
		{"secret hash too short", func(p *pb.CreateAPIToken) { p.SecretHash = p.SecretHash[:63] }, CodeInvalid},
		{"secret hash not hex", func(p *pb.CreateAPIToken) { p.SecretHash = strings.Repeat("g", 64) }, CodeInvalid},
		{"lifetime of 59 minutes", func(p *pb.CreateAPIToken) {
			p.ExpiresAtMs = tokenIssuedMs + (59 * time.Minute).Milliseconds()
		}, CodeInvalid},
		{"lifetime of 367 days", func(p *pb.CreateAPIToken) {
			p.ExpiresAtMs = tokenIssuedMs + MaxAPITokenLifetime.Milliseconds() + 1
		}, CodeInvalid},
		{"expiry in the past", func(p *pb.CreateAPIToken) { p.ExpiresAtMs = tokenIssuedMs - 1 }, CodeInvalid},
		{"no expiry", func(p *pb.CreateAPIToken) { p.ExpiresAtMs = 0 }, CodeInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := tokenFSM(t)
			p := validTokenPayload(1)
			tt.mutate(p)
			res := h.apply(createTokenCmd("create-"+tt.name, "t1", p), tokenIssuedMs)
			checkCode(t, res.Err, tt.code)
			if tt.code == "" {
				if got := len(h.tokens("t1")); got != 1 {
					t.Fatalf("stored tokens = %d, want 1", got)
				}
			} else if got := len(h.tokens("t1")); got != 0 {
				t.Fatalf("rejected token was stored: %d tokens", got)
			}
		})
	}
}

// TestCreateAPITokenStoresHashOnly checks the stored record: the hash only, the
// creator, and the times from the command.
func TestCreateAPITokenStoresHashOnly(t *testing.T) {
	h := tokenFSM(t)
	p := validTokenPayload(7)
	res := h.apply(createTokenCmd("create-7", "t1", p), tokenIssuedMs)
	checkCode(t, res.Err, "")
	list := h.tokens("t1")
	if len(list) != 1 {
		t.Fatalf("stored tokens = %d, want 1", len(list))
	}
	got := list[0]
	want := store.APIToken{
		ID: tokID(7), TenantID: "t1", Scope: APITokenScopeTenant, Name: "token 7", Role: "tenant-editor",
		SecretHash: sha256Hex("secret-7"), CreatedMs: tokenIssuedMs, ExpiresMs: p.GetExpiresAtMs(),
		CreatedByKind: "user", CreatedByID: "u-admin", CreatedByName: "Ada",
	}
	if got != want {
		t.Errorf("stored token = %+v\nwant %+v", got, want)
	}
	if res.Result == nil || !strings.Contains(string(res.Result), `"id":"`+tokID(7)+`"`) {
		t.Errorf("result = %s, want the token ID", res.Result)
	}
}

// TestAPITokenActorsAndTenant covers the actor rule (only signed-in users),
// the tenant rules (present, not the system tenant, existing, not suspended)
// and the command level.
func TestAPITokenActorsAndTenant(t *testing.T) {
	tests := []struct {
		name   string
		cmd    func(*alertFSM) *pb.Command
		code   string
		prep   func(h *alertFSM)
		wantOK bool
	}{
		{"user", func(*alertFSM) *pb.Command { return createTokenCmd("c", "t1", validTokenPayload(1)) }, "", nil, true},
		{"API token actor", func(*alertFSM) *pb.Command {
			return NewCreateAPIToken("c", "t1", &pb.Actor{Kind: pb.Actor_KIND_API_TOKEN, Id: tokID(2)}, validTokenPayload(1))
		}, CodeInvalid, nil, false},
		{"system actor", func(*alertFSM) *pb.Command {
			return NewCreateAPIToken("c", "t1", &pb.Actor{Kind: pb.Actor_KIND_SYSTEM}, validTokenPayload(1))
		}, CodeInvalid, nil, false},
		{"user without ID", func(*alertFSM) *pb.Command {
			return NewCreateAPIToken("c", "t1", &pb.Actor{Kind: pb.Actor_KIND_USER}, validTokenPayload(1))
		}, CodeInvalid, nil, false},
		{"no tenant", func(*alertFSM) *pb.Command { return createTokenCmd("c", "", validTokenPayload(1)) }, CodeInvalid, nil, false},
		{"system tenant", func(*alertFSM) *pb.Command {
			return createTokenCmd("c", store.SystemTenantID, validTokenPayload(1))
		}, CodeInvalid, nil, false},
		{"unknown tenant", func(*alertFSM) *pb.Command { return createTokenCmd("c", "t9", validTokenPayload(1)) }, CodeNotFound, nil, false},
		{"suspended tenant", func(*alertFSM) *pb.Command { return createTokenCmd("c", "t1", validTokenPayload(1)) }, CodeInvalid,
			func(h *alertFSM) {
				h.adminDo(1500, "suspend-t1", actorSystem, &pb.SetTenantSuspended{TenantId: "t1", Suspended: true}, "")
			}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := tokenFSM(t)
			if tt.prep != nil {
				tt.prep(h)
			}
			res := h.apply(tt.cmd(h), tokenIssuedMs)
			checkCode(t, res.Err, tt.code)
			wantCount := 0
			if tt.wantOK {
				wantCount = 1
			}
			if got := len(h.tokens("t1")); got != wantCount {
				t.Errorf("stored tokens = %d, want %d", got, wantCount)
			}
		})
	}
}

// TestAPITokenLimit covers the limit of 50 tokens per tenant (a fixed FSM
// limit, not a plan limit) and a freed slot after a revocation.
func TestAPITokenLimit(t *testing.T) {
	h := tokenFSM(t)
	for i := range MaxAPITokensPerTenant {
		checkCode(t, h.apply(createTokenCmd(fmt.Sprintf("create-%d", i), "t1", validTokenPayload(i)), tokenIssuedMs).Err, "")
	}
	res := h.apply(createTokenCmd("create-over", "t1", validTokenPayload(MaxAPITokensPerTenant)), tokenIssuedMs)
	checkCode(t, res.Err, CodeLimitExceeded)

	// Another tenant has its own limit.
	checkCode(t, h.apply(createTokenCmd("create-t2", "t2", validTokenPayload(1000)), tokenIssuedMs).Err, "")

	// A revocation frees a slot.
	checkCode(t, h.apply(NewRevokeAPIToken("revoke-0", "t1", userActor(), tokID(0)), tokenIssuedMs).Err, "")
	checkCode(t, h.apply(createTokenCmd("create-again", "t1", validTokenPayload(MaxAPITokensPerTenant)), tokenIssuedMs).Err, "")
}

// TestAPITokenNameIsUniquePerTenant covers a name taken in the tenant
// (CodeConflict) and the same name in another tenant (allowed).
func TestAPITokenNameIsUniquePerTenant(t *testing.T) {
	h := tokenFSM(t)
	checkCode(t, h.apply(createTokenCmd("create-1", "t1", validTokenPayload(1)), tokenIssuedMs).Err, "")

	dup := validTokenPayload(2)
	dup.Name = "token 1"
	res := h.apply(createTokenCmd("create-dup", "t1", dup), tokenIssuedMs)
	checkCode(t, res.Err, CodeConflict)
	if !strings.Contains(res.Err.Error(), "an API token with this name already exists") {
		t.Errorf("conflict message = %q", res.Err)
	}
	checkCode(t, h.apply(createTokenCmd("create-dup-t2", "t2", dup), tokenIssuedMs).Err, "")
}

// TestAPITokenNeedsLevel9 covers ADR 0018: below command level 9 the token and
// prune commands and Batch are rejected with CodeUnsupported and nothing changes.
func TestAPITokenNeedsLevel9(t *testing.T) {
	h := tokenFSMAt(t, AuditCommandLevel-1)
	checkCode(t, h.apply(createTokenCmd("create", "t1", validTokenPayload(1)), tokenIssuedMs).Err, CodeUnsupported)
	checkCode(t, h.apply(NewRevokeAPIToken("revoke", "t1", userActor(), tokID(1)), tokenIssuedMs).Err, CodeUnsupported)
	checkCode(t, h.apply(NewPruneAudit(tokenIssuedMs, tokenIssuedMs), tokenIssuedMs+1).Err, CodeUnsupported)
	cmd := NewBatch("batch", "t1", userActor(), []*pb.BatchOp{monitorOp("m1", 60, 1)})
	checkCode(t, h.apply(cmd, tokenIssuedMs).Err, CodeUnsupported)
	if n := len(h.tokens("t1")); n != 0 {
		t.Errorf("stored tokens = %d, want 0", n)
	}
}

// TestRevokeAPIToken covers revocation: the token is gone afterwards, unknown
// and foreign tokens are not found, and only signed-in users may revoke.
func TestRevokeAPIToken(t *testing.T) {
	tests := []struct {
		name   string
		tenant string
		id     string
		actor  *pb.Actor
		code   string
		gone   bool
	}{
		{"user revokes own token", "t1", tokID(1), userActor(), "", true},
		{"unknown token", "t1", tokID(9), userActor(), CodeNotFound, false},
		{"token of another tenant", "t2", tokID(1), userActor(), CodeNotFound, false},
		{"API token actor", "t1", tokID(1), &pb.Actor{Kind: pb.Actor_KIND_API_TOKEN, Id: tokID(1)}, CodeInvalid, false},
		{"system actor", "t1", tokID(1), &pb.Actor{Kind: pb.Actor_KIND_SYSTEM}, CodeInvalid, false},
		{"no tenant", "", tokID(1), userActor(), CodeInvalid, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := tokenFSM(t)
			checkCode(t, h.apply(createTokenCmd("create", "t1", validTokenPayload(1)), tokenIssuedMs).Err, "")
			res := h.apply(NewRevokeAPIToken("revoke", tt.tenant, tt.actor, tt.id), tokenIssuedMs+1)
			checkCode(t, res.Err, tt.code)
			remaining := len(h.tokens("t1"))
			if tt.gone && remaining != 0 {
				t.Errorf("token still stored after revocation")
			}
			if !tt.gone && remaining != 1 {
				t.Errorf("stored tokens of t1 = %d, want 1", remaining)
			}
		})
	}
}

// TestPruneAudit covers the system-only pruning of the audit log and of expired
// tokens: the cut-offs are checked against the command time, and only expired
// tokens and old entries go.
func TestPruneAudit(t *testing.T) {
	createdAt := tokenIssuedMs
	setup := func(t *testing.T) *alertFSM {
		t.Helper()
		h := tokenFSM(t)
		// Token 1 expires after one hour, token 2 after one year.
		short := validTokenPayload(1)
		short.ExpiresAtMs = createdAt + time.Hour.Milliseconds()
		checkCode(t, h.apply(createTokenCmd("create-short", "t1", short), createdAt).Err, "")
		long := validTokenPayload(2)
		long.ExpiresAtMs = createdAt + MaxAPITokenLifetime.Milliseconds()
		checkCode(t, h.apply(createTokenCmd("create-long", "t1", long), createdAt).Err, "")
		return h
	}

	t.Run("removes old entries and expired tokens", func(t *testing.T) {
		h := setup(t)
		if n := len(auditEntries(t, h)); n < 2 {
			t.Fatalf("audit entries before pruning = %d, want at least 2", n)
		}
		// Entries before createdAt+2h and tokens expired before createdAt+2h go.
		before := createdAt + (2 * time.Hour).Milliseconds()
		res := h.apply(NewPruneAudit(before, before), before+1000)
		checkCode(t, res.Err, "")
		if n := len(auditEntries(t, h)); n != 0 {
			t.Errorf("audit entries after pruning = %d, want 0", n)
		}
		list := h.tokens("t1")
		if len(list) != 1 || list[0].ID != tokID(2) {
			t.Errorf("tokens after pruning = %+v, want only %s", list, tokID(2))
		}
	})

	t.Run("cut-offs are checked against the command time", func(t *testing.T) {
		tests := []struct {
			name   string
			before int64
			tokens int64
			issued int64
			code   string
		}{
			{"audit cut-off zero", 0, 1, 2000, CodeInvalid},
			{"audit cut-off negative", -1, 1, 2000, CodeInvalid},
			{"audit cut-off after command", 3000, 1, 2000, CodeInvalid},
			{"token cut-off zero", 1, 0, 2000, CodeInvalid},
			{"token cut-off after command", 1, 3000, 2000, CodeInvalid},
			{"cut-offs equal to command time", 2000, 2000, 2000, ""},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				h := setup(t)
				cmd := NewPruneAudit(tt.before, tt.tokens)
				checkCode(t, h.apply(cmd, tt.issued).Err, tt.code)
			})
		}
	})

	t.Run("only the system may prune", func(t *testing.T) {
		for _, actor := range []*pb.Actor{userActor(), {Kind: pb.Actor_KIND_API_TOKEN, Id: tokID(1)}, nil} {
			h := setup(t)
			before := len(auditEntries(t, h))
			cmd := NewPruneAudit(createdAt+1, createdAt+1)
			cmd.Actor = actor
			checkCode(t, h.apply(cmd, createdAt+2000).Err, CodeInvalid)
			if n := len(auditEntries(t, h)); n != before {
				t.Errorf("audit entries = %d after a rejected prune, want %d", n, before)
			}
		}
	})

}

// auditEntries returns every audit entry, newest first.
func auditEntries(t *testing.T, h *alertFSM) []store.AuditEntry {
	t.Helper()
	page, err := h.st.ListAudit(h.ctx, store.AuditQuery{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	return page.Entries
}
