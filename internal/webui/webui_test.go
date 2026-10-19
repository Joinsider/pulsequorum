// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":         {Data: []byte("<html>app</html>")},
		"main-4K2ZQ3XH.js":   {Data: []byte("console.log(1)")},
		"favicon.ico":        {Data: []byte("ico")},
		"media/logo.svg":     {Data: []byte("<svg/>")},
		".gitkeep":           {Data: nil},
		"assets/.secret.txt": {Data: []byte("x")},
	}
}

func TestNewHandler(t *testing.T) {
	h := NewHandler(testFS())
	tests := []struct {
		path       string
		wantStatus int
		wantBody   string
		wantCache  string
	}{
		{"/", http.StatusOK, "app", "no-cache"},
		{"/index.html", http.StatusOK, "app", "no-cache"},
		{"/monitors/42", http.StatusOK, "app", "no-cache"},
		{"/main-4K2ZQ3XH.js", http.StatusOK, "console.log", "public, max-age=31536000, immutable"},
		{"/favicon.ico", http.StatusOK, "ico", "public, max-age=3600"},
		{"/media/logo.svg", http.StatusOK, "<svg/>", "public, max-age=3600"},
		{"/missing-ABCDEFGH.js", http.StatusNotFound, "", ""},
		{"/.gitkeep", http.StatusNotFound, "", ""},
		{"/assets/.secret.txt", http.StatusNotFound, "", ""},
		{"/../../etc/passwd", http.StatusOK, "app", "no-cache"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantBody != "" && !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("body = %q, want containing %q", rec.Body.String(), tt.wantBody)
			}
			if got := rec.Header().Get("Cache-Control"); tt.wantCache != "" && got != tt.wantCache {
				t.Errorf("Cache-Control = %q, want %q", got, tt.wantCache)
			}
		})
	}
}

func TestNewHandlerNotBuilt(t *testing.T) {
	h := NewHandler(fstest.MapFS{".gitkeep": {}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "make build") {
		t.Fatalf("got %d %q, want 503 with build hint", rec.Code, rec.Body.String())
	}
}

func TestNewHandlerRejectsWrites(t *testing.T) {
	rec := httptest.NewRecorder()
	NewHandler(testFS()).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
