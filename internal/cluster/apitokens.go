// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
)

// API token rules (ADR 0031, section 1). The API and the auth middleware use
// these constants; the FSM enforces them when it applies a token.
const (
	// APITokenScopeTenant is the only scope of an API token; other scopes need a later ADR.
	APITokenScopeTenant = "tenant"
	// MinAPITokenLifetime and MaxAPITokenLifetime bound expires_at_ms minus issued_at_ms.
	MinAPITokenLifetime = time.Hour
	MaxAPITokenLifetime = 366 * 24 * time.Hour
	// MaxAPITokensPerTenant is the fixed limit of tokens per tenant (not a plan limit).
	MaxAPITokensPerTenant = 50
	// APITokenRetentionAfterExpiry is how long expired tokens stay listed before PruneAudit removes them.
	APITokenRetentionAfterExpiry = 30 * 24 * time.Hour
)

const (
	apiTokenIDPrefix = "tok_"
	apiTokenIDLength = 16
	apiTokenNameMax  = 64
)

// APITokenResult is the result of CreateAPIToken and RevokeAPIToken.
type APITokenResult struct {
	ID string `json:"id"`
}

// ValidAPITokenRole reports whether role may be given to an API token. A token
// never carries platform-admin (ADR 0031, section 1).
func ValidAPITokenRole(role string) bool {
	switch role {
	case "tenant-viewer", "tenant-editor", "tenant-admin":
		return true
	}
	return false
}

// ValidAPITokenName reports whether name is a valid token name: 1 to 64 runes,
// no control characters, and not only whitespace.
func ValidAPITokenName(name string) bool {
	n := utf8.RuneCountInString(name)
	if n < 1 || n > apiTokenNameMax || strings.TrimSpace(name) == "" {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// validAPITokenID reports whether id is "tok_" followed by 16 characters of
// the base32 alphabet a-z and 2-7.
func validAPITokenID(id string) bool {
	rest, ok := strings.CutPrefix(id, apiTokenIDPrefix)
	if !ok || len(rest) != apiTokenIDLength {
		return false
	}
	for i := range len(rest) {
		c := rest[i]
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}

// validSHA256Hex reports whether s is a SHA-256 digest in lower-case hex.
func validSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// checkAPITokenFields validates the fields of a CreateAPIToken against the
// command time.
func checkAPITokenFields(c *pb.CreateAPIToken, issuedAtMs int64) error {
	switch {
	case !validAPITokenID(c.GetId()):
		return invalid("API token ID must be %s followed by %d characters of a-z and 2-7", apiTokenIDPrefix, apiTokenIDLength)
	case c.GetScope() != APITokenScopeTenant:
		return invalid("API token scope must be %s", APITokenScopeTenant)
	case !ValidAPITokenRole(c.GetRole()):
		return invalid("API token role must be tenant-viewer, tenant-editor or tenant-admin")
	case !ValidAPITokenName(c.GetName()):
		return invalid("API token name must be 1 to %d characters without control characters", apiTokenNameMax)
	case !validSHA256Hex(c.GetSecretHash()):
		return invalid("API token secret hash must be a SHA-256 digest in lower-case hex")
	}
	lifetime := c.GetExpiresAtMs() - issuedAtMs
	if lifetime < MinAPITokenLifetime.Milliseconds() || lifetime > MaxAPITokenLifetime.Milliseconds() {
		return invalid("API token lifetime must be between %v and %v", MinAPITokenLifetime, MaxAPITokenLifetime)
	}
	return nil
}

// applyCreateAPIToken stores an API token of the command's tenant (ADR 0031,
// section 1). Rules, in order: command level, a signed-in user as actor, a
// tenant that exists and is not suspended, valid fields, the token limit, and
// a free name. Only the hash of the secret is stored.
func applyCreateAPIToken(ctx context.Context, tx store.StateTx, cmd *pb.Command, c *pb.CreateAPIToken) (json.RawMessage, error) {
	if err := requireCommandLevel(ctx, tx, "creating API tokens", AuditCommandLevel); err != nil {
		return nil, err
	}
	if err := requireUserActor(cmd, "creating API tokens"); err != nil {
		return nil, err
	}
	tenantID := cmd.GetTenantId()
	switch tenantID {
	case "":
		return nil, invalid("API token command without tenant")
	case store.SystemTenantID:
		return nil, invalid("the system tenant has no API tokens")
	}
	tn, err := tx.GetTenant(ctx, tenantID)
	if err != nil {
		return nil, storeError(err)
	}
	if tn.Suspended {
		return nil, invalid("tenant %s is suspended", tenantID)
	}
	if err := checkAPITokenFields(c, cmd.GetIssuedAtMs()); err != nil {
		return nil, err
	}
	n, err := tx.CountAPITokens(ctx, tenantID)
	if err != nil {
		return nil, storeError(err)
	}
	if n >= MaxAPITokensPerTenant {
		return nil, &CommandError{Code: CodeLimitExceeded, Message: fmt.Sprintf("a tenant can have at most %d API tokens", MaxAPITokensPerTenant)}
	}
	actor := cmd.GetActor()
	err = tx.InsertAPIToken(ctx, store.APIToken{
		ID:            c.GetId(),
		TenantID:      tenantID,
		Scope:         c.GetScope(),
		Name:          c.GetName(),
		Role:          c.GetRole(),
		SecretHash:    c.GetSecretHash(),
		CreatedMs:     cmd.GetIssuedAtMs(),
		ExpiresMs:     c.GetExpiresAtMs(),
		CreatedByKind: auditActorKind(actor.GetKind()),
		CreatedByID:   actor.GetId(),
		CreatedByName: actor.GetName(),
	})
	if errors.Is(err, store.ErrConflict) {
		return nil, &CommandError{Code: CodeConflict, Message: "an API token with this name already exists"}
	}
	if err != nil {
		return nil, storeError(err)
	}
	return json.Marshal(APITokenResult{ID: c.GetId()})
}

// applyRevokeAPIToken deletes an API token of the command's tenant (ADR 0031,
// section 1). The audit entry of the token stays. A token of another tenant is
// not found.
func applyRevokeAPIToken(ctx context.Context, tx store.StateTx, cmd *pb.Command, r *pb.RevokeAPIToken) (json.RawMessage, error) {
	if err := requireCommandLevel(ctx, tx, "revoking API tokens", AuditCommandLevel); err != nil {
		return nil, err
	}
	if err := requireUserActor(cmd, "revoking API tokens"); err != nil {
		return nil, err
	}
	if cmd.GetTenantId() == "" {
		return nil, invalid("API token command without tenant")
	}
	if err := tx.DeleteAPIToken(ctx, cmd.GetTenantId(), r.GetId()); err != nil {
		return nil, storeError(err)
	}
	return json.Marshal(APITokenResult{ID: r.GetId()})
}

// applyPruneAudit deletes the audit entries and the expired API tokens that are
// older than the cut-offs of the command (ADR 0031, section 2). Only the system
// may send it. Both cut-offs must be positive and not after the command time.
func applyPruneAudit(ctx context.Context, tx store.StateTx, cmd *pb.Command, p *pb.PruneAudit) (json.RawMessage, error) {
	if err := requireCommandLevel(ctx, tx, "pruning the audit log", AuditCommandLevel); err != nil {
		return nil, err
	}
	if cmd.GetActor().GetKind() != pb.Actor_KIND_SYSTEM {
		return nil, invalid("pruning the audit log is only allowed for the system")
	}
	issuedAt := cmd.GetIssuedAtMs()
	if p.GetAuditBeforeMs() <= 0 || p.GetAuditBeforeMs() > issuedAt {
		return nil, invalid("audit_before_ms must be above 0 and not after the command time")
	}
	if p.GetTokensExpiredBeforeMs() <= 0 || p.GetTokensExpiredBeforeMs() > issuedAt {
		return nil, invalid("tokens_expired_before_ms must be above 0 and not after the command time")
	}
	if err := tx.PruneAudit(ctx, p.GetAuditBeforeMs()); err != nil {
		return nil, err
	}
	if err := tx.PruneAPITokens(ctx, p.GetTokensExpiredBeforeMs()); err != nil {
		return nil, err
	}
	return json.RawMessage("{}"), nil
}

// NewCreateAPIToken builds the command that stores an API token of tenantID
// (ADR 0031). The caller passes the actor, a signed-in user, and the request
// ID of the API request, so that retries are deduplicated.
func NewCreateAPIToken(requestID, tenantID string, actor *pb.Actor, t *pb.CreateAPIToken) *pb.Command {
	return stamp(&pb.Command{
		RequestId: requestID,
		TenantId:  tenantID,
		Actor:     actor,
		Payload:   &pb.Command_CreateApiToken{CreateApiToken: t},
	})
}

// NewRevokeAPIToken builds the command that deletes the API token id of tenantID
// (ADR 0031).
func NewRevokeAPIToken(requestID, tenantID string, actor *pb.Actor, id string) *pb.Command {
	return stamp(&pb.Command{
		RequestId: requestID,
		TenantId:  tenantID,
		Actor:     actor,
		Payload:   &pb.Command_RevokeApiToken{RevokeApiToken: &pb.RevokeAPIToken{Id: id}},
	})
}

// NewPruneAudit builds the system command that removes audit entries older than
// auditBeforeMs and API tokens that expired before tokensExpiredBeforeMs (ADR
// 0031). The request ID is derived from the audit cut-off, so a retry of the
// same run is deduplicated.
func NewPruneAudit(auditBeforeMs, tokensExpiredBeforeMs int64) *pb.Command {
	return stamp(&pb.Command{
		RequestId: "prune-audit:" + strconv.FormatInt(auditBeforeMs, 10),
		Actor:     &pb.Actor{Kind: pb.Actor_KIND_SYSTEM},
		Payload: &pb.Command_PruneAudit{PruneAudit: &pb.PruneAudit{
			AuditBeforeMs:         auditBeforeMs,
			TokensExpiredBeforeMs: tokensExpiredBeforeMs,
		}},
	})
}
