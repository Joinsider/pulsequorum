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
	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/store"
)

// Notification channels (FA-ALR-01, FA-ALR-08, ADR 0014). Plaintext secrets
// are handled only in channel_secrets.go and in the sender.
//
// The tenant endpoints here and the system channel endpoints (ADR 0025) share
// the helpers below. Each helper takes the tenant explicitly: the tenant
// endpoints pass the caller's tenant, the system endpoints pass
// store.SystemTenantID. Nothing else decides the tenant.

// ListNotificationChannels returns the channels of the caller's tenant.
func (h *Handlers) ListNotificationChannels(ctx context.Context, _ gen.ListNotificationChannelsRequestObject) (gen.ListNotificationChannelsResponseObject, error) {
	p, err := principal(ctx, readAccess)
	if err != nil {
		return nil, err
	}
	out, err := h.listChannels(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	return gen.ListNotificationChannels200JSONResponse(out), nil
}

// CreateNotificationChannel validates the input, encrypts the secrets and
// creates the channel through Raft.
func (h *Handlers) CreateNotificationChannel(ctx context.Context, req gen.CreateNotificationChannelRequestObject) (gen.CreateNotificationChannelResponseObject, error) {
	p, err := principal(ctx, adminAccess)
	if err != nil {
		return nil, err
	}
	out, err := h.createChannel(ctx, p.TenantID, req.Params.IdempotencyKey, req.Body)
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
	out, err := h.getChannel(ctx, p.TenantID, req.ChannelId)
	if err != nil {
		return nil, err
	}
	return gen.GetNotificationChannel200JSONResponse(out), nil
}

// UpdateNotificationChannel replaces the channel's settings. An omitted URL or a
// masked value keeps the stored secret (channelDocument).
func (h *Handlers) UpdateNotificationChannel(ctx context.Context, req gen.UpdateNotificationChannelRequestObject) (gen.UpdateNotificationChannelResponseObject, error) {
	p, err := principal(ctx, adminAccess)
	if err != nil {
		return nil, err
	}
	out, err := h.updateChannel(ctx, p.TenantID, req.ChannelId, req.Body)
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
	if err := h.deleteChannel(ctx, p.TenantID, req.ChannelId); err != nil {
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
	out, err := h.testChannel(ctx, p.TenantID, req.ChannelId)
	if err != nil {
		return nil, err
	}
	return gen.TestNotificationChannel200JSONResponse(out), nil
}

// listChannels returns the channels of tenantID, secrets masked.
func (h *Handlers) listChannels(ctx context.Context, tenantID string) ([]gen.NotificationChannel, error) {
	chs, err := h.deps.State.ListChannels(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]gen.NotificationChannel, 0, len(chs))
	for _, ch := range chs {
		api, err := h.apiChannel(ctx, ch)
		if err != nil {
			return nil, err
		}
		out = append(out, api)
	}
	return out, nil
}

// createChannel validates the input, encrypts the secrets and creates a channel
// of tenantID through Raft. A retry with the same idempotency key returns the
// channel of the first attempt; the key is scoped to the tenant (requestID).
func (h *Handlers) createChannel(ctx context.Context, tenantID string, key *uuid.UUID, in *gen.NotificationChannelInput) (gen.NotificationChannel, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return gen.NotificationChannel{}, err
	}
	doc, err := h.channelDocument(ctx, tenantID, id.String(), in, nil)
	if err != nil {
		return gen.NotificationChannel{}, err
	}
	cmd, err := cluster.NewPutChannel(requestID(tenantID, key), tenantID, id.String(), doc, 0)
	if err != nil {
		return gen.NotificationChannel{}, err
	}
	res, err := h.apply(ctx, cmd)
	if err != nil {
		return gen.NotificationChannel{}, err
	}
	// A deduplicated retry returns the ID of the first attempt.
	var put cluster.PutResult
	if err := json.Unmarshal(res.Result, &put); err != nil {
		return gen.NotificationChannel{}, fmt.Errorf("decode put result: %w", err)
	}
	ch, err := h.deps.State.GetChannel(ctx, tenantID, put.ID)
	if err != nil {
		return gen.NotificationChannel{}, err
	}
	return h.apiChannel(ctx, ch)
}

// getChannel returns one channel of tenantID, secrets masked.
func (h *Handlers) getChannel(ctx context.Context, tenantID string, id uuid.UUID) (gen.NotificationChannel, error) {
	ch, err := h.channel(ctx, tenantID, id)
	if err != nil {
		return gen.NotificationChannel{}, err
	}
	return h.apiChannel(ctx, ch)
}

// updateChannel replaces the settings of a channel of tenantID. An omitted URL
// or a masked value keeps the stored secret (channelDocument).
func (h *Handlers) updateChannel(ctx context.Context, tenantID string, id uuid.UUID, in *gen.NotificationChannelInput) (gen.NotificationChannel, error) {
	current, err := h.channel(ctx, tenantID, id)
	if err != nil {
		return gen.NotificationChannel{}, err
	}
	doc, err := h.channelDocument(ctx, tenantID, current.ID, in, &current)
	if err != nil {
		return gen.NotificationChannel{}, err
	}
	cmd, err := cluster.NewPutChannel(uuid.NewString(), tenantID, current.ID, doc, current.Revision)
	if err != nil {
		return gen.NotificationChannel{}, err
	}
	if _, err := h.apply(ctx, cmd); err != nil {
		return gen.NotificationChannel{}, err
	}
	ch, err := h.deps.State.GetChannel(ctx, tenantID, current.ID)
	if err != nil {
		return gen.NotificationChannel{}, err
	}
	return h.apiChannel(ctx, ch)
}

// deleteChannel removes a channel of tenantID.
func (h *Handlers) deleteChannel(ctx context.Context, tenantID string, id uuid.UUID) error {
	if _, err := h.channel(ctx, tenantID, id); err != nil {
		return err
	}
	_, err := h.apply(ctx, cluster.NewDeleteChannel(uuid.NewString(), tenantID, id.String()))
	return err
}

// testChannel sends a test message to a channel of tenantID synchronously. It
// bypasses the outbox and does not change the channel state.
func (h *Handlers) testChannel(ctx context.Context, tenantID string, id uuid.UUID) (gen.DeliveryResult, error) {
	ch, err := h.channel(ctx, tenantID, id)
	if err != nil {
		return gen.DeliveryResult{}, err
	}
	if h.deps.Keyring == nil || h.deps.Alerts == nil {
		return gen.DeliveryResult{}, errors.New("notification channels are not configured")
	}
	testID, err := uuid.NewV7()
	if err != nil {
		return gen.DeliveryResult{}, err
	}
	target, err := alert.DecryptTarget(ctx, h.deps.Keyring, ch)
	if err != nil {
		h.log.Warn("cannot decrypt notification channel", "tenant", ch.TenantID, "channel", ch.ID, "err", err)
		msg := "cannot decrypt channel secrets"
		return gen.DeliveryResult{Delivered: false, Error: &msg}, nil
	}
	payload := testPayload(ch.TenantID, testID.String(), h.deps.Now())
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
	return out, nil
}

// testPayload is the content of a test message. A test of a system channel
// has the shape of a real system alert (ADR 0025); other channels get the
// shape of a monitor recovery.
func testPayload(tenantID, testID string, now time.Time) cluster.AlertPayload {
	if tenantID == store.SystemTenantID {
		return cluster.AlertPayload{
			AlertID:   "test:" + testID,
			TenantID:  tenantID,
			Event:     store.EventSystem,
			SinceMs:   now.UnixMilli(),
			Cause:     "This is a test notification",
			Confirmed: true,
			System: &cluster.SystemAlertInfo{
				Kind: "test", Subject: "test", Active: true, Level: 0,
				Details: json.RawMessage("{}"),
			},
		}
	}
	return cluster.AlertPayload{
		AlertID:        "test:" + testID,
		TenantID:       tenantID,
		MonitorName:    "UptimeCluster test",
		Event:          store.EventUp,
		Status:         string(store.StatusUp),
		PreviousStatus: string(store.StatusDown),
		SinceMs:        now.UnixMilli(),
		Cause:          "This is a test notification",
		Confirmed:      true,
	}
}

// channel loads a channel of tenantID. Channels of other tenants are reported
// as not found (FA-TEN-02); the caller passes its own tenant or the system tenant.
func (h *Handlers) channel(ctx context.Context, tenantID string, id uuid.UUID) (store.NotificationChannel, error) {
	ch, err := h.deps.State.GetChannel(ctx, tenantID, id.String())
	if errors.Is(err, store.ErrNotFound) {
		return store.NotificationChannel{}, notFound("notification channel")
	}
	return ch, err
}
