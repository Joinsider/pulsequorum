// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package configsecret

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/joinsider/uptimecluster/internal/secrets"
	"github.com/joinsider/uptimecluster/internal/store"
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

const schema = `{
  "type": "object",
  "properties": {
    "url": {"type": "string"},
    "port": {"type": "integer"},
    "headers": {"type": "object", "additionalProperties": {"type": "string", "writeOnly": true}},
    "auth": {
      "type": "object",
      "properties": {
        "type": {"type": "string"},
        "username": {"type": "string"},
        "password": {"type": "string", "writeOnly": true},
        "token": {"type": "string", "writeOnly": true}
      }
    },
    "servers": {"type": "array", "items": {"type": "object", "properties": {
      "host": {"type": "string"}, "secret": {"type": "string", "writeOnly": true}}}},
    "variant": {"oneOf": [{"type": "object", "properties": {"key": {"type": "string", "writeOnly": true}}}]}
  }
}`

func paths(t *testing.T) Paths {
	t.Helper()
	p, err := PathsOf(json.RawMessage(schema))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func asMap(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPathsOf(t *testing.T) {
	want := Paths{
		{"auth", "password"}, {"auth", "token"}, {"headers", "*"}, {"servers", "*", "secret"}, {"variant", "key"},
	}
	if got := paths(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	if _, err := PathsOf(json.RawMessage(`{"properties":{"a":{"$ref":"#/$defs/x"}}}`)); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("$ref: err = %v, want ErrUnsupportedSchema", err)
	}
}

// ADR 0015: secrets are encrypted, masked in the stored configuration and
// restored exactly (numbers included) when a check runs.
func TestSplitAndResolve(t *testing.T) {
	ctx := context.Background()
	kr := testKeyring(t)
	in := json.RawMessage(`{"url":"https://example.com","port":8443,
		"headers":{"Authorization":"Bearer hdr-secret-1","X/Odd~Name":"odd-secret-2"},
		"auth":{"type":"basic","username":"bob","password":"pw-secret-3"},
		"servers":[{"host":"a","secret":"srv-secret-4"},{"host":"b"}]}`)

	masked, sec, err := Split(kr, paths(t), "m1", in, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, plain := range []string{"hdr-secret-1", "odd-secret-2", "pw-secret-3", "srv-secret-4"} {
		if strings.Contains(string(masked), plain) {
			t.Errorf("masked config contains %q: %s", plain, masked)
		}
		for ptr, c := range sec {
			if strings.Contains(c, plain) || !secrets.IsEncrypted(c) {
				t.Errorf("secret %s is not an opaque ciphertext: %s", ptr, c)
			}
		}
	}
	wantPtrs := []string{"/auth/password", "/headers/Authorization", "/headers/X~1Odd~0Name", "/servers/0/secret"}
	if got := sortedKeys(sec); !reflect.DeepEqual(got, wantPtrs) {
		t.Fatalf("pointers = %v, want %v", got, wantPtrs)
	}
	m := asMap(t, masked)
	if m["auth"].(map[string]any)["password"] != Mask || m["auth"].(map[string]any)["username"] != "bob" {
		t.Errorf("auth not masked correctly: %v", m["auth"])
	}

	resolved, err := Resolve(ctx, kr, "m1", masked, sec)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(asMap(t, resolved), asMap(t, in)) {
		t.Fatalf("resolved = %s\nwant %s", resolved, in)
	}
	if !strings.Contains(string(resolved), `"port":8443`) {
		t.Errorf("number changed: %s", resolved)
	}

	// The ciphertexts are bound to the monitor (AAD).
	if _, err := Resolve(ctx, kr, "m2", masked, sec); err == nil {
		t.Error("secrets of m1 resolved for m2")
	}
}

func TestSplitUpdateSemantics(t *testing.T) {
	kr := testKeyring(t)
	p := paths(t)
	_, prev, err := Split(kr, p, "m1", json.RawMessage(
		`{"headers":{"Authorization":"a-secret","X-Key":"k-secret"},"auth":{"type":"basic","password":"pw-secret"}}`), nil)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		in       string
		wantKeep []string // pointers whose old ciphertext must be kept
		wantNew  []string // pointers with a new ciphertext
		wantGone []string // pointers that must be dropped
		wantErr  error
	}{
		{
			name:     "mask keeps, absent password keeps, removed header drops",
			in:       `{"headers":{"Authorization":"***"},"auth":{"type":"basic"}}`,
			wantKeep: []string{"/headers/Authorization", "/auth/password"},
			wantGone: []string{"/headers/X-Key"},
		},
		{
			name:     "new value replaces",
			in:       `{"headers":{"Authorization":"***","X-Key":"new-secret"},"auth":{"type":"basic","password":"***"}}`,
			wantKeep: []string{"/headers/Authorization", "/auth/password"},
			wantNew:  []string{"/headers/X-Key"},
		},
		{
			name:     "removing the auth object drops its secrets",
			in:       `{"headers":{"Authorization":"***","X-Key":"***"}}`,
			wantKeep: []string{"/headers/Authorization", "/headers/X-Key"},
			wantGone: []string{"/auth/password"},
		},
		{
			name:     "empty string deletes",
			in:       `{"headers":{"Authorization":"","X-Key":"***"},"auth":{"type":"basic","password":""}}`,
			wantKeep: []string{"/headers/X-Key"},
			wantGone: []string{"/headers/Authorization", "/auth/password"},
		},
		{
			name:    "mask for an unknown header is rejected",
			in:      `{"headers":{"X-Other":"***"}}`,
			wantErr: ErrMaskWithoutSecret,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			masked, got, err := Split(kr, p, "m1", json.RawMessage(tc.in), prev)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, ptr := range tc.wantKeep {
				if got[ptr] != prev[ptr] {
					t.Errorf("%s not kept", ptr)
				}
			}
			for _, ptr := range tc.wantNew {
				if got[ptr] == "" || got[ptr] == prev[ptr] {
					t.Errorf("%s not replaced", ptr)
				}
			}
			for _, ptr := range tc.wantGone {
				if _, ok := got[ptr]; ok {
					t.Errorf("%s still present", ptr)
				}
				if segs, _ := parsePointer(ptr); lookup(asAny(t, masked), segs) != nil {
					t.Errorf("%s still in config: %s", ptr, masked)
				}
			}
			// Every stored secret has its mask in the configuration.
			if _, err := Resolve(context.Background(), kr, "m1", masked, got); err != nil {
				t.Errorf("resolve: %v", err)
			}
		})
	}

	// Without previous secrets (new monitor or type change) a mask is invalid.
	if _, _, err := Split(kr, p, "m1", json.RawMessage(`{"auth":{"type":"basic","password":"***"}}`), nil); !errors.Is(err, ErrMaskWithoutSecret) {
		t.Fatalf("mask without previous: err = %v", err)
	}
}

func asAny(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	v, err := decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestReencrypt(t *testing.T) {
	ctx := context.Background()
	kr := testKeyring(t)
	masked, sec, err := Split(kr, paths(t), "m1", json.RawMessage(`{"auth":{"type":"bearer","token":"tok-secret"}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	cloned, err := Reencrypt(ctx, kr, "m1", "m2", sec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(ctx, kr, "m2", masked, cloned); err != nil {
		t.Fatalf("clone does not resolve for m2: %v", err)
	}
	if _, err := Resolve(ctx, kr, "m1", masked, cloned); err == nil {
		t.Fatal("clone still resolves for m1")
	}
}

func TestMaskConfigAndHasPlaintext(t *testing.T) {
	p := paths(t)
	legacy := json.RawMessage(`{"url":"https://x","auth":{"type":"basic","password":"legacy-pw"},"headers":{"A":"***"}}`)
	plain, err := HasPlaintext(p, legacy)
	if err != nil || !plain {
		t.Fatalf("HasPlaintext(legacy) = %v, %v", plain, err)
	}
	masked, err := MaskConfig(p, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(masked), "legacy-pw") {
		t.Fatalf("not masked: %s", masked)
	}
	if plain, _ := HasPlaintext(p, masked); plain {
		t.Fatal("masked config reported as plaintext")
	}
}
