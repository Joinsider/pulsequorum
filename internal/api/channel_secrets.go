// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/uptimecluster/internal/alert"
	"github.com/joinsider/uptimecluster/internal/api/gen"
	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/store"
)

// This file holds the secret handling of the channel API (ADR 0011, 0014):
// plaintext URLs and header values exist only inside these functions and are
// never returned, logged or stored unencrypted.

func invalidChannel(detail string) *Problem {
	return &Problem{Status: http.StatusUnprocessableEntity, Title: "Validation Failed", Detail: detail}
}

// channelDocument validates the input and encrypts its secrets for the
// channel id. current is nil on create; on update an omitted URL or a value
// containing the mask keeps the stored secret.
func (h *Handlers) channelDocument(ctx context.Context, id string, in *gen.NotificationChannelInput, current *store.NotificationChannel) (cluster.ChannelDocument, error) {
	if in == nil {
		return cluster.ChannelDocument{}, &Problem{Status: http.StatusBadRequest, Title: "Bad Request", Detail: "request body is required"}
	}
	if h.deps.Keyring == nil || h.deps.Alerts == nil {
		return cluster.ChannelDocument{}, errors.New("notification channels are not configured")
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len([]rune(name)) > 200 {
		return cluster.ChannelDocument{}, invalidChannel("name must have 1 to 200 characters")
	}
	doc := cluster.ChannelDocument{Name: name, Kind: string(in.Kind), Events: []string{store.EventDown, store.EventUp}}
	if in.Events == nil {
		// New channels also get flapping and channel_failed once the cluster supports them (ADR 0023).
		level, err := h.deps.State.CommandLevel(ctx)
		if err != nil {
			return cluster.ChannelDocument{}, fmt.Errorf("read command level: %w", err)
		}
		if cluster.EffectiveCommandLevel(level) >= cluster.FlappingCommandLevel {
			doc.Events = append(doc.Events, store.EventFlapping, store.EventChannelFailed)
		}
	}
	if in.Events != nil {
		doc.Events = make([]string, 0, len(*in.Events))
		for _, e := range *in.Events {
			if !e.Valid() {
				return cluster.ChannelDocument{}, invalidChannel("unknown event " + string(e))
			}
			doc.Events = append(doc.Events, string(e))
		}
	}
	if in.IsDefault != nil {
		doc.IsDefault = *in.IsDefault
	}
	sameKind := current != nil && current.Kind == doc.Kind

	var target alert.Target
	keepURL := false
	switch in.Kind {
	case gen.Shoutrrr:
		if in.Webhook != nil {
			return cluster.ChannelDocument{}, invalidChannel("webhook settings are only allowed for kind webhook")
		}
		switch {
		case in.Url != nil && !strings.Contains(*in.Url, alert.Mask):
			target = alert.Target{Kind: store.ChannelShoutrrr, URL: strings.TrimSpace(*in.Url)}
		case sameKind:
			keepURL = true
		default:
			return cluster.ChannelDocument{}, invalidChannel("url is required")
		}
	case gen.Webhook:
		if in.Webhook == nil {
			return cluster.ChannelDocument{}, invalidChannel("webhook settings are required for kind webhook")
		}
		if in.Url != nil {
			return cluster.ChannelDocument{}, invalidChannel("url is only allowed for kind shoutrrr; use webhook.url")
		}
		w := in.Webhook
		target = alert.Target{Kind: store.ChannelWebhook, URL: strings.TrimSpace(w.Url)}
		if w.Method != nil {
			target.Method = string(*w.Method)
		}
		if w.BodyTemplate != nil {
			target.Template = *w.BodyTemplate
		}
		if target.URL == "" || strings.Contains(target.URL, alert.Mask) {
			if !sameKind {
				return cluster.ChannelDocument{}, invalidChannel("webhook.url is required")
			}
			keepURL = true
		}
		doc.WebhookMethod, doc.WebhookTemplate = target.Method, target.Template
		if w.Headers != nil {
			var previous map[string]string
			if sameKind {
				previous = current.WebhookHeaders
			}
			for name := range *w.Headers {
				if http.CanonicalHeaderKey(name) == "Content-Type" || strings.TrimSpace(name) == "" {
					return cluster.ChannelDocument{}, invalidChannel("header " + name + " cannot be set")
				}
			}
			enc, err := alert.EncryptHeaders(h.deps.Keyring, id, *w.Headers, previous, alert.Mask)
			if err != nil {
				return cluster.ChannelDocument{}, invalidChannel("headers: a masked value can only keep an existing header")
			}
			doc.WebhookHeaders = enc
		}
	default:
		return cluster.ChannelDocument{}, invalidChannel("kind must be shoutrrr or webhook")
	}

	if keepURL {
		doc.URLEnc = current.URLEnc
		// Validate settings that are not secret against the stored URL.
		if in.Kind == gen.Webhook {
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
