// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

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

// feedToken is the secret the feed redirect's token is stored under.
const feedToken = "feed-token"

// feedRedirect is a token-bearing redirect whose target is a package feed on
// an Azure DevOps host the per-person lane also credentials.
func feedRedirect() types.EgressRedirect {
	return types.EgressRedirect{From: "https://registry.npmjs.org/", Ecosystem: "npm", TokenSecretRef: feedToken,
		To: "https://pkgs.dev.azure.com/contoso/_packaging/feed/npm/registry/"}
}

// feedSite is a deployment with the per-person Azure DevOps lane and that redirect.
func feedSite() types.SiteConfig {
	sc := adoSite(adoEntraTestRow())
	sc.EgressRedirects = []types.EgressRedirect{feedRedirect()}
	return sc
}

// On a run the per-person Azure DevOps lane resolves for, a token-bearing
// redirect whose target is a lane host is not a second credential: the lane
// carries that host, and the redirect is applied without its token. Launch,
// Review and the policy preview all admit the run.
func TestRunDoors_ALaneRunWithATokenRedirectOntoALaneHostIsAdmitted(t *testing.T) {
	body := `{"agent":"claude-code","task":"t","confinement_class":"CC2","inline_policy":{"min_confinement_class":"CC2",` +
		`"allowed_domains":["api.anthropic.com"],"workspace_repos":[{"repo":"` + adoTestRepo + `"}]}}`
	for door, want := range map[string]int{
		"/api/v1/runs": http.StatusCreated, "/api/v1/runs/preflight": http.StatusOK, "/api/v1/runs/policy-preview": http.StatusOK,
	} {
		t.Run(door, func(t *testing.T) {
			srv, st, _ := govEscapeFixture(t, &capStore{})
			st.workspaces = []types.Workspace{{ID: uuid.New(), Name: "ado",
				Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: adoTestRepo}}}}
			st.siteConfig = feedSite()
			// The session's subject is the lane's owner, as at dispatch.
			w := doSSO(t, srv, http.MethodPost, door, govSession(t, adoTestOwner, []string{"eng"}, false), body)
			if w.Code != want {
				t.Fatalf("%s = %d %s, want %d: the lane carries the feed host's credential", door, w.Code, w.Body.String(), want)
			}
		})
	}
}
