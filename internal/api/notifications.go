// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/uptimecluster/internal/alert"
	"github.com/joinsider/uptimecluster/internal/api/gen"
	"github.com/joinsider/uptimecluster/internal/auth"
	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/store"
)

// Notification channels (FA-ALR-01, FA-ALR-08, ADR 0014). Plaintext secrets
// are handled only in channel_secrets.go and in the sender.

// ListNotificationChannels returns the channels of the caller's tenant.
func (h *Handlers) ListNotificationChannels(ctx context.Context, _ gen.ListNotificationChannelsRequestObject) (gen.ListNotificationChannelsResponseObject, error) {
	p, err := principal(ctx, readAccess)
	if err != nil {
		return nil, err
	}
	chs, err := h.deps.State.ListChannels(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	out := make(gen.ListNotificationChannels200JSONResponse, 0, len(chs))
	for _, ch := range chs {
		api, err := h.apiChannel(ctx, ch)
		if err != nil {
			return nil, err
		}
		out = append(out, api)
	}
	return out, nil
}

// CreateNotificationChannel validates the input, encrypts the secrets and
// creates the channel through Raft.
func (h *Handlers) CreateNotificationChannel(ctx context.Context, req gen.CreateNotificationChannelRequestObject) (gen.CreateNotificationChannelResponseObject, error) {
	p, err := principal(ctx, adminAccess)
	if err != nil {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	doc, err := h.channelDocument(id.String(), req.Body, nil)
	if err != nil {
		return nil, err
	}
	cmd, err := cluster.NewPutChannel(requestID(p.TenantID, req.Params.IdempotencyKey), p.TenantID, id.String(), doc, 0)
	if err != nil {
		return nil, err
	}
	res, err := h.apply(ctx, cmd)
	if err != nil {
		return nil, err
	}
	// A deduplicated retry returns the ID of the first attempt.
	var put cluster.PutResult
	if err := json.Unmarshal(res.Result, &put); err != nil {
		return nil, fmt.Errorf("decode put result: %w", err)
	}
	ch, err := h.deps.State.GetChannel(ctx, p.TenantID, put.ID)
	if err != nil {
		return nil, err
	}
	out, err := h.apiChannel(ctx, ch)
	if err != nil {
		return nil, err
	}
	return gen.CreateNotificationChannel201JSONResponse(out), nil
}

// GetNotificationChannel returns one channel of the caller's tenant.
func (h *Handlers) GetNotificationChannel(ctx context.Context, req gen.GetNotificationChannelRequestObject) (gen.GetNotificationChannelResponseObject, error) {
	p, err := principal(ctx, readAccess)
	if err != nil {
		return nil, err
	}
	ch, err := h.channel(ctx, p, req.ChannelId)
	if err != nil {
		return nil, err
	}
	out, err := h.apiChannel(ctx, ch)
	if err != nil {
		return nil, err
	}
	return gen.GetNotificationChannel200JSONResponse(out), nil
}

// UpdateNotificationChannel replaces the channel's settings. An omitted URL
// or a masked value keeps the stored secret (channelDocument).
func (h *Handlers) UpdateNotificationChannel(ctx context.Context, req gen.UpdateNotificationChannelRequestObject) (gen.UpdateNotificationChannelResponseObject, error) {
	p, err := principal(ctx, adminAccess)
	if err != nil {
		return nil, err
	}
	current, err := h.channel(ctx, p, req.ChannelId)
	if err != nil {
		return nil, err
	}
	doc, err := h.channelDocument(current.ID, req.Body, &current)
	if err != nil {
		return nil, err
	}
	cmd, err := cluster.NewPutChannel(uuid.NewString(), p.TenantID, current.ID, doc, current.Revision)
	if err != nil {
		return nil, err
	}
	if _, err := h.apply(ctx, cmd); err != nil {
		return nil, err
	}
	ch, err := h.deps.State.GetChannel(ctx, p.TenantID, current.ID)
	if err != nil {
		return nil, err
	}
	out, err := h.apiChannel(ctx, ch)
	if err != nil {
		return nil, err
	}
	return gen.UpdateNotificationChannel200JSONResponse(out), nil
}

// DeleteNotificationChannel removes the channel. Its monitor links and pending
// deliveries are removed with it by the schema.
func (h *Handlers) DeleteNotificationChannel(ctx context.Context, req gen.DeleteNotificationChannelRequestObject) (gen.DeleteNotificationChannelResponseObject, error) {
	p, err := principal(ctx, adminAccess)
	if err != nil {
		return nil, err
	}
	if _, err := h.channel(ctx, p, req.ChannelId); err != nil {
		return nil, err
	}
	if _, err := h.apply(ctx, cluster.NewDeleteChannel(uuid.NewString(), p.TenantID, req.ChannelId.String())); err != nil {
		return nil, err
	}
	return gen.DeleteNotificationChannel204Response{}, nil
}

// TestNotificationChannel sends a test message synchronously (FA-ALR-08). It
// bypasses the outbox and does not change the channel state.
func (h *Handlers) TestNotificationChannel(ctx context.Context, req gen.TestNotificationChannelRequestObject) (gen.TestNotificationChannelResponseObject, error) {
	p, err := principal(ctx, adminAccess)
	if err != nil {
		return nil, err
	}
	ch, err := h.channel(ctx, p, req.ChannelId)
	if err != nil {
		return nil, err
	}
	if h.deps.Keyring == nil || h.deps.Alerts == nil {
		return nil, errors.New("notification channels are not configured")
	}
	testID, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	target, err := alert.DecryptTarget(ctx, h.deps.Keyring, ch)
	if err != nil {
		h.log.Warn("cannot decrypt notification channel", "tenant", ch.TenantID, "channel", ch.ID, "err", err)
		msg := "cannot decrypt channel secrets"
		return gen.TestNotificationChannel200JSONResponse(gen.DeliveryResult{Delivered: false, Error: &msg}), nil
	}
	now := h.deps.Now()
	payload := cluster.AlertPayload{
		AlertID:        "test:" + testID.String(),
		TenantID:       p.TenantID,
		MonitorName:    "UptimeCluster test",
		Event:          store.EventUp,
		Status:         string(store.StatusUp),
		PreviousStatus: string(store.StatusDown),
		SinceMs:        now.UnixMilli(),
		Cause:          "This is a test notification",
		Confirmed:      true,
	}
	start := time.Now()
	sendErr := h.deps.Alerts.Send(ctx, target, payload)
	duration := int(time.Since(start).Milliseconds())
	out := gen.DeliveryResult{Delivered: sendErr == nil, DurationMs: &duration}
	if sendErr != nil {
		// Only redacted delivery messages are returned; anything else is generic.
		msg := "delivery failed"
		var de *alert.DeliveryError
		if errors.As(sendErr, &de) {
			msg = de.Msg
		}
		out.Error = &msg
	}
	return gen.TestNotificationChannel200JSONResponse(out), nil
}

// channel loads a channel of the caller's tenant; other tenants' channels are
// reported as not found (FA-TEN-02).
func (h *Handlers) channel(ctx context.Context, p auth.Principal, id uuid.UUID) (store.NotificationChannel, error) {
	ch, err := h.deps.State.GetChannel(ctx, p.TenantID, id.String())
	if errors.Is(err, store.ErrNotFound) {
		return store.NotificationChannel{}, notFound("notification channel")
	}
	return ch, err
}
