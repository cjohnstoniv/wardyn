// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestRunIdentitySubject_NoSSOAdminToken pins what subject a CI run carries on
// a deployment with no OIDC: the shared admin token has no person behind it, so
// the run is minted for the fixed, non-empty mechanism subject "admin-token"
// (never ""). A person's identity or the local operator's override it, and a
// forged X-Wardyn-Principal header never does. The second half grades that
// subject as a credential holder: with no OIDC it can hold its own model
// credential (not_configured, then live), under OIDC it is not_applicable.
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

	// The property CI without SSO rests on: what the subject may DO depends on
	// s.cfg.OIDC (providerAccessMechanism, credentialOwner), which the subject
	// alone cannot show.
	h, sec := newSecretsHarness(t)
	h.srv.cfg.OIDC = nil
	p := paKeyProvider("ci-fake", types.ModelProviderBedrockBearer)
	if got := h.srv.providerAccessFor(ctx, p, adminTokenPrincipal).State; got != modelAccessNotConfigured {
		t.Fatalf("no OIDC, no key: admin-token state = %q, want %q", got, modelAccessNotConfigured)
	}
	_ = sec.For(adminTokenPrincipal).Put(ctx, providerSecretName(p.UID, providerKeyPart), []byte("fake-bearer-token-0123456789"))
	if got := h.srv.providerAccessFor(ctx, p, adminTokenPrincipal).State; got != modelAccessLive {
		t.Fatalf("no OIDC, own key stored: admin-token state = %q, want %q", got, modelAccessLive)
	}
	h.srv.cfg.OIDC = &oidc.Authenticator{}
	if got := h.srv.providerAccessFor(ctx, p, adminTokenPrincipal).State; got != modelAccessNotApplicable {
		t.Fatalf("under OIDC: admin-token state = %q, want %q", got, modelAccessNotApplicable)
	}
}
