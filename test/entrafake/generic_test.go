// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package entrafake_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/cjohnstoniv/wardyn/test/entrafake"
)

const genericVerifier = "verifier-0123456789012345678901234567890123456789"

// refreshWith posts a refresh_token grant, with scope when it is not "".
func refreshWith(t *testing.T, s *entrafake.Server, token, scope string) (int, map[string]any) {
	t.Helper()
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {token}}
	if scope != "" {
		form.Set("scope", scope)
	}
	return postToken(t, s, form)
}

// Generic mode is a plain OIDC authority: any scope is granted as asked, and no token response names it.
func TestGenericModeOmitsScopeAndAcceptsAnyScope(t *testing.T) {
	s := entrafake.New()
	defer s.Close()
	s.SetGenericMode(true)

	body := redeemCode(t, s, genericVerifier, "openid", "offline_access", "api.read", "https://other.example/thing")
	if _, has := body["scope"]; has {
		t.Fatalf("generic code response names a scope: %v", body["scope"])
	}
	refresh, _ := body["refresh_token"].(string)
	if refresh == "" {
		t.Fatalf("no refresh token in %v", body)
	}
	status, next := refreshWith(t, s, refresh, "")
	if status != http.StatusOK {
		t.Fatalf("refresh: status %d body %v", status, next)
	}
	if _, has := next["scope"]; has {
		t.Fatalf("generic refresh response names a scope: %v", next["scope"])
	}
	if got := s.RefreshRequestScopes(); len(got) != 1 || got[0] != "" {
		t.Fatalf("recorded refresh scopes = %q, want one empty entry", got)
	}
	// A scope the refresh carries is ignored, not a consent question.
	status, _ = refreshWith(t, s, next["refresh_token"].(string), "never.consented")
	if status != http.StatusOK {
		t.Fatalf("refresh with an arbitrary scope: status %d", status)
	}
}

// The default mode still names the granted scope, so generic mode is the only thing that omits it.
func TestEntraModeStillNamesScope(t *testing.T) {
	s := entrafake.New()
	defer s.Close()
	body := redeemCode(t, s, genericVerifier, "openid", "offline_access", s.ConsentedScopes()[1])
	if got, _ := body["scope"].(string); got == "" {
		t.Fatalf("an Entra response omitted scope: %v", body)
	}
}

// With reuse detection on, presenting a rotated-away token revokes the family: the token that replaced it
// answers invalid_grant too. Without it, only the spent one does.
func TestReuseDetectionRevokesTheFamily(t *testing.T) {
	for _, on := range []bool{false, true} {
		s := entrafake.New()
		s.SetGenericMode(true)
		s.SetReuseDetection(on)
		first := redeemCode(t, s, genericVerifier, "openid", "offline_access")["refresh_token"].(string)
		status, second := refreshWith(t, s, first, "")
		if status != http.StatusOK {
			t.Fatalf("on=%v: first refresh status %d", on, status)
		}
		newest := second["refresh_token"].(string)
		if status, _ := refreshWith(t, s, first, ""); status != http.StatusBadRequest {
			t.Fatalf("on=%v: replaying the spent token = %d, want 400", on, status)
		}
		status, _ = refreshWith(t, s, newest, "")
		if on && status != http.StatusBadRequest {
			t.Fatalf("reuse detection on: the newest token = %d, want it revoked with its family", status)
		}
		if !on && status != http.StatusOK {
			t.Fatalf("reuse detection off: the newest token = %d, want it alive", status)
		}
		s.Close()
	}
}
