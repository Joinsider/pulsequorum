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
	"github.com/joinsider/uptimecluster/internal/tenant"
)

// OIDCCommandLevel is the command level of BindTenantOrg (ADR 0027). Below that
// level the command is rejected and nothing is written, so that a cluster with
// voters of different versions does not diverge (ADR 0018).
const OIDCCommandLevel = 7

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

// applyBindTenantOrg sets tenants.zitadel_org_id (ADR 0027). Rules, in order:
//   - the tenant ID is set and is not the system tenant;
//   - the organization ID is valid (see tenant.ValidOrgID);
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
	case !tenant.ValidOrgID(orgID):
		return nil, invalid("%s", tenant.OrgIDRule)
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
