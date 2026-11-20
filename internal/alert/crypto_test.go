// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package alert

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/joinsider/pulsequorum/internal/secrets"
	"github.com/joinsider/pulsequorum/internal/store"
)

func testKeyring(t *testing.T) *secrets.Keyring {
	t.Helper()
	kek, err := secrets.LoadKEK("", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))), "")
	if err != nil {
		t.Fatal(err)
	}
	dek, err := secrets.NewDEK()
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := kek.Wrap("key-1", dek)
	if err != nil {
		t.Fatal(err)
	}
	kr := secrets.NewKeyring(kek, func(context.Context) ([]store.SecretKey, error) {
		return []store.SecretKey{{KeyID: "key-1", WrappedDEK: wrapped, KEKFingerprint: kek.Fingerprint}}, nil
	})
	if err := kr.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	return kr
}

// TestChannelSecrets covers ADR 0011/0014: secrets round-trip, are bound to
// their channel and field, and masked headers keep their stored value.
func TestChannelSecrets(t *testing.T) {
	ctx := context.Background()
	kr := testKeyring(t)
	urlEnc, err := EncryptURL(kr, "ch-1", "https://hooks.example.com/secret")
	if err != nil {
		t.Fatal(err)
	}
	headers, err := EncryptHeaders(kr, "ch-1", map[string]string{"Authorization": "Bearer abc"}, nil, Mask)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(urlEnc, "secret") || !secrets.IsEncrypted(urlEnc) || !secrets.IsEncrypted(headers["Authorization"]) {
		t.Fatalf("not encrypted: %s %v", urlEnc, headers)
	}
	ch := store.NotificationChannel{ID: "ch-1", Kind: store.ChannelWebhook, URLEnc: urlEnc, WebhookHeaders: headers, WebhookMethod: http.MethodPut}
	target, err := DecryptTarget(ctx, kr, ch)
	if err != nil || target.URL != "https://hooks.example.com/secret" || target.Headers["Authorization"] != "Bearer abc" || target.Method != http.MethodPut {
		t.Fatalf("target = %+v, %v", target, err)
	}

	// Copying the ciphertext to another channel or field must fail.
	moved := ch
	moved.ID = "ch-2"
	if _, err := DecryptTarget(ctx, kr, moved); err == nil {
		t.Error("URL of another channel decrypted")
	}
	swapped := ch
	swapped.URLEnc = headers["Authorization"]
	if _, err := DecryptTarget(ctx, kr, swapped); err == nil {
		t.Error("header ciphertext decrypted as URL")
	}

	// An update with the Mask keeps the stored header; a new value replaces it.
	kept, err := EncryptHeaders(kr, "ch-1", map[string]string{"Authorization": Mask, "X-New": "v"}, headers, Mask)
	if err != nil || kept["Authorization"] != headers["Authorization"] || !secrets.IsEncrypted(kept["X-New"]) {
		t.Fatalf("kept = %v, %v", kept, err)
	}
	if _, err := EncryptHeaders(kr, "ch-1", map[string]string{"X-Unknown": Mask}, headers, Mask); err == nil {
		t.Error("masked value without stored header accepted")
	}
}
