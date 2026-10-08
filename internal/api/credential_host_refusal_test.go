// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// One credential per host, at the three run doors, for every run.

func apiKeyOn(host string) types.GrantSpec {
	return types.GrantSpec{Kind: types.GrantAPIKey, TTLSeconds: 3600,
		Scope: json.RawMessage(`{"host":"` + host + `","secret_name":"` + govCorpSecret + `"}`)}
}

// twoCredentialsOnOneHost gives the deployment's default policy — the policy a
// member's run starts from — two credentials for corp.example.
func twoCredentialsOnOneHost(f *componentFixture) {
	f.srv.cfg.DefaultPolicy.AllowedDomains = append(f.srv.cfg.DefaultPolicy.AllowedDomains, "corp.example")
	f.srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{apiKeyOn("corp.example"), apiKeyOn("Corp.Example.")}
}

// tokenRedirectOnAPolicyCredential gives it one credential for corp.example
// and the deployment a redirect that puts its own token on that host.
func tokenRedirectOnAPolicyCredential(f *componentFixture) {
	f.srv.cfg.DefaultPolicy.AllowedDomains = append(f.srv.cfg.DefaultPolicy.AllowedDomains, "corp.example")
	f.srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{apiKeyOn("corp.example")}
	f.st.siteConfig.EgressRedirects = []types.EgressRedirect{
		{From: "https://registry.npmjs.org/", To: "https://corp.example:8443/npm", Ecosystem: "npm", TokenSecretRef: govCorpSecret}}
}

// A run whose policy and deployment bind two credentials to one host is
// refused with the same status and bytes at launch, at Review and at the
// policy preview — whether or not the run carries components — and the
// sentence names no host: either credential may be an admin's.
func TestRunDoors_TwoCredentialsForOneHostAreRefusedAtEveryDoor(t *testing.T) {
	own := inlineComponent([]string{"svc.example"}, headerSecret(compOwnSecret, "svc.example"))
	for name, arrange := range map[string]func(*componentFixture){
		"two api_key grants for one host":                twoCredentialsOnOneHost,
		"a policy credential on a token redirect's host": tokenRedirectOnAPolicyCredential,
	} {
		for with, refs := range map[string][]any{"without components": nil, "with a component": {own}} {
			t.Run(name+"/"+with, func(t *testing.T) {
				f := newComponentFixture(t)
				arrange(f)
				body := componentBody(refs...)
				var first string
				for i, door := range componentDoors {
					w := f.ask(t, door, body)
					got := decodeErrorBody(t, w)
					if w.Code != http.StatusUnprocessableEntity || got.Reason != reasonCredentialHostCollision {
						t.Fatalf("%s = %d %s, want 422 %s", door, w.Code, w.Body.String(), reasonCredentialHostCollision)
					}
					if strings.Contains(strings.ToLower(w.Body.String()), "corp.example") || strings.Contains(w.Body.String(), govCorpSecret) {
						t.Errorf("%s names the host or the secret: %s", door, w.Body.String())
					}
					if i == 0 {
						first = w.Body.String()
					} else if w.Body.String() != first {
						t.Errorf("%s answered %s, launch answered %s", door, w.Body.String(), first)
					}
				}
			})
		}
	}

	// What is not a collision is not refused as one: a single credential, a
	// redirect with no token on the same host, a token redirect on another
	// host, and one mirror behind two ecosystems.
	for name, arrange := range map[string]func(*componentFixture){
		"one credential": func(f *componentFixture) {
			f.srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{apiKeyOn("corp.example")}
		},
		"a redirect with no token on the credential's host": func(f *componentFixture) {
			f.srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{apiKeyOn("corp.example")}
			f.st.siteConfig.EgressRedirects = []types.EgressRedirect{{From: "https://registry.npmjs.org/", To: "https://corp.example/npm", Ecosystem: "npm"}}
		},
		"one mirror behind two ecosystems, and a credential elsewhere": func(f *componentFixture) {
			f.srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{apiKeyOn("corp.example")}
			f.st.siteConfig.EgressRedirects = []types.EgressRedirect{
				{From: "https://registry.npmjs.org/", To: "https://mirror.corp.example/npm", Ecosystem: "npm", TokenSecretRef: govCorpSecret},
				{From: "https://pypi.org/", To: "https://mirror.corp.example:8443/pypi", Ecosystem: "pip", TokenSecretRef: govCorpSecret}}
		},
	} {
		t.Run("not a collision/"+name, func(t *testing.T) {
			f := newComponentFixture(t)
			f.srv.cfg.DefaultPolicy.AllowedDomains = append(f.srv.cfg.DefaultPolicy.AllowedDomains, "corp.example")
			arrange(f)
			for _, door := range componentDoors {
				w := f.ask(t, door, componentBody())
				if w.Code >= 300 && decodeErrorBody(t, w).Reason == reasonCredentialHostCollision {
					t.Errorf("%s = %d %s, want no credential collision", door, w.Code, w.Body.String())
				}
			}
		})
	}
}
