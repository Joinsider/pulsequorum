// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/joinsider/pulsequorum/internal/api/gen"
	"github.com/joinsider/pulsequorum/internal/auth"
)

// Helpers for the tests that run with real local authentication (ADR 0008).

// testPassword meets the length policy (FA-AUTH-01).
const testPassword = "correct horse battery"

// bearer returns the header arguments for a Bearer access token.
func bearer(token string) []string { return []string{"Authorization", "Bearer " + token} }

// requestedWith is the CSRF header of the refresh and logout endpoints.
var requestedWith = []string{"X-Requested-With", "pulsequorum"}

// cookieHeader sends a refresh cookie back to the API.
func cookieHeader(c *http.Cookie) []string { return []string{"Cookie", c.Name + "=" + c.Value} }

// refreshCookieOf returns the refresh cookie set by a response, or nil.
func refreshCookieOf(r response) *http.Cookie {
	for _, c := range (&http.Response{Header: r.hdr}).Cookies() {
		if c.Name == refreshCookie {
			return c
		}
	}
	return nil
}

// newLocalAdminEnv returns a local-auth env in which the first platform admin
// "root-admin" exists, plus that admin's access token.
func newLocalAdminEnv(t *testing.T) (*env, string) {
	t.Helper()
	e := newEnvWith(t, true)
	res := e.setup("root-admin", testPassword)
	if res.code != http.StatusCreated {
		t.Fatalf("setup = %d %s", res.code, res.body)
	}
	return e, decode[gen.TokenResponse](t, res).AccessToken
}

// setup calls the setup endpoint with the env's one-time token.
func (e *env) setup(username, password string) response {
	return e.do("POST", "/api/v1/auth/setup", "", map[string]any{
		"setupToken": e.setupToken, "username": username, "password": password,
	})
}

// login calls the login endpoint.
func (e *env) login(username, password string) response {
	return e.do("POST", "/api/v1/auth/login", "", map[string]any{"username": username, "password": password})
}

// mustLogin logs in and returns the access token and the refresh cookie.
func (e *env) mustLogin(username, password string) (string, *http.Cookie) {
	e.t.Helper()
	res := e.login(username, password)
	if res.code != http.StatusOK {
		e.t.Fatalf("login %s = %d %s", username, res.code, res.body)
	}
	return decode[gen.TokenResponse](e.t, res).AccessToken, refreshCookieOf(res)
}

// createUserRes creates a user through the API as the actor.
func (e *env) createUserRes(actor, username, role string) response {
	return e.do("POST", "/api/v1/users", "", map[string]any{
		"username": username, "password": testPassword, "role": role,
	}, bearer(actor)...)
}

// addUser creates a user through the API and logs them in. It returns the user
// ID and the access token.
func (e *env) addUser(actor, username, role string) (string, string) {
	e.t.Helper()
	res := e.createUserRes(actor, username, role)
	if res.code != http.StatusCreated {
		e.t.Fatalf("create %s = %d %s", username, res.code, res.body)
	}
	id := decode[gen.User](e.t, res).Id.String()
	token, _ := e.mustLogin(username, testPassword)
	return id, token
}

// TestLocalAuthFlow walks through the HTTP flow of local authentication
// (FA-AUTH-01..04): setup, login, rotation with reuse detection, logout,
// password change and the immediate effect of disabling a user.
func TestLocalAuthFlow(t *testing.T) {
	t.Parallel()
	e := newEnvWith(t, true)

	t.Run("protected endpoint needs a token", func(t *testing.T) {
		if res := e.do("GET", "/api/v1/monitors", "", nil); res.code != http.StatusUnauthorized {
			t.Fatalf("no token = %d, want 401", res.code)
		}
	})

	t.Run("setup validation", func(t *testing.T) {
		wrong := e.do("POST", "/api/v1/auth/setup", "", map[string]any{
			"setupToken": "not-the-token", "username": "root-admin", "password": testPassword,
		})
		if wrong.code != http.StatusForbidden {
			t.Errorf("wrong setup token = %d, want 403", wrong.code)
		}
		if res := e.setup("ab", testPassword); res.code != http.StatusUnprocessableEntity {
			t.Errorf("short username = %d, want 422", res.code)
		}
		if res := e.setup("root-admin", "short"); res.code != http.StatusUnprocessableEntity {
			t.Errorf("weak password = %d, want 422", res.code)
		}
	})

	var admin string
	var cookie1 *http.Cookie
	t.Run("setup succeeds once", func(t *testing.T) {
		res := e.setup("root-admin", testPassword)
		if res.code != http.StatusCreated {
			t.Fatalf("setup = %d %s", res.code, res.body)
		}
		admin = decode[gen.TokenResponse](t, res).AccessToken
		if admin == "" {
			t.Fatal("setup returned no access token")
		}
		cookie1 = refreshCookieOf(res)
		if cookie1 == nil {
			t.Fatal("setup set no refresh cookie")
		}
		if !cookie1.HttpOnly || !cookie1.Secure || cookie1.SameSite != http.SameSiteStrictMode || cookie1.Path != "/api/v1/auth" {
			t.Errorf("refresh cookie attributes: HttpOnly=%v Secure=%v SameSite=%v Path=%q",
				cookie1.HttpOnly, cookie1.Secure, cookie1.SameSite, cookie1.Path)
		}
	})

	t.Run("setup is closed after the first admin", func(t *testing.T) {
		if res := e.setup("second-admin", testPassword); res.code != http.StatusForbidden {
			t.Errorf("second setup = %d, want 403", res.code)
		}
		// Every token is refused once the first admin exists; a random one as well.
		res := e.do("POST", "/api/v1/auth/setup", "", map[string]any{
			"setupToken": uuid.NewString(), "username": "second-admin", "password": testPassword,
		})
		if res.code != http.StatusForbidden {
			t.Errorf("setup with random token = %d, want 403", res.code)
		}
	})

	var loginWrong, loginUnknown []byte
	t.Run("login failures are indistinguishable", func(t *testing.T) {
		wrong := e.login("root-admin", "wrong password here")
		unknown := e.login("nobody-here", testPassword)
		if wrong.code != http.StatusUnauthorized || unknown.code != http.StatusUnauthorized {
			t.Fatalf("codes = %d / %d, want 401 / 401", wrong.code, unknown.code)
		}
		loginWrong, loginUnknown = wrong.body, unknown.body
		if !bytes.Equal(loginWrong, loginUnknown) {
			t.Errorf("bodies differ:\n%s\n%s", loginWrong, loginUnknown)
		}
	})

	var cookie2 *http.Cookie
	t.Run("refresh requires the CSRF header", func(t *testing.T) {
		res := e.do("POST", "/api/v1/auth/refresh", "", nil, cookieHeader(cookie1)...)
		if res.code != http.StatusBadRequest {
			t.Errorf("refresh without header = %d, want 400", res.code)
		}
	})

	t.Run("refresh rotates the cookie", func(t *testing.T) {
		res := e.do("POST", "/api/v1/auth/refresh", "", nil, append(requestedWith, cookieHeader(cookie1)...)...)
		if res.code != http.StatusOK {
			t.Fatalf("refresh = %d %s", res.code, res.body)
		}
		cookie2 = refreshCookieOf(res)
		if cookie2 == nil || cookie2.Value == cookie1.Value {
			t.Fatalf("refresh did not rotate the cookie: %+v", cookie2)
		}
	})

	t.Run("reusing the old cookie revokes the family", func(t *testing.T) {
		if res := e.do("POST", "/api/v1/auth/refresh", "", nil, append(requestedWith, cookieHeader(cookie1)...)...); res.code != http.StatusUnauthorized {
			t.Fatalf("reuse = %d, want 401", res.code)
		}
		if res := e.do("POST", "/api/v1/auth/refresh", "", nil, append(requestedWith, cookieHeader(cookie2)...)...); res.code != http.StatusUnauthorized {
			t.Fatalf("newest cookie after reuse = %d, want 401", res.code)
		}
	})

	t.Run("logout ends the session", func(t *testing.T) {
		_, cookie3 := e.mustLogin("root-admin", testPassword)
		res := e.do("POST", "/api/v1/auth/logout", "", nil, append(requestedWith, cookieHeader(cookie3)...)...)
		if res.code != http.StatusNoContent {
			t.Fatalf("logout = %d %s", res.code, res.body)
		}
		cleared := refreshCookieOf(res)
		if cleared == nil || cleared.MaxAge >= 0 {
			t.Errorf("logout did not clear the cookie: %+v", cleared)
		}
		if res := e.do("POST", "/api/v1/auth/refresh", "", nil, append(requestedWith, cookieHeader(cookie3)...)...); res.code != http.StatusUnauthorized {
			t.Errorf("refresh after logout = %d, want 401", res.code)
		}
	})

	t.Run("me describes the caller", func(t *testing.T) {
		res := e.do("GET", "/api/v1/me", "", nil, bearer(admin)...)
		if res.code != http.StatusOK {
			t.Fatalf("me = %d %s", res.code, res.body)
		}
		me := decode[gen.Me](t, res)
		if me.Username != "root-admin" || me.TenantId != "default" || string(me.AuthMode) != "local" ||
			len(me.Roles) != 1 || me.Roles[0] != "platform-admin" {
			t.Errorf("me = %+v", me)
		}
	})

	t.Run("password change", func(t *testing.T) {
		const newPassword = "another long password"
		token, oldCookie := e.mustLogin("root-admin", testPassword)
		wrong := e.do("PUT", "/api/v1/me/password", "", map[string]any{
			"currentPassword": "not the current one", "newPassword": newPassword,
		}, bearer(token)...)
		if wrong.code != http.StatusUnprocessableEntity {
			t.Fatalf("wrong current password = %d, want 422", wrong.code)
		}
		res := e.do("PUT", "/api/v1/me/password", "", map[string]any{
			"currentPassword": testPassword, "newPassword": newPassword,
		}, bearer(token)...)
		if res.code != http.StatusNoContent {
			t.Fatalf("change password = %d %s", res.code, res.body)
		}
		if refreshCookieOf(res) == nil {
			t.Error("password change set no new refresh cookie")
		}
		if res := e.do("POST", "/api/v1/auth/refresh", "", nil, append(requestedWith, cookieHeader(oldCookie)...)...); res.code != http.StatusUnauthorized {
			t.Errorf("old cookie after password change = %d, want 401", res.code)
		}
		if res := e.login("root-admin", testPassword); res.code != http.StatusUnauthorized {
			t.Errorf("old password still accepted: %d", res.code)
		}
		e.mustLogin("root-admin", newPassword)
	})

	t.Run("disabled user loses access at once", func(t *testing.T) {
		id, token := e.addUser(admin, "carol", "tenant-editor")
		if res := e.do("GET", "/api/v1/monitors", "", nil, bearer(token)...); res.code != http.StatusOK {
			t.Fatalf("carol before disable = %d", res.code)
		}
		res := e.do("PUT", "/api/v1/users/"+id, "", map[string]any{"disabled": true}, bearer(admin)...)
		if res.code != http.StatusOK {
			t.Fatalf("disable = %d %s", res.code, res.body)
		}
		if res := e.do("GET", "/api/v1/monitors", "", nil, bearer(token)...); res.code != http.StatusUnauthorized {
			t.Errorf("carol's access token after disable = %d, want 401", res.code)
		}
	})
}

// TestLoginThrottling checks FA-AUTH-04 (ADR 0008): failures 1 to 5 answer
// 401; the 6th attempt answers 429 with Retry-After, even with the right password.
func TestLoginThrottling(t *testing.T) {
	t.Parallel()
	e := newEnvWith(t, true)
	if res := e.setup("root-admin", testPassword); res.code != http.StatusCreated {
		t.Fatalf("setup = %d %s", res.code, res.body)
	}
	for i := range 5 {
		if res := e.login("root-admin", "wrong password here"); res.code != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d, want 401", i+1, res.code)
		}
	}
	res := e.login("root-admin", testPassword)
	if res.code != http.StatusTooManyRequests {
		t.Fatalf("attempt after 5 failures = %d %s, want 429", res.code, res.body)
	}
	if res.hdr.Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
}

// TestLoginOverlongPassword checks that a password longer than the policy
// never matches, even if its first 256 bytes are the real password.
func TestLoginOverlongPassword(t *testing.T) {
	t.Parallel()
	e := newEnvWith(t, true)
	stored := testPassword + strings.Repeat("x", auth.MaxPasswordLen-len(testPassword))
	if res := e.setup("root-admin", stored); res.code != http.StatusCreated {
		t.Fatalf("setup = %d %s", res.code, res.body)
	}
	if res := e.login("root-admin", stored+"y"); res.code != http.StatusUnauthorized {
		t.Errorf("257-byte password = %d, want 401", res.code)
	}
	if res := e.login("root-admin", stored); res.code != http.StatusOK {
		t.Errorf("stored 256-byte password = %d, want 200", res.code)
	}
}

// TestChangePasswordThrottling checks that wrong current passwords count as
// account failures: after five wrong attempts even the right one answers 429.
func TestChangePasswordThrottling(t *testing.T) {
	t.Parallel()
	e, admin := newLocalAdminEnv(t)
	for i := range 5 {
		res := e.do("PUT", "/api/v1/me/password", "", map[string]any{
			"currentPassword": "wrong current password", "newPassword": "another long password",
		}, bearer(admin)...)
		if res.code != http.StatusUnprocessableEntity {
			t.Fatalf("wrong current %d = %d, want 422", i+1, res.code)
		}
	}
	res := e.do("PUT", "/api/v1/me/password", "", map[string]any{
		"currentPassword": testPassword, "newPassword": "another long password",
	}, bearer(admin)...)
	if res.code != http.StatusTooManyRequests {
		t.Fatalf("correct current after 5 wrong = %d %s, want 429", res.code, res.body)
	}
	if res.hdr.Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
}
