// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package diskstat

import (
	"errors"
	"runtime"
	"testing"
)

func TestFreeRatio(t *testing.T) {
	tests := []struct {
		name    string
		path    func(t *testing.T) string
		wantErr bool
	}{
		{"temp dir", func(t *testing.T) string { return t.TempDir() }, false},
		{"missing path", func(t *testing.T) string { return t.TempDir() + "/missing" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ratio, err := FreeRatio(tt.path(t))
			if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
				if !errors.Is(err, ErrUnsupported) {
					t.Fatalf("err = %v, want ErrUnsupported", err)
				}
				return
			}
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ratio = %v, want error", ratio)
				}
				return
			}
			if err != nil {
				t.Fatalf("FreeRatio: %v", err)
			}
			if ratio <= 0 || ratio > 1 {
				t.Errorf("ratio = %v, want 0 < ratio <= 1", ratio)
			}
		})
	}
}
