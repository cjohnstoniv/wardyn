// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// groups_scope_test.go pins what Wardyn's authorization request asks for, and
// the one thing it says when that is not enough.
//
// The request is fixed at "openid profile email" and does NOT ask for `groups`.
// That is deliberate — Entra defines no such scope and rejects unrecognised
// ones, and an IdP that merely ADVERTISES the scope may not have granted it to
// this client, where asking is invalid_scope, i.e. every human locked out on an
// upgrade nobody opted into. But the cost is silent in the dangerous direction:
// a scope-gated IdP omits the claim, and an omitted `groups` is byte-for-byte
// "asked, and there were none" — sessionGroups reports a COMPLETE empty
// snapshot and deriveRole reads "checked, and nothing matched". There is no
// `_claim_names` marker to fail closed on, because the IdP is not saying
// anything at all.
//
// So boot is where it gets said, and these are the two halves: the request
// itself must not drift, and the warning must fire on exactly the deployments
// it is about — never on the Entra path, whose scopes_supported has no `groups`.
package oidc_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// newScopeIdP serves a discovery document with a caller-chosen
// `scopes_supported`. oidctest.Server cannot: its discovery struct has no such
// field, which is exactly why nothing pinned this before.
func newScopeIdP(t *testing.T, scopesSupported []string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		doc := map[string]any{
			"issuer":                                srv.URL,
			"authorization_endpoint":                srv.URL + "/auth",
			"token_endpoint":                        srv.URL + "/token",
			"jwks_uri":                              srv.URL + "/keys",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		}
		if scopesSupported != nil {
			doc["scopes_supported"] = scopesSupported
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	})
	return srv
}

// newScopeAuth builds an Authenticator against issuer and returns everything
// the default logger emitted while it was constructed.
func newScopeAuth(t *testing.T, issuer string, roleMap map[string]string) (*writoidc.Authenticator, string) {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	auth, err := writoidc.New(context.Background(), writoidc.Config{
		IssuerURL:   issuer,
		ClientID:    "wardyn-client",
		RedirectURL: "http://wardyn.example/auth/callback",
		RoleMap:     roleMap,
	}, testHMACKey)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return auth, buf.String()
}

// TestAuthorizationRequestScopesAreFixed is the behaviour half, and it must stay
// green through any change to the warning: a boot LOG line may not become a
// silent change to what every login asks the IdP for. `groups` in particular is
// the one an Entra tenant rejects outright.
func TestAuthorizationRequestScopesAreFixed(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)

	w := httptest.NewRecorder()
	auth.LoginHandler(w, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	loc, err := url.Parse(w.Result().Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse authorization redirect: %v", err)
	}
	if got, want := loc.Query().Get("scope"), "openid profile email"; got != want {
		t.Fatalf("authorization request scope = %q, want %q — Entra rejects an unrecognised scope, so widening this "+
			"breaks the documented Entra path for every deployment at once; if the change is intended, it belongs "+
			"behind an operator knob, not in the constant", got, want)
	}
	if got := loc.Query().Get("response_type"); got != "code" {
		t.Fatalf("response_type = %q, want \"code\"", got)
	}
}

// TestBootWarnsWhenAGroupsScopeIsAdvertisedButNotRequested is the warning half.
//
// Counterfactual: delete the warnUnrequestedGroupsScope call from New and the
// first row goes red — a deployment whose role map is keyed entirely on group
// names, against a provider that gates the claim behind a scope, boots clean
// and every one of those rows quietly decides nothing.
func TestBootWarnsWhenAGroupsScopeIsAdvertisedButNotRequested(t *testing.T) {
	cases := []struct {
		name            string
		scopesSupported []string
		roleMap         map[string]string
		wantWarn        bool
	}{
		{
			name:            "advertised, and the map is keyed on a group name",
			scopesSupported: []string{"openid", "profile", "email", "groups"},
			roleMap:         map[string]string{"eng-team": writoidc.RoleMember},
			wantWarn:        true,
		},
		{
			// An email key is answered by the `email` claim this request DOES
			// ask for, so nothing here depends on the missing scope.
			name:            "advertised, but the map is keyed only on emails",
			scopesSupported: []string{"openid", "profile", "email", "groups"},
			roleMap:         map[string]string{"alice@corp.example": writoidc.RoleAdmin},
			wantWarn:        false,
		},
		{
			// THE ENTRA PATH, and the row that keeps this from being noise:
			// Entra advertises no `groups` scope and emits the claim without
			// one, so an App-Role/group-keyed map there is correct as written.
			name:            "not advertised (the Entra shape), map keyed on a claim",
			scopesSupported: []string{"openid", "profile", "email", "offline_access"},
			roleMap:         map[string]string{"wardyn.contractors": writoidc.RoleMember},
			wantWarn:        false,
		},
		{
			name:            "advertised, no role map at all",
			scopesSupported: []string{"openid", "profile", "email", "groups"},
			roleMap:         nil,
			wantWarn:        false,
		},
		{
			// A provider that publishes no scopes_supported is not evidence
			// that it gates anything; guessing would cry wolf on every boot.
			name:            "discovery document omits scopes_supported",
			scopesSupported: nil,
			roleMap:         map[string]string{"eng-team": writoidc.RoleMember},
			wantWarn:        false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newScopeIdP(t, tc.scopesSupported)
			_, logs := newScopeAuth(t, srv.URL, tc.roleMap)
			got := strings.Contains(logs, "advertises a `groups` scope that Wardyn does not request")
			if got != tc.wantWarn {
				t.Fatalf("boot warned = %v, want %v\nlogs:\n%s", got, tc.wantWarn, logs)
			}
			if !tc.wantWarn {
				return
			}
			// The line has to be actionable on its own: an operator reading it
			// in a log aggregator has neither this test nor the source.
			for _, want := range []string{"WARDYN_OIDC_ROLE_MAP", "eng-team", srv.URL} {
				if !strings.Contains(logs, want) {
					t.Errorf("boot warning does not name %q — an operator cannot act on it\nlogs:\n%s", want, logs)
				}
			}
		})
	}
}
