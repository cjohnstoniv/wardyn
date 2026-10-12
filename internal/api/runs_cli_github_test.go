// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

const ghCLIRowID = "6f0c1d2e-0000-4000-8000-0000000c05a5"

// githubCLIFixture is the component fixture whose deployment policy grants the
// App lane on acme/app, with an organisation's GitHub CLI row stored.
func githubCLIFixture(t *testing.T) (*componentFixture, map[string]any) {
	t.Helper()
	f := newComponentFixture(t)
	f.srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{{Kind: types.GrantGitHubToken, Scope: json.RawMessage(`{"repos":["acme/app"]}`)}}
	ref := f.org(ghCLIRowID, types.ComponentDefinition{CLI: &types.ComponentCLI{Kind: types.CLIKindGitHub,
		GitHub: &types.CLIGitHub{Capabilities: []ghscope.Capability{ghscope.CapIssuesRead, ghscope.CapCodeRead}}}})
	return f, ref
}

// A run with a GitHub CLI row and a GitHub repository launches with the door:
// the sandbox holds only the inert token, the proxy holds the door's grant
// (written at create with api set) and terminates api.github.com on 443, which
// the git broker's confinement no longer denies.
func TestGitHubCLI_CreateWiresTheDoor(t *testing.T) {
	f, ref := githubCLIFixture(t)
	fr := f.srv.cfg.Runner.(*fakeRunner)
	body := componentBody(ref)
	body["repo"] = "acme/app"
	if w := f.ask(t, componentDoors[0], body); w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	fr.waitForSandbox(t)
	fr.mu.Lock()
	spec := fr.lastSpec
	fr.mu.Unlock()
	pc := spec.ProxyConfig
	if spec.Env["GH_TOKEN"] != githubCLITokenSentinel || spec.Env["GH_PROMPT_DISABLED"] != "1" {
		t.Errorf("GH_TOKEN %q GH_PROMPT_DISABLED %q", spec.Env["GH_TOKEN"], spec.Env["GH_PROMPT_DISABLED"])
	}
	door := pc.GitHubAPI
	if door == nil || !slices.Equal(door.Repos, []string{"acme/app"}) || len(door.Grants) != 1 ||
		!slices.Equal(door.Capabilities, []ghscope.Capability{ghscope.CapCodeRead, ghscope.CapIssuesRead}) {
		t.Fatalf("GitHubAPI = %+v", door)
	}
	var cliGrant *types.CredentialGrant
	for _, g := range f.st.govEscapeStore.grants {
		if g.ID == door.Grants["acme"] {
			cliGrant = &g
		}
	}
	if cliGrant == nil || cliGrant.Spec.Kind != types.GrantGitHubToken ||
		string(cliGrant.Spec.Scope) != `{"api":true,"capabilities":["code_read","issues_read"],"repos":["acme/app"]}` {
		t.Fatalf("the door's grant row = %+v", cliGrant)
	}
	if slices.Contains(slices.Collect(maps.Values(pc.GitGrants)), door.Grants["acme"]) {
		t.Error("the git broker serves the CLI's grant")
	}
	if !slices.Contains(pc.Policy.AllowedDomains, "api.github.com:443") || slices.Contains(pc.Policy.DeniedDomains, "api.github.com") {
		t.Errorf("allowed %v denied %v: the door's host is not reachable", pc.Policy.AllowedDomains, pc.Policy.DeniedDomains)
	}
	if pc.MITMCACertPEM == "" {
		t.Error("no per-run CA: the door cannot terminate api.github.com")
	}
}

// Without a GitHub repository the launch and Review doors refuse with the
// card's reason, and the preview shows the row unavailable for it.
func TestGitHubCLI_NoRepoIsTheCardsReason(t *testing.T) {
	f, ref := githubCLIFixture(t)
	for _, door := range componentDoors[:2] {
		w := f.ask(t, door, componentBody(ref))
		if got := decodeErrorBody(t, w); w.Code != http.StatusUnprocessableEntity || got.Reason != reasonCLINoGitHubRepo {
			t.Errorf("%s = %d %s, want 422 %s", door, w.Code, w.Body, reasonCLINoGitHubRepo)
		}
	}
	w := f.ask(t, componentDoors[2], componentBody(ref))
	if w.Code != http.StatusOK {
		t.Fatalf("preview = %d %s", w.Code, w.Body)
	}
	var got struct {
		Access accessBlock `json:"access"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(got.Access.Rows, func(r client.AccessRow) bool { return r.Kind == accessKindCLI })
	if i < 0 || got.Access.Rows[i].Status != accessUnavailable || got.Access.Rows[i].Reason != reasonCLINoGitHubRepo || len(got.Access.Rows[i].Credentials) != 0 {
		t.Fatalf("access rows %+v, want the CLI row unavailable for %s with no credential", got.Access.Rows, reasonCLINoGitHubRepo)
	}
}
