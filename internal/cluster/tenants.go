// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
)

// OIDCCommandLevel is the command level of BindTenantOrg (ADR 0027). Below that
// level the command is rejected and nothing is written, so that a cluster with
// voters of different versions does not diverge (ADR 0018).
const OIDCCommandLevel = 7

// maxOrgIDLength is the longest accepted identity-provider organization ID, in bytes.
const maxOrgIDLength = 128

// BindTenantOrgResult is the result of BindTenantOrg (ADR 0027).
type BindTenantOrgResult struct {
	TenantID string `json:"tenant_id"`
	OrgID    string `json:"org_id"`
}

// NewBindTenantOrg builds the command that binds a tenant to an identity-provider
// organization (ADR 0027). The request ID is derived from the binding, so a retry
// of the same binding is deduplicated by the FSM.
func NewBindTenantOrg(tenantID, orgID string) *pb.Command {
	return stamp(&pb.Command{
		RequestId: "bind-tenant-org:" + tenantID + ":" + orgID,
		Actor:     &pb.Actor{Kind: pb.Actor_KIND_SYSTEM},
		Payload: &pb.Command_BindTenantOrg{BindTenantOrg: &pb.BindTenantOrg{
			TenantId: tenantID, OrgId: orgID,
		}},
	})
}

// validOrgID reports whether id is a usable organization ID: 1 to 128 bytes of
// ASCII letters, digits, '.', '_', '@' and '-'.
func validOrgID(id string) bool {
	if id == "" || len(id) > maxOrgIDLength {
		return false
	}
	for i := range len(id) {
		c := id[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case c == '.', c == '_', c == '@', c == '-':
		default:
			return false
		}
	}
	return true
}

// applyBindTenantOrg sets tenants.zitadel_org_id (ADR 0027). Rules, in order:
//   - the tenant ID is set and is not the system tenant;
//   - the organization ID is valid (see validOrgID);
//   - the tenant exists;
//   - no other tenant holds the organization;
//   - a tenant already bound to the same organization is left unchanged (no write);
//   - a tenant bound to another organization is rejected: rebinding is done
//     through the tenant admin API.
//
// Only the organization and the update time of the tenant change.
func applyBindTenantOrg(ctx context.Context, tx store.StateTx, cmd *pb.Command, b *pb.BindTenantOrg) (json.RawMessage, error) {
	if err := requireCommandLevel(ctx, tx, "binding tenants to organizations", OIDCCommandLevel); err != nil {
		return nil, err
	}
	tenantID, orgID := b.GetTenantId(), b.GetOrgId()
	switch {
	case tenantID == "":
		return nil, invalid("bind tenant org without tenant")
	case tenantID == store.SystemTenantID:
		return nil, invalid("the system tenant cannot be bound to an organization")
	case !validOrgID(orgID):
		return nil, invalid("organization ID must be 1 to %d characters of A-Z, a-z, 0-9, '.', '_', '@' or '-'", maxOrgIDLength)
	}
	result := BindTenantOrgResult{TenantID: tenantID, OrgID: orgID}

	tn, err := tx.GetTenant(ctx, tenantID)
	if err != nil {
		return nil, storeError(err)
	}

	holder, err := tx.GetTenantByOrg(ctx, orgID)
	switch {
	case err == nil && holder.ID != tenantID:
		return nil, &CommandError{Code: CodeConflict, Message: fmt.Sprintf("organization %s is already bound to another tenant", orgID)}
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return nil, storeError(err)
	}

	switch {
	case tn.ZitadelOrgID == orgID:
		return json.Marshal(result)
	case tn.ZitadelOrgID != "":
		return nil, &CommandError{Code: CodeConflict, Message: "tenant is already bound; rebinding is done through the tenant admin API"}
	}

	tn.ZitadelOrgID = orgID
	tn.UpdatedMs = cmd.GetIssuedAtMs()
	if err := tx.PutTenant(ctx, tn); err != nil {
		return nil, storeError(err)
	}
	return json.Marshal(result)
}
