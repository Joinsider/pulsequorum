// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package fake

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTargetAnswersAndCountsPerPath(t *testing.T) {
	tg := NewTarget()
	srv := httptest.NewServer(tg)
	t.Cleanup(srv.Close)

	get := func(path string) int {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	if got := get("/a"); got != http.StatusOK {
		t.Errorf("healthy status = %d, want 200", got)
	}
	tg.SetFailing(true)
	if !tg.Failing() {
		t.Error("Failing() = false after SetFailing(true)")
	}
	if got := get("/a"); got != http.StatusServiceUnavailable {
		t.Errorf("failing status = %d, want 503", got)
	}
	_ = get("/b")

	if n := tg.Hits("/a"); n != 2 {
		t.Errorf("Hits(/a) = %d, want 2", n)
	}
	if n := tg.Hits("/b"); n != 1 {
		t.Errorf("Hits(/b) = %d, want 1", n)
	}
	if n := tg.Hits("/missing"); n != 0 {
		t.Errorf("Hits(/missing) = %d, want 0", n)
	}
	if n := tg.TotalHits(); n != 3 {
		t.Errorf("TotalHits() = %d, want 3", n)
	}
}

func TestListenBindsEphemeralPort(t *testing.T) {
	srv, err := Listen("127.0.0.1:0", NewTarget())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	if srv.Port() == 0 {
		t.Error("Port() = 0 for an ephemeral listener")
	}
}
