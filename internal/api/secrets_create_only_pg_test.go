// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// putSecretIfNew is a secret PUT as the console's Add sends it when its list did not have the name.
func putSecretIfNew(t *testing.T, srv *Server, path string, cookie *http.Cookie, value string, createOnly bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPut, path, strings.NewReader(`{"value":"`+value+`"}`))
	r.AddCookie(cookie)
	if createOnly {
		r.Header.Set("If-None-Match", "*")
	}
	w := httptest.NewRecorder()
	panicFails(t, srv.Handler()).ServeHTTP(w, r)
	return w
}

// TestPG_SecretPutIfNoneMatchCreatesOnly: a create-only PUT of a name stored since the client's list was read
// answers 409 secret_exists and leaves the stored value as it was; on a new name it stores; without the header the
// same PUT still replaces. The explicit personal route shares the write, so it refuses the same way.
func TestPG_SecretPutIfNoneMatchCreatesOnly(t *testing.T) {
	e := newOwnerOnlyPG(t)
	srv := e.h.srv
	read := func(owner, name string) string {
		v, err := e.sec.For(owner).Get(secretstore.WithPurpose(context.Background(), secretstore.PurposeDispatch), name)
		if err != nil {
			t.Fatalf("read %s/%s: %v", owner, name, err)
		}
		return string(v)
	}
	if w := putSecretIfNew(t, srv, "/api/v1/secrets/npm-token", e.admin, "npm-first-value", false); w.Code != http.StatusNoContent {
		t.Fatalf("first store: %d %s", w.Code, w.Body.String())
	}
	w := putSecretIfNew(t, srv, "/api/v1/secrets/npm-token", e.admin, "npm-second-value", true)
	if w.Code != http.StatusConflict || errorReason(w) != "secret_exists" {
		t.Fatalf("create-only over a stored name: %d reason %q, want 409 secret_exists; body=%s", w.Code, errorReason(w), w.Body.String())
	}
	if got := read("", "npm-token"); got != "npm-first-value" {
		t.Errorf("a refused create-only write replaced the value: %q", got)
	}
	if w := putSecretIfNew(t, srv, "/api/v1/secrets/pypi-token", e.admin, "pypi-first-value", true); w.Code != http.StatusNoContent {
		t.Errorf("create-only of a new name: %d %s", w.Code, w.Body.String())
	}
	if w := putSecretIfNew(t, srv, "/api/v1/secrets/npm-token", e.admin, "npm-second-value", false); w.Code != http.StatusNoContent || read("", "npm-token") != "npm-second-value" {
		t.Errorf("the plain PUT no longer replaces: %d %s", w.Code, w.Body.String())
	}

	member := ssoSession(t, "sub-create-only", "create-only@corp.example", oidc.RoleUser)
	if w := putSecretIfNew(t, srv, "/api/v1/me/secrets/own-token", member, "own-first-value", true); w.Code != http.StatusNoContent {
		t.Fatalf("member's first create-only store: %d %s", w.Code, w.Body.String())
	}
	if w := putSecretIfNew(t, srv, "/api/v1/me/secrets/own-token", member, "own-second-value", true); w.Code != http.StatusConflict || errorReason(w) != "secret_exists" {
		t.Errorf("member's create-only over their own stored name: %d reason %q, want 409 secret_exists", w.Code, errorReason(w))
	}
}
