// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRunIdentitySubject_NoSSOAdminToken pins what subject a CI run carries on
// a deployment with no OIDC: the shared admin token has no person behind it, so
// the run is minted for the fixed, non-empty mechanism subject "admin-token"
// (never ""), and that is the namespace its model-provider credential is read
// from. A person's identity or the local operator's override it, and a forged
// X-Wardyn-Principal header never does.
func TestRunIdentitySubject_NoSSOAdminToken(t *testing.T) {
	ctx := context.Background()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", nil).WithContext(ctx)
	_, actor := actorFromRequest(req)
	if got := runIdentitySubject(ctx, actor); got != adminTokenPrincipal {
		t.Errorf("no-SSO admin token: subject = %q, want %q", got, adminTokenPrincipal)
	}

	forged := httptest.NewRequest(http.MethodPost, "/v1/runs", nil)
	forged.Header.Set("X-Wardyn-Principal", "alice@example.com")
	_, actor = actorFromRequest(forged)
	if got := runIdentitySubject(forged.Context(), actor); got != adminTokenPrincipal {
		t.Errorf("admin token with a forged principal header: subject = %q, want %q", got, adminTokenPrincipal)
	}

	person := withOIDCHuman(ctx, "alice@example.com")
	_, actor = actorFromRequest(httptest.NewRequest(http.MethodPost, "/v1/runs", nil).WithContext(person))
	if got := runIdentitySubject(person, actor); got != "alice@example.com" {
		t.Errorf("SSO person: subject = %q, want their sub", got)
	}

	local := withLocalPrincipal(ctx, "local-operator")
	if got := runIdentitySubject(local, "someone-else"); got != "local-operator" {
		t.Errorf("local mode: subject = %q, want the local principal", got)
	}
}
