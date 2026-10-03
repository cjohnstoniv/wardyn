// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package entrafake_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/test/entrafake"
)

const (
	foundryResource = "https://ai.azure.com"
	foundryScope    = foundryResource + "/user_impersonation"
	adoResourceID   = "499b84ac-1321-427f-aa17-267ca6975798"
)

// TestDefaultRequestIsExpandedNeverEchoed: a `<resource>/.default` request is
// consented iff the fixture holds a scope under that resource, and the answer
// lists those scopes, never the `.default` literal — on the code leg and on a
// refresh. Echoing the literal is what let a `.default`-literal implementation
// pass every test and fail against a real tenant.
func TestDefaultRequestIsExpandedNeverEchoed(t *testing.T) {
	s := entrafake.New()
	defer s.Close()
	s.SetConsentedScopes(foundryScope, adoResourceID+"/vso.code_write")

	first := redeemCode(t, s, "verifier-gggggggggggggggggggggggggggggggggggggggg",
		foundryResource+"/.default", "offline_access", "openid")
	granted, _ := first["scope"].(string)
	if strings.Contains(granted, ".default") {
		t.Fatalf("the answer echoes the `.default` literal: %q", granted)
	}
	for _, want := range []string{foundryScope, "offline_access", "openid"} {
		if !strings.Contains(granted, want) {
			t.Fatalf("granted scope %q does not carry %q", granted, want)
		}
	}
	if strings.Contains(granted, adoResourceID) {
		t.Fatalf("the grant carries a scope of a resource it was not asked for: %q", granted)
	}

	refresh, _ := first["refresh_token"].(string)
	status, body := postToken(t, s, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"scope":         {foundryResource + "/.default"},
	})
	if status != http.StatusOK {
		t.Fatalf("refresh with `.default`: status %d body %v", status, body)
	}
	if got, _ := body["scope"].(string); got != foundryScope {
		t.Fatalf("refresh answered %q; want exactly %q", got, foundryScope)
	}

	t.Run("a resource the fixture holds nothing under is refused on both legs", func(t *testing.T) {
		q := authorize(t, s, "st-5", "verifier-hhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhh",
			"https://cognitiveservices.azure.com/.default", "openid")
		if q.Get("error") != entrafake.ErrConsentRequired {
			t.Fatalf("authorize error = %q, want %q", q.Get("error"), entrafake.ErrConsentRequired)
		}
		rotated, _ := body["refresh_token"].(string)
		status, refused := postToken(t, s, url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {rotated},
			"scope":         {"https://cognitiveservices.azure.com/.default"},
		})
		if status == http.StatusOK || refused["error"] != entrafake.ErrConsentRequired {
			t.Fatalf("refresh: status %d body %v; want consent_required", status, refused)
		}
	})

	t.Run("a fixture holding only the .default literal holds nothing under the resource", func(t *testing.T) {
		s.SetConsentedScopes(foundryResource + "/.default")
		q := authorize(t, s, "st-6", "verifier-iiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiii", foundryResource+"/.default")
		if q.Get("error") != entrafake.ErrConsentRequired {
			t.Fatalf("authorize error = %q, want %q", q.Get("error"), entrafake.ErrConsentRequired)
		}
	})
}

// TestScopeNamingTwoResourcesIsRefused: one token is for one resource, so a
// scope string naming two is AADSTS700022 on both legs. Bare OIDC scopes name no
// resource and never trigger it.
func TestScopeNamingTwoResourcesIsRefused(t *testing.T) {
	s := entrafake.New()
	defer s.Close()
	s.SetConsentedScopes(foundryScope, adoResourceID+"/vso.code_write")

	q := authorize(t, s, "st-7", "verifier-jjjjjjjjjjjjjjjjjjjjjjjjjjjjjjjjjjjjjjjj",
		foundryResource+"/.default", adoResourceID+"/vso.code_write", "offline_access")
	if q.Get("error") != entrafake.ErrInvalidRequest || !strings.Contains(q.Get("error_description"), entrafake.AADSTSMultipleResources) {
		t.Fatalf("authorize: error %q description %q; want %s", q.Get("error"), q.Get("error_description"), entrafake.AADSTSMultipleResources)
	}

	first := redeemCode(t, s, "verifier-kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk",
		"openid", "offline_access", foundryScope)
	refresh, _ := first["refresh_token"].(string)
	status, body := postToken(t, s, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"scope":         {foundryResource + "/.default " + adoResourceID + "/vso.code_write"},
	})
	if status == http.StatusOK || body["error"] != entrafake.ErrInvalidRequest {
		t.Fatalf("refresh: status %d body %v; want invalid_request", status, body)
	}
	if desc, _ := body["error_description"].(string); !strings.Contains(desc, entrafake.AADSTSMultipleResources) {
		t.Fatalf("refresh error_description %q does not name %s", desc, entrafake.AADSTSMultipleResources)
	}
}
