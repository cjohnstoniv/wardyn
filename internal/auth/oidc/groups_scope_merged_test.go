// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// groups_scope_merged_test.go pins the half of the groups-scope warning that
// boot cannot reach.
//
// The boot warning (groups_scope_test.go) reads Config.RoleMap — the chart's
// WARDYN_OIDC_ROLE_MAP — and nothing else. The map a login derives from is that
// one MERGED with the console's Getting Started -> People rows, which are
// written and deleted through the API while the process runs. A deployment that
// manages every group->role row from the console therefore booted completely
// SILENT on the exact condition the warning exists for, and a boot-time store
// read could not have fixed it either: the row an admin adds at 10am was not
// there at 9am. So the merged map is asked at login, once per process.
package oidc_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc/oidctest"
	jose "github.com/go-jose/go-jose/v4"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// mergedScopeWarning is the opening of the login-time line. The boot line has
// its own wording, so a test asserting one can never be satisfied by the other.
const mergedScopeWarning = "no login on this deployment has carried a `roles` or `groups` claim"

// scopeLoginIdP is a COMPLETE fake IdP — discovery with a caller-chosen
// scopes_supported, a JWKS, and a token endpoint — because this pin needs a
// real /auth/callback to run, and neither oidctest.Server (no scopes_supported)
// nor newScopeIdP (discovery only) can drive one.
type scopeLoginIdP struct {
	srv     *httptest.Server
	priv    *rsa.PrivateKey
	idToken string
}

func newScopeLoginIdP(t *testing.T, scopesSupported []string) *scopeLoginIdP {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	jwk, err := json.Marshal(jose.JSONWebKey{Key: priv.Public(), KeyID: "good", Algorithm: "RS256", Use: "sig"})
	if err != nil {
		t.Fatalf("marshal jwk: %v", err)
	}
	idp := &scopeLoginIdP{priv: priv}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		doc := map[string]any{
			"issuer":                                idp.srv.URL,
			"authorization_endpoint":                idp.srv.URL + "/auth",
			"token_endpoint":                        idp.srv.URL + "/token",
			"jwks_uri":                              idp.srv.URL + "/keys",
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
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"keys":[%s]}`, jwk)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": idp.idToken,
		})
	})
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	return idp
}

// sign stages the id_token the next /token exchange returns.
func (i *scopeLoginIdP) sign(t *testing.T, sub string, groups []string) {
	t.Helper()
	claims := map[string]any{
		"iss": i.srv.URL, "sub": sub, "aud": "wardyn-client",
		"email": sub + "@corp.example", "email_verified": true, "nonce": roleCallbackNonce,
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	}
	if groups != nil {
		claims["groups"] = groups
	}
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	i.idToken = oidctest.SignIDToken(i.priv, "good", "RS256", string(raw))
}

// newMergedScopeAuth builds an Authenticator against idp and returns it plus
// the live log buffer, so the test can read what BOOT said and what the LOGIN
// said separately.
func newMergedScopeAuth(t *testing.T, idp *scopeLoginIdP, chart map[string]string, rows []writoidc.RoleMapping) (*writoidc.Authenticator, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	auth, err := writoidc.New(context.Background(), writoidc.Config{
		IssuerURL:    idp.srv.URL,
		ClientID:     "wardyn-client",
		ClientSecret: "secret",
		RedirectURL:  "http://localhost/auth/callback",
		RoleMap:      chart,
		DefaultRole:  writoidc.RoleMember,
		RoleMappings: &fakeRoleMappingSource{rows: rows},
	}, testHMACKey)
	if err != nil {
		t.Fatalf("writoidc.New: %v", err)
	}
	return auth, buf
}

// TestLoginWarnsWhenTheMERGEDMapNeedsTheGroupsScope is the residue pin: the
// console-managed row that boot cannot see.
//
// Counterfactual: delete the warnMergedMapNeedsGroupsScope call from
// CallbackHandler and the first row goes red — a deployment whose every
// group->role row is console-managed gets no signal at boot AND none at login,
// which is the state the finding was reopened on.
func TestLoginWarnsWhenTheMERGEDMapNeedsTheGroupsScope(t *testing.T) {
	withGroups := []string{"openid", "profile", "email", "groups"}
	entraShape := []string{"openid", "profile", "email", "offline_access"}
	groupRow := []writoidc.RoleMapping{{Value: "eng-team", Role: writoidc.RoleMember}}

	cases := []struct {
		name            string
		scopesSupported []string
		chart           map[string]string
		rows            []writoidc.RoleMapping
		claimGroups     []string
		wantWarn        bool
	}{
		{
			// THE RESIDUE. Chart map EMPTY, every group row console-managed:
			// boot is silent by construction, so login is the only place left.
			name:            "console-managed group row only, claim never arrives",
			scopesSupported: withGroups,
			rows:            groupRow,
			wantWarn:        true,
		},
		{
			// The IdP does send the claim to this client, so the scope is not
			// gating anything and there is nothing to say.
			name:            "console-managed group row, and the claim arrives",
			scopesSupported: withGroups,
			rows:            groupRow,
			claimGroups:     []string{"eng-team"},
			wantWarn:        false,
		},
		{
			// An email row is answered by the `email` claim this request DOES
			// ask for.
			name:            "console-managed rows keyed only on emails",
			scopesSupported: withGroups,
			rows:            []writoidc.RoleMapping{{Value: "alice@corp.example", Role: writoidc.RoleAdmin}},
			wantWarn:        false,
		},
		{
			// THE ENTRA PATH: no `groups` scope advertised, claim emitted
			// without one. Neither half of the warning may ever fire here.
			name:            "provider advertises no groups scope (the Entra shape)",
			scopesSupported: entraShape,
			rows:            groupRow,
			wantWarn:        false,
		},
		{
			name:            "no role map anywhere",
			scopesSupported: withGroups,
			wantWarn:        false,
		},
		{
			// A provider publishing no scopes_supported is not evidence that
			// it gates anything.
			name:            "discovery document omits scopes_supported",
			scopesSupported: nil,
			rows:            groupRow,
			wantWarn:        false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idp := newScopeLoginIdP(t, tc.scopesSupported)
			auth, logs := newMergedScopeAuth(t, idp, tc.chart, tc.rows)

			// The finding's own evidence: with no CHART map there is nothing
			// for the boot half to see, whatever the console holds.
			if len(tc.chart) == 0 && strings.Contains(logs.String(), "advertises a `groups` scope that Wardyn does not request") {
				t.Fatalf("boot warned with an EMPTY chart map — the boot half must stay keyed on Config.RoleMap\nlogs:\n%s", logs.String())
			}
			bootLogs := logs.Len()

			idp.sign(t, "sub-scope", tc.claimGroups)
			w, sess := doCallback(t, auth)
			if sess.Sub != "sub-scope" {
				t.Fatalf("login failed (status %d, %q) — the warning must never deny a login",
					w.Code, w.Result().Header.Get("Location"))
			}

			loginLogs := logs.String()[bootLogs:]
			if got := strings.Contains(loginLogs, mergedScopeWarning); got != tc.wantWarn {
				t.Fatalf("login warned = %v, want %v\nlogin logs:\n%s", got, tc.wantWarn, loginLogs)
			}
			if !tc.wantWarn {
				return
			}
			// Actionable on its own: an operator reading it in an aggregator
			// has neither this test nor the source.
			for _, want := range []string{"WARDYN_OIDC_ROLE_MAP", "eng-team", idp.srv.URL, "console_store=true"} {
				if !strings.Contains(loginLogs, want) {
					t.Errorf("login warning does not name %q — an operator cannot act on it\nlogin logs:\n%s", want, loginLogs)
				}
			}
		})
	}
}

// TestMergedGroupsScopeWarningIsOncePerProcess pins the two properties that
// keep the line from becoming the one every operator filters out: it is said
// once, and a login that DOES carry the claim settles the question for good.
func TestMergedGroupsScopeWarningIsOncePerProcess(t *testing.T) {
	withGroups := []string{"openid", "profile", "email", "groups"}
	groupRow := []writoidc.RoleMapping{{Value: "eng-team", Role: writoidc.RoleMember}}

	t.Run("three groupless logins say it once", func(t *testing.T) {
		idp := newScopeLoginIdP(t, withGroups)
		auth, logs := newMergedScopeAuth(t, idp, nil, groupRow)
		for i := range 3 {
			idp.sign(t, fmt.Sprintf("sub-%d", i), nil)
			doCallback(t, auth)
		}
		if n := strings.Count(logs.String(), mergedScopeWarning); n != 1 {
			t.Fatalf("warned %d times across three logins, want exactly 1 — a per-login warning about a "+
				"deployment-shaped condition is how a warning gets filtered out permanently", n)
		}
	})

	t.Run("a login carrying the claim silences it for good", func(t *testing.T) {
		idp := newScopeLoginIdP(t, withGroups)
		auth, logs := newMergedScopeAuth(t, idp, nil, groupRow)
		// First: proof the IdP sends the claim to this client.
		idp.sign(t, "sub-in-groups", []string{"eng-team"})
		doCallback(t, auth)
		// Then a human genuinely in no groups. The question is already answered.
		idp.sign(t, "sub-groupless", nil)
		doCallback(t, auth)
		if strings.Contains(logs.String(), mergedScopeWarning) {
			t.Fatalf("warned after a login had already carried the claim — that login is proof the scope is not "+
				"gating anything, so the later groupless human is simply in no groups\nlogs:\n%s", logs.String())
		}
	})
}
