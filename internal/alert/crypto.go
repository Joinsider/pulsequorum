// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package alert

import (
	"context"
	"fmt"

	"github.com/joinsider/uptimecluster/internal/secrets"
	"github.com/joinsider/uptimecluster/internal/store"
)

// Keyring encrypts and decrypts channel secrets (*secrets.Keyring, ADR 0011).
type Keyring interface {
	Encrypt(plaintext []byte, aad string) (string, error)
	Decrypt(ctx context.Context, ciphertext, aad string) ([]byte, error)
}

var _ Keyring = (*secrets.Keyring)(nil)

// The AAD binds each ciphertext to its channel and field, so it cannot be
// copied to another record (ADR 0011).
func urlAAD(channelID string) string { return "notification_channels/" + channelID + "/url" }

func headerAAD(channelID, name string) string {
	return "notification_channels/" + channelID + "/header/" + name
}

// EncryptURL encrypts a channel URL for the channel ID.
func EncryptURL(kr Keyring, channelID, plainURL string) (string, error) {
	enc, err := kr.Encrypt([]byte(plainURL), urlAAD(channelID))
	if err != nil {
		return "", fmt.Errorf("encrypt channel URL: %w", err)
	}
	return enc, nil
}

// EncryptHeaders encrypts webhook header values. Values equal to keep are
// taken from previous (already encrypted), so an update can leave masked
// headers unchanged.
func EncryptHeaders(kr Keyring, channelID string, plain, previous map[string]string, keep string) (map[string]string, error) {
	out := make(map[string]string, len(plain))
	for name, v := range plain {
		if v == keep {
			old, ok := previous[name]
			if !ok {
				return nil, fmt.Errorf("header %q: no stored value to keep", name)
			}
			out[name] = old
			continue
		}
		enc, err := kr.Encrypt([]byte(v), headerAAD(channelID, name))
		if err != nil {
			return nil, fmt.Errorf("encrypt header %q: %w", name, err)
		}
		out[name] = enc
	}
	return out, nil
}

// DecryptTarget turns a stored channel into a Target with plaintext secrets.
// The result must only be used for one delivery and never be logged.
func DecryptTarget(ctx context.Context, kr Keyring, ch store.NotificationChannel) (Target, error) {
	u, err := kr.Decrypt(ctx, ch.URLEnc, urlAAD(ch.ID))
	if err != nil {
		return Target{}, fmt.Errorf("decrypt URL of channel %s: %w", ch.ID, err)
	}
	t := Target{Kind: ch.Kind, URL: string(u), Method: ch.WebhookMethod, Template: ch.WebhookTemplate}
	if len(ch.WebhookHeaders) > 0 {
		t.Headers = make(map[string]string, len(ch.WebhookHeaders))
		for name, enc := range ch.WebhookHeaders {
			v, err := kr.Decrypt(ctx, enc, headerAAD(ch.ID, name))
			if err != nil {
				return Target{}, fmt.Errorf("decrypt header %q of channel %s: %w", name, ch.ID, err)
			}
			t.Headers[name] = string(v)
		}
	}
	return t, nil
}
