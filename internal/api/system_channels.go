// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package api

import (
	"context"

	"github.com/joinsider/uptimecluster/internal/api/gen"
	"github.com/joinsider/uptimecluster/internal/store"
)

// System channels (FA-OBS-01, ADR 0025): the notification channels of the
// reserved tenant store.SystemTenantID, which receive system alerts. They are
// managed only by platform admins. The tenant is always store.SystemTenantID,
// never taken from the request, and the helpers of notifications.go and
// channel_secrets.go do the rest, so masking, encryption and test sends are
// the same as for tenant channels.

// ListSystemChannels returns the system channels, secrets masked.
func (h *Handlers) ListSystemChannels(ctx context.Context, _ gen.ListSystemChannelsRequestObject) (gen.ListSystemChannelsResponseObject, error) {
	if _, err := principal(ctx, platformAccess); err != nil {
		return nil, err
	}
	out, err := h.listChannels(ctx, store.SystemTenantID)
	if err != nil {
		return nil, err
	}
	return gen.ListSystemChannels200JSONResponse(out), nil
}

// CreateSystemChannel creates a system channel. The event system is included
// server-side.
func (h *Handlers) CreateSystemChannel(ctx context.Context, req gen.CreateSystemChannelRequestObject) (gen.CreateSystemChannelResponseObject, error) {
	if _, err := principal(ctx, platformAccess); err != nil {
		return nil, err
	}
	out, err := h.createChannel(ctx, store.SystemTenantID, req.Params.IdempotencyKey, req.Body)
	if err != nil {
		return nil, err
	}
	return gen.CreateSystemChannel201JSONResponse(out), nil
}

// GetSystemChannel returns one system channel. Channels of tenants are not found.
func (h *Handlers) GetSystemChannel(ctx context.Context, req gen.GetSystemChannelRequestObject) (gen.GetSystemChannelResponseObject, error) {
	if _, err := principal(ctx, platformAccess); err != nil {
		return nil, err
	}
	out, err := h.getChannel(ctx, store.SystemTenantID, req.ChannelId)
	if err != nil {
		return nil, err
	}
	return gen.GetSystemChannel200JSONResponse(out), nil
}

// UpdateSystemChannel replaces the settings of a system channel.
func (h *Handlers) UpdateSystemChannel(ctx context.Context, req gen.UpdateSystemChannelRequestObject) (gen.UpdateSystemChannelResponseObject, error) {
	if _, err := principal(ctx, platformAccess); err != nil {
		return nil, err
	}
	out, err := h.updateChannel(ctx, store.SystemTenantID, req.ChannelId, req.Body)
	if err != nil {
		return nil, err
	}
	return gen.UpdateSystemChannel200JSONResponse(out), nil
}

// DeleteSystemChannel removes a system channel.
func (h *Handlers) DeleteSystemChannel(ctx context.Context, req gen.DeleteSystemChannelRequestObject) (gen.DeleteSystemChannelResponseObject, error) {
	if _, err := principal(ctx, platformAccess); err != nil {
		return nil, err
	}
	if err := h.deleteChannel(ctx, store.SystemTenantID, req.ChannelId); err != nil {
		return nil, err
	}
	return gen.DeleteSystemChannel204Response{}, nil
}

// TestSystemChannel sends a test system alert to a system channel synchronously.
func (h *Handlers) TestSystemChannel(ctx context.Context, req gen.TestSystemChannelRequestObject) (gen.TestSystemChannelResponseObject, error) {
	if _, err := principal(ctx, platformAccess); err != nil {
		return nil, err
	}
	out, err := h.testChannel(ctx, store.SystemTenantID, req.ChannelId)
	if err != nil {
		return nil, err
	}
	return gen.TestSystemChannel200JSONResponse(out), nil
}
