// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package oidc

import "testing"

// FuzzAuthenticate feeds arbitrary bearer tokens to the verifier. It must
// never panic, and only the unmodified valid token may be accepted.
func FuzzAuthenticate(f *testing.F) {
	h := newHarness(f)
	valid := h.idp.Sign(h.claims())
	f.Add(valid)
	f.Add(h.idp.SignNone(h.claims()))
	f.Add("a.b.c")
	f.Add("..")
	f.Add("eyJhbGciOiJSUzI1NiJ9..")
	f.Fuzz(func(t *testing.T, token string) {
		p, err := h.authenticate(token)
		if err == nil && token != valid {
			t.Fatalf("accepted modified token %q as %+v", token, p)
		}
	})
}
