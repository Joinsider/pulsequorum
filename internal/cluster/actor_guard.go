// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package cluster

import (
	"context"

	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
)

// The API rejects API tokens on everything but tenant configuration
// (ADR 0031). The FSM repeats the rule, so a missed check in a handler cannot
// let a token manage users, tokens, tenants or cluster objects.

// tokenWritableKinds are the entity kinds an API token may create, change or
// delete: the tenant's own configuration.
var tokenWritableKinds = map[pb.EntityKind]bool{
	pb.EntityKind_ENTITY_KIND_MONITOR:              true,
	pb.EntityKind_ENTITY_KIND_NOTIFICATION_CHANNEL: true,
	pb.EntityKind_ENTITY_KIND_MAINTENANCE_WINDOW:   true,
}

// checkEntityActor rejects writes of an API token to entity kinds outside
// tokenWritableKinds. Below AuditCommandLevel the rule is off, as binaries of
// level 8 do not have it (ADR 0018); the API accepts tokens only from level 9.
func checkEntityActor(ctx context.Context, tx store.StateTx, cmd *pb.Command, kind pb.EntityKind) error {
	if cmd.GetActor().GetKind() != pb.Actor_KIND_API_TOKEN || tokenWritableKinds[kind] {
		return nil
	}
	level, err := currentCommandLevel(ctx, tx)
	if err != nil {
		return err
	}
	if level < AuditCommandLevel {
		return nil
	}
	return invalid("API tokens cannot change objects of kind %s", kind)
}

// requireUserActor admits only interactively signed-in users. API token
// management uses it: a token must not create or revoke tokens, and the
// system has no reason to.
func requireUserActor(cmd *pb.Command, what string) error {
	if cmd.GetActor().GetKind() != pb.Actor_KIND_USER || cmd.GetActor().GetId() == "" {
		return invalid("%s is only allowed for signed-in users", what)
	}
	return nil
}
