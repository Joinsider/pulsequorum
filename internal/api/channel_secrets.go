// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/pulsequorum/internal/alert"
	"github.com/joinsider/pulsequorum/internal/api/gen"
	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/store"
)

// This file holds the secret handling of the channel API (ADR 0011, 0014):
// plaintext URLs and header values exist only inside these functions and are
// never returned, logged or stored unencrypted.

func invalidChannel(detail string) *Problem {
	return &Problem{Status: http.StatusUnprocessableEntity, Title: "Validation Failed", Detail: detail}
}

// channelEvents returns the events a channel of tenantID subscribes to (ADR
// 0023, 0025). Tenant channels never subscribe to system: the event is reserved
// for the system tenant and answers 400. System channels always subscribe to
// it; without requested events they get only that one.
func (h *Handlers) channelEvents(ctx context.Context, tenantID string, requested *[]gen.NotificationEvent) ([]string, error) {
	system := tenantID == store.SystemTenantID
	if requested == nil {
		if system {
			return []string{store.EventSystem}, nil
		}
		events := []string{store.EventDown, store.EventUp}
		// New channels also get flapping and channel_failed once the cluster supports them (ADR 0023).
		level, err := h.deps.State.CommandLevel(ctx)
		if err != nil {
			return nil, fmt.Errorf("read command level: %w", err)
		}
		eff := cluster.EffectiveCommandLevel(level)
		if eff >= cluster.FlappingCommandLevel {
			events = append(events, store.EventFlapping, store.EventChannelFailed)
		}
		if eff >= cluster.ProbeCommandLevel {
			events = append(events, store.EventProbeOffline)
		}
		return events, nil
	}
	events := make([]string, 0, len(*requested)+1)
	for _, e := range *requested {
		switch {
		case !e.Valid():
			return nil, invalidChannel("unknown event " + string(e))
		case e == gen.System && !system:
			return nil, &Problem{Status: http.StatusBadRequest, Title: "Bad Request",
				Detail: "the event system is reserved for system channels"}
		case e == gen.ProbeOffline && system:
			return nil, &Problem{Status: http.StatusBadRequest, Title: "Bad Request",
				Detail: "the event probe_offline is not valid for system channels"}
		}
		events = append(events, string(e))
	}
	if system && !slices.Contains(events, store.EventSystem) {
		events = append(events, store.EventSystem)
	}
	return events, nil
}

// channelDocument validates the input and encrypts its secrets for the channel
// id of tenantID. tenantID is the caller's tenant or store.SystemTenantID; it
// decides the event policy (channelEvents). current is nil on create; on update
// an omitted URL or a value containing the mask keeps the stored secret.
func (h *Handlers) channelDocument(ctx context.Context, tenantID, id string, in *gen.NotificationChannelInput, current *store.NotificationChannel) (cluster.ChannelDocument, error) {
	doc, err := h.initChannelDocument(ctx, tenantID, in)
	if err != nil {
		return cluster.ChannelDocument{}, err
	}
	sameKind := current != nil && current.Kind == doc.Kind

	var target alert.Target
	var keepURL bool
	switch in.Kind {
	case gen.Shoutrrr:
		target, keepURL, err = shoutrrrTarget(in, sameKind)
	case gen.Webhook:
		target, keepURL, err = h.webhookTarget(id, in, current, sameKind, &doc)
	default:
		return cluster.ChannelDocument{}, invalidChannel("kind must be shoutrrr or webhook")
	}
	if err != nil {
		return cluster.ChannelDocument{}, err
	}
	return h.finalizeChannelDocument(in.Kind, id, target, keepURL, current, doc)
}

func (h *Handlers) initChannelDocument(ctx context.Context, tenantID string, in *gen.NotificationChannelInput) (cluster.ChannelDocument, error) {
	if in == nil {
		return cluster.ChannelDocument{}, &Problem{Status: http.StatusBadRequest, Title: problemTitleBadRequest, Detail: "request body is required"}
	}
	if h.deps.Keyring == nil || h.deps.Alerts == nil {
		return cluster.ChannelDocument{}, errors.New("notification channels are not configured")
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len([]rune(name)) > 200 {
		return cluster.ChannelDocument{}, invalidChannel("name must have 1 to 200 characters")
	}
	events, err := h.channelEvents(ctx, tenantID, in.Events)
	if err != nil {
		return cluster.ChannelDocument{}, err
	}
	doc := cluster.ChannelDocument{Name: name, Kind: string(in.Kind), Events: events}
	// System channels have no monitors to attach to, so they are never a default.
	if in.IsDefault != nil && tenantID != store.SystemTenantID {
		doc.IsDefault = *in.IsDefault
	}
	return doc, nil
}

func shoutrrrTarget(in *gen.NotificationChannelInput, sameKind bool) (alert.Target, bool, error) {
	if in.Webhook != nil {
		return alert.Target{}, false, invalidChannel("webhook settings are only allowed for kind webhook")
	}
	switch {
	case in.Url != nil && !strings.Contains(*in.Url, alert.Mask):
		return alert.Target{Kind: store.ChannelShoutrrr, URL: strings.TrimSpace(*in.Url)}, false, nil
	case sameKind:
		return alert.Target{}, true, nil
	default:
		return alert.Target{}, false, invalidChannel("url is required")
	}
}

func (h *Handlers) webhookTarget(id string, in *gen.NotificationChannelInput, current *store.NotificationChannel, sameKind bool, doc *cluster.ChannelDocument) (alert.Target, bool, error) {
	if in.Webhook == nil {
		return alert.Target{}, false, invalidChannel("webhook settings are required for kind webhook")
	}
	if in.Url != nil {
		return alert.Target{}, false, invalidChannel("url is only allowed for kind shoutrrr; use webhook.url")
	}
	w := in.Webhook
	target := alert.Target{Kind: store.ChannelWebhook, URL: strings.TrimSpace(w.Url)}
	if w.Method != nil {
		target.Method = string(*w.Method)
	}
	if w.BodyTemplate != nil {
		target.Template = *w.BodyTemplate
	}
	keepURL := false
	if target.URL == "" || strings.Contains(target.URL, alert.Mask) {
		if !sameKind {
			return alert.Target{}, false, invalidChannel("webhook.url is required")
		}
		keepURL = true
	}
	doc.WebhookMethod, doc.WebhookTemplate = target.Method, target.Template
	if w.Headers != nil {
		enc, err := h.encryptWebhookHeaders(id, *w.Headers, current, sameKind)
		if err != nil {
			return alert.Target{}, false, err
		}
		doc.WebhookHeaders = enc
	}
	return target, keepURL, nil
}

func (h *Handlers) encryptWebhookHeaders(id string, headers map[string]string, current *store.NotificationChannel, sameKind bool) (map[string]string, error) {
	var previous map[string]string
	if sameKind {
		previous = current.WebhookHeaders
	}
	for name := range headers {
		if http.CanonicalHeaderKey(name) == "Content-Type" || strings.TrimSpace(name) == "" {
			return nil, invalidChannel("header " + name + " cannot be set")
		}
	}
	enc, err := alert.EncryptHeaders(h.deps.Keyring, id, headers, previous, alert.Mask)
	if err != nil {
		return nil, invalidChannel("headers: a masked value can only keep an existing header")
	}
	return enc, nil
}

func (h *Handlers) finalizeChannelDocument(kind gen.NotificationKind, id string, target alert.Target, keepURL bool, current *store.NotificationChannel, doc cluster.ChannelDocument) (cluster.ChannelDocument, error) {
	if keepURL {
		doc.URLEnc = current.URLEnc
		// Validate settings that are not secret against the stored URL.
		if kind == gen.Webhook {
			if err := alert.ValidateTemplate(target.Template); err != nil {
				return cluster.ChannelDocument{}, invalidChannel(err.Error())
			}
		}
		return doc, nil
	}
	if err := h.deps.Alerts.Validate(target); err != nil {
		return cluster.ChannelDocument{}, invalidChannel(err.Error()) // DeliveryError messages are redacted
	}
	enc, err := alert.EncryptURL(h.deps.Keyring, id, target.URL)
	if err != nil {
		return cluster.ChannelDocument{}, err
	}
	doc.URLEnc = enc
	return doc, nil
}

// apiChannel converts a stored channel; URLs and header values are masked.
func (h *Handlers) apiChannel(ctx context.Context, ch store.NotificationChannel) (gen.NotificationChannel, error) {
	id, _ := uuid.Parse(ch.ID)
	out := gen.NotificationChannel{
		Id:        id,
		Name:      ch.Name,
		Kind:      gen.NotificationKind(ch.Kind),
		IsDefault: ch.IsDefault,
		State:     gen.NotificationChannelState(ch.State),
		Events:    make([]gen.NotificationEvent, 0, len(ch.Events)),
		CreatedAt: time.UnixMilli(ch.CreatedMs).UTC(),
	}
	for _, e := range ch.Events {
		out.Events = append(out.Events, gen.NotificationEvent(e))
	}
	if ch.LastError != "" {
		out.LastError = &ch.LastError
	}
	masked := alert.Mask
	if h.deps.Keyring != nil {
		if t, err := alert.DecryptTarget(ctx, h.deps.Keyring, ch); err == nil {
			masked = alert.MaskURL(t.URL)
		} else {
			h.log.Warn("cannot decrypt notification channel", "tenant", ch.TenantID, "channel", ch.ID, "err", err)
		}
	}
	if ch.Kind == store.ChannelWebhook {
		w := gen.WebhookConfig{Url: masked}
		if ch.WebhookMethod != "" {
			m := gen.WebhookConfigMethod(ch.WebhookMethod)
			w.Method = &m
		}
		if ch.WebhookTemplate != "" {
			w.BodyTemplate = &ch.WebhookTemplate
		}
		if len(ch.WebhookHeaders) > 0 {
			headers := make(map[string]string, len(ch.WebhookHeaders))
			for name := range ch.WebhookHeaders {
				headers[name] = alert.Mask
			}
			w.Headers = &headers
		}
		out.Webhook = &w
	} else {
		out.MaskedUrl = &masked
	}
	return out, nil
}
