// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package secrets

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixedKey returns a KeySize key filled with b.
func fixedKey(b byte) []byte { return bytes.Repeat([]byte{b}, KeySize) }

// TestTransferable covers ADR 0011 option B: only a generated KEK is handed
// over to joining voters; external keys stay with the operator.
func TestTransferable(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(t.TempDir(), "k.key")
	if err := os.WriteFile(keyFile, []byte(base64.StdEncoding.EncodeToString(fixedKey(5))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		envKey   string
		file     string
		wantOK   bool
		wantKind string
	}{
		{name: "generated in data dir", wantOK: true, wantKind: SourceGenerated},
		{name: "MASTER_KEY", envKey: testKey(1), wantOK: false, wantKind: SourceEnv},
		{name: "MASTER_KEY_FILE", file: keyFile, wantOK: false, wantKind: SourceFile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kekDir := dir
			if tt.wantKind != SourceGenerated {
				kekDir = t.TempDir()
			}
			kek, err := LoadKEK(kekDir, tt.envKey, tt.file)
			if err != nil {
				t.Fatalf("LoadKEK: %v", err)
			}
			if kek.Source != tt.wantKind {
				t.Fatalf("Source = %q, want %q", kek.Source, tt.wantKind)
			}
			key, ok := kek.Transferable()
			if ok != tt.wantOK {
				t.Fatalf("Transferable ok = %v, want %v", ok, tt.wantOK)
			}
			if ok {
				if len(key) != KeySize {
					t.Fatalf("key has %d bytes, want %d", len(key), KeySize)
				}
				if KeyFingerprint(key) != kek.Fingerprint {
					t.Errorf("transferred key does not match the KEK fingerprint")
				}
			} else if key != nil {
				t.Errorf("Transferable returned a key for source %q", kek.Source)
			}
		})
	}
}

// TestTransferableIsCopy checks that changing the returned key does not
// change what the KEK hands over next.
func TestTransferableIsCopy(t *testing.T) {
	kek, err := LoadKEK(t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	first, ok := kek.Transferable()
	if !ok {
		t.Fatal("generated KEK is not transferable")
	}
	want := bytes.Clone(first)
	for i := range first {
		first[i] = 0
	}
	second, _ := kek.Transferable()
	if !bytes.Equal(second, want) {
		t.Error("mutating the returned key changed the KEK")
	}
	second[0] ^= 0xff
	third, _ := kek.Transferable()
	if !bytes.Equal(third, want) {
		t.Error("mutating a previously returned key changed the KEK")
	}
}

func TestInstallGeneratedStoresKey(t *testing.T) {
	dir := t.TempDir()
	key := fixedKey(7)
	fp := KeyFingerprint(key)

	if err := InstallGenerated(dir, key, fp); err != nil {
		t.Fatalf("InstallGenerated: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, GeneratedKEKFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}

	kek, err := LoadKEK(dir, "", "")
	if err != nil {
		t.Fatalf("LoadKEK after install: %v", err)
	}
	if kek.Fingerprint != fp || kek.Source != SourceGenerated {
		t.Errorf("KEK = fingerprint %q source %q, want %q/%q", kek.Fingerprint, kek.Source, fp, SourceGenerated)
	}
	if got, ok := kek.Transferable(); !ok || !bytes.Equal(got, key) {
		t.Errorf("installed key is not transferable unchanged")
	}
}

func TestInstallGeneratedRejects(t *testing.T) {
	short := fixedKey(9)[:KeySize-1]
	tests := []struct {
		name string
		key  []byte
		fp   string
	}{
		{"fingerprint of another key", fixedKey(7), KeyFingerprint(fixedKey(8))},
		{"empty fingerprint", fixedKey(7), ""},
		{"key too short", short, KeyFingerprint(short)},
		{"key too long", append(fixedKey(7), 1), KeyFingerprint(append(fixedKey(7), 1))},
		{"nil key", nil, KeyFingerprint(nil)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			err := InstallGenerated(dir, tt.key, tt.fp)
			if err == nil {
				t.Fatal("InstallGenerated accepted the key")
			}
			if _, statErr := os.Stat(filepath.Join(dir, GeneratedKEKFile)); !os.IsNotExist(statErr) {
				t.Errorf("a file was written despite the error: %v", statErr)
			}
		})
	}
}

func TestInstallGeneratedExisting(t *testing.T) {
	key := fixedKey(7)
	fp := KeyFingerprint(key)

	t.Run("identical key is a no-op", func(t *testing.T) {
		dir := t.TempDir()
		if err := InstallGenerated(dir, key, fp); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, GeneratedKEKFile)
		before, err := os.ReadFile(path) //nolint:gosec // G304: path inside t.TempDir()
		if err != nil {
			t.Fatal(err)
		}
		if err := InstallGenerated(dir, key, fp); err != nil {
			t.Fatalf("second install with the same key: %v", err)
		}
		after, err := os.ReadFile(path) //nolint:gosec // G304: path inside t.TempDir()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Error("file changed although the key was identical")
		}
	})

	t.Run("different key is refused and not overwritten", func(t *testing.T) {
		dir := t.TempDir()
		if err := InstallGenerated(dir, key, fp); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, GeneratedKEKFile)
		before, err := os.ReadFile(path) //nolint:gosec // G304: path inside t.TempDir()
		if err != nil {
			t.Fatal(err)
		}
		other := fixedKey(8)
		err = InstallGenerated(dir, other, KeyFingerprint(other))
		if err == nil || !strings.Contains(err.Error(), "another cluster") {
			t.Fatalf("error = %v, want one mentioning another cluster", err)
		}
		after, err := os.ReadFile(path) //nolint:gosec // G304: path inside t.TempDir()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Error("existing master key was overwritten")
		}
	})

	t.Run("unreadable existing key is refused and not overwritten", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, GeneratedKEKFile)
		garbage := []byte("not a key\n")
		if err := os.WriteFile(path, garbage, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := InstallGenerated(dir, key, fp); err == nil {
			t.Fatal("InstallGenerated succeeded over an invalid key file")
		}
		after, err := os.ReadFile(path) //nolint:gosec // G304: path inside t.TempDir()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(garbage, after) {
			t.Error("invalid key file was overwritten")
		}
	})
}

// TestKeyFingerprintMatchesLoadKEK checks that the fingerprint sent at join
// is the one a node computes once it has loaded the same key.
func TestKeyFingerprintMatchesLoadKEK(t *testing.T) {
	random := make([]byte, KeySize)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		key  []byte
	}{
		{"constant 1", fixedKey(1)},
		{"constant 2", fixedKey(2)},
		{"random", random},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := KeyFingerprint(tt.key)
			if len(want) != 32 {
				t.Errorf("fingerprint %q has %d hex digits, want 32", want, len(want))
			}

			env := base64.StdEncoding.EncodeToString(tt.key)
			kek, err := LoadKEK(t.TempDir(), env, "")
			if err != nil {
				t.Fatalf("LoadKEK(env): %v", err)
			}
			if kek.Fingerprint != want {
				t.Errorf("env fingerprint = %q, want %q", kek.Fingerprint, want)
			}

			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, GeneratedKEKFile), []byte(env+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			kek, err = LoadKEK(dir, "", "")
			if err != nil {
				t.Fatalf("LoadKEK(generated): %v", err)
			}
			if kek.Fingerprint != want {
				t.Errorf("generated fingerprint = %q, want %q", kek.Fingerprint, want)
			}
		})
	}

	if KeyFingerprint(fixedKey(1)) == KeyFingerprint(fixedKey(2)) {
		t.Error("different keys have the same fingerprint")
	}
}
