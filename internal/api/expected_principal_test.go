// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The guard reads the principal humanOrAdminAuth installed, so it must run after it: in local mode the
// operator is "local:test", and a draft composed under that person passes while one composed under the
// pre-auth fallback ("admin-token") is refused. With the guard ahead of auth, every console write in
// local or SSO mode would be refused, because only the fallback is visible there.
func TestExpectedPrincipalGuardRunsAfterAuth(t *testing.T) {
	srv, _, _ := govEscapeFixture(t, &capStore{})
	cfg := srv.cfg
	cfg.OIDC = nil
	cfg.LocalMode = true
	cfg.LocalOperator = "local:test"
	cfg.LocalLoopback = true
	h := New(cfg).Handler()
	post := func(who string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(`{"agent":"claude-code","task":"t"}`))
		r.Header.Set(expectedPrincipalHeader, url.PathEscape(who))
		r.Host, r.RemoteAddr = "127.0.0.1", "127.0.0.1:54321"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := post("local:test"); w.Code != http.StatusCreated {
		t.Errorf("a draft under the signed-in operator = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	if w := post(adminTokenPrincipal); w.Code != http.StatusPreconditionFailed || errorReason(w) != reasonExpectedPrincipalMismatch {
		t.Errorf("a draft under the fallback principal = %d %q, want 412 expected_principal_mismatch; body=%s", w.Code, errorReason(w), w.Body.String())
	}
}
