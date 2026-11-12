// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/url"
	"testing"
)

// RFC 8693 section 2.1: actor_token_type is required with actor_token and must not appear without it,
// and it is one of the subject token types this endpoint accepts. Each malformed pair is a 400
// invalid_request before any token is verified, and nothing is minted.
func TestTokenExchange_ActorTokenNeedsType(t *testing.T) {
	for _, tc := range []struct {
		name string
		form url.Values
	}{
		{"an actor token without its type", gapCovForm(url.Values{"actor_token": {"an-agent-token"}})},
		{"an actor token type without the token", gapCovForm(url.Values{"actor_token_type": {tokenTypeAccessToken}})},
		{"an empty actor token with a type", gapCovForm(url.Values{"actor_token": {""}, "actor_token_type": {tokenTypeAccessToken}})},
		{"an unknown actor token type", gapCovForm(url.Values{"actor_token": {"an-agent-token"}, "actor_token_type": {"urn:example:other"}})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds := gapCovPortalStore()
			h := newHarness(t)
			srv := New(baseTestConfig(h, ds))
			w := gapCovExchange(t, srv, gapCovPortalID.String(), gapCovPortalSecret, true, tc.form)
			if w.Code != http.StatusBadRequest || gapCovOAuthError(t, w) != "invalid_request" {
				t.Fatalf("answer = %d %s, want 400 invalid_request", w.Code, w.Body)
			}
			if ds.mints != 0 {
				t.Errorf("%d token(s) minted for a malformed request", ds.mints)
			}
			if rows := govCovAudits(h, "delegation.exchange"); len(rows) != 0 {
				t.Errorf("a malformed request wrote delegation.exchange rows: %+v", rows)
			}
		})
	}

	// A well-formed actor pair passes the form check: without SSO it reaches the sso_not_configured
	// refusal, not invalid_request.
	for _, typ := range []string{tokenTypeAccessToken, tokenTypeIDToken, tokenTypeJWT} {
		t.Run("a well-formed pair, type "+typ, func(t *testing.T) {
			ds := gapCovPortalStore()
			srv := New(baseTestConfig(newHarness(t), ds))
			w := gapCovExchange(t, srv, gapCovPortalID.String(), gapCovPortalSecret, true,
				gapCovForm(url.Values{"actor_token": {"an-agent-token"}, "actor_token_type": {typ}}))
			if w.Code != http.StatusBadRequest || gapCovOAuthError(t, w) != "invalid_grant" {
				t.Fatalf("answer = %d %s, want 400 invalid_grant (sso_not_configured)", w.Code, w.Body)
			}
		})
	}
}
