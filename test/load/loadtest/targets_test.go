// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestTargetFarm(t *testing.T) {
	srv := httptest.NewServer(newTargetFarm(3, 0).handler())
	defer srv.Close()

	do := func(method, path string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantBody   string
	}{
		{"target up", http.MethodGet, "/t/1", http.StatusOK, "ok"},
		{"out of range", http.MethodGet, "/t/3", http.StatusNotFound, ""},
		{"negative index", http.MethodGet, "/t/-1", http.StatusNotFound, ""},
		{"non numeric", http.MethodGet, "/t/x", http.StatusNotFound, ""},
		{"control down", http.MethodPost, "/control/1/down", http.StatusNoContent, ""},
		{"target down", http.MethodGet, "/t/1", http.StatusServiceUnavailable, "down"},
		{"other target unaffected", http.MethodGet, "/t/0", http.StatusOK, "ok"},
		{"control up", http.MethodPost, "/control/1/up", http.StatusNoContent, ""},
		{"target up again", http.MethodGet, "/t/1", http.StatusOK, "ok"},
		{"control out of range", http.MethodPost, "/control/9/down", http.StatusNotFound, ""},
		{"control needs POST", http.MethodGet, "/control/1/down", http.StatusMethodNotAllowed, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := do(tt.method, tt.path)
			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d", status, tt.wantStatus)
			}
			if tt.wantBody != "" && body != tt.wantBody {
				t.Errorf("body = %q, want %q", body, tt.wantBody)
			}
		})
	}
}

func TestTargetFarmConcurrentControl(t *testing.T) {
	farm := newTargetFarm(8, 0)
	srv := httptest.NewServer(farm.handler())
	defer srv.Close()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				resp, err := srv.Client().Post(fmt.Sprintf("%s/control/%d/down", srv.URL, i), "text/plain", nil)
				if err == nil {
					_ = resp.Body.Close()
				}
				resp, err = srv.Client().Get(fmt.Sprintf("%s/t/%d", srv.URL, i))
				if err == nil {
					_ = resp.Body.Close()
				}
			}
		}()
	}
	wg.Wait()
	for i := 0; i < 8; i++ {
		if !farm.down[i].Load() {
			t.Errorf("target %d not down after concurrent control", i)
		}
	}
}
