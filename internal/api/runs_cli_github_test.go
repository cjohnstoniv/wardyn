// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

const ghCLIRowID = "6f0c1d2e-0000-4000-8000-0000000c05a5"

// githubCLIFixture is the component fixture whose deployment policy grants the
// App lane on acme/app, with an organisation's GitHub CLI row stored.
func githubCLIFixture(t *testing.T) (*componentFixture, map[string]any) {
	t.Helper()
	return githubCLIFixtureScope(t, `{"repos":["acme/app"]}`)
}

func githubCLIFixtureScope(t *testing.T, scope string) (*componentFixture, map[string]any) {
	t.Helper()
	f := newComponentFixture(t)
	f.srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{{Kind: types.GrantGitHubToken, Scope: json.RawMessage(scope)}}
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

// The GitHub CLI's door credentials api.github.com, so a run that binds another credential to that
// host is refused at every door: a header component after the CLI row or before it, and a policy's
// own credential there. Two GitHub CLI rows share the one door and are admitted.
func TestGitHubCLI_DoorHostCarriesOneCredential(t *testing.T) {
	header := inlineComponent([]string{"api.github.com"}, headerSecret(compOwnSecret, "api.github.com"))
	for _, tc := range []struct {
		name string
		body func(f *componentFixture, cli map[string]any) map[string]any
	}{
		{"a header component after the CLI row", func(_ *componentFixture, cli map[string]any) map[string]any {
			return componentBody(cli, header)
		}},
		{"a header component before the CLI row", func(_ *componentFixture, cli map[string]any) map[string]any {
			return componentBody(header, cli)
		}},
		{"a policy credential on the door's host", func(f *componentFixture, cli map[string]any) map[string]any {
			f.srv.cfg.DefaultPolicy.EligibleGrants = append(f.srv.cfg.DefaultPolicy.EligibleGrants, types.GrantSpec{Kind: types.GrantAPIKey, TTLSeconds: 3600,
				Scope: json.RawMessage(`{"host":"api.github.com","secret_name":"` + govCorpSecret + `"}`)})
			return componentBody(cli)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, cli := githubCLIFixture(t)
			body := tc.body(f, cli)
			body["repo"] = "acme/app"
			for _, door := range componentDoors {
				w := f.ask(t, door, body)
				if got := decodeErrorBody(t, w); w.Code != http.StatusUnprocessableEntity || got.Reason != reasonComponentHostCollision {
					t.Errorf("%s = %d %s, want 422 %s", door, w.Code, w.Body, reasonComponentHostCollision)
				}
			}
		})
	}
	f, cli := githubCLIFixture(t)
	second := f.org("6f0c1d2e-0000-4000-8000-0000000c05a6", types.ComponentDefinition{CLI: &types.ComponentCLI{Kind: types.CLIKindGitHub,
		GitHub: &types.CLIGitHub{Capabilities: []ghscope.Capability{ghscope.CapIssuesRead}}}})
	body := componentBody(cli, second)
	body["repo"] = "acme/app"
	if w := f.ask(t, componentDoors[1], body); w.Code >= 300 {
		t.Fatalf("two GitHub CLI rows at %s = %d %s, want admitted", componentDoors[1], w.Code, w.Body)
	}
}

// Without a GitHub repository the launch and Review doors refuse with the
// card's reason, and the preview shows the row unavailable for it.
func TestGitHubCLI_NoRepoIsTheCardsReason(t *testing.T) {
	f, ref := githubCLIFixtureScope(t, `{"repos":[]}`)
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

// The repositories the card and the Review gate read (githubCLIRepos) are the
// door's: the git broker's, which create derives from the same grants.
func TestGitHubCLI_CardReposAreTheDoors(t *testing.T) {
	for _, tc := range []struct {
		name, scope, repo string
		want              []string
	}{
		{"template grant, the run's repository", `{"repos":[]}`, "Acme/App", []string{"acme/app"}},
		{"the grant's own repositories", `{"repos":["acme/app","acme/lib"]}`, "", []string{"acme/app", "acme/lib"}},
		{"both", `{"repos":["acme/lib"]}`, "acme/app", []string{"acme/app", "acme/lib"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, ref := githubCLIFixtureScope(t, tc.scope)
			fr := f.srv.cfg.Runner.(*fakeRunner)
			body := componentBody(ref)
			if tc.repo != "" {
				body["repo"] = tc.repo
			}
			if w := f.ask(t, componentDoors[0], body); w.Code != http.StatusCreated {
				t.Fatalf("create = %d %s", w.Code, w.Body)
			}
			fr.waitForSandbox(t)
			fr.mu.Lock()
			door := fr.lastSpec.ProxyConfig.GitHubAPI
			fr.mu.Unlock()
			card := githubCLIRepos(createRunRequest{Agent: "claude-code", Repo: tc.repo}, f.srv.cfg.DefaultPolicy, types.SiteConfig{})
			if door == nil || !slices.Equal(door.Repos, tc.want) || !slices.Equal(card, tc.want) {
				t.Fatalf("door %+v card %v, want both %v", door, card, tc.want)
			}
		})
	}
}

// A built-in GitHub component's grant counts only its own repositories: the run's
// declared repository is not mapped onto it, at the door or on the card.
func TestGitHubCLI_CardSkipsDeclaredReposForABuiltinGrant(t *testing.T) {
	builtin := types.GrantSpec{Kind: types.GrantGitHubToken,
		Scope: json.RawMessage(`{"builtin":"` + types.ComponentBuiltinGitHub + `","repos":["acme/lib"]}`)}
	spec := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{builtin}}
	got := githubCLIRepos(createRunRequest{Agent: "claude-code", Repo: "acme/app"}, spec, types.SiteConfig{})
	if !slices.Equal(got, []string{"acme/lib"}) {
		t.Fatalf("card repos = %v, want only the built-in's [acme/lib]", got)
	}
}

// The preview's CLI row names the host the door opens and terminates, and the
// App credential as proxy only.
func TestGitHubCLI_RowListsTheDoorsHost(t *testing.T) {
	f, ref := githubCLIFixture(t)
	body := componentBody(ref)
	body["repo"] = "acme/app"
	w := f.ask(t, componentDoors[2], body)
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
	if i < 0 || !slices.Contains(got.Access.Rows[i].Hosts, "api.github.com") || len(got.Access.Rows[i].Credentials) != 1 {
		t.Fatalf("CLI row = %+v", got.Access.Rows)
	}
	h := slices.IndexFunc(got.Access.Hosts, func(h client.AccessHost) bool { return h.Host == "api.github.com" })
	if h < 0 || got.Access.Hosts[h].Via != viaInjected || got.Access.Hosts[h].Verdict != verdictAllowed {
		t.Fatalf("hosts = %+v, want api.github.com injected and allowed", got.Access.Hosts)
	}
}

// The door lifts only the git broker's own deny of api.github.com; a deny the
// run already carried stays, and the door's host stays unreachable.
func TestGitHubCLI_ConfineKeepsTheRunsOwnDeny(t *testing.T) {
	door := &proxy.GitHubAPIGrant{Repos: []string{"acme/app"}, Grants: map[string]uuid.UUID{"acme": uuid.New()}}
	p := dispatchParams{GitGrants: map[string]uuid.UUID{"acme/app": uuid.New()}, Components: componentDispatch{GitHubAPI: door}}
	for _, tc := range []struct {
		name     string
		denied   []string
		wantDeny bool
	}{
		{"broker's deny lifted", nil, false},
		{"run's own deny kept", []string{"api.github.com"}, true},
	} {
		policy := types.RunPolicySpec{DeniedDomains: tc.denied}
		env := map[string]string{}
		confineBrokerEgress(&policy, p, env)
		denied := slices.ContainsFunc(policy.DeniedDomains, func(d string) bool { return ceilingDenies([]string{d}, "api.github.com") })
		if denied != tc.wantDeny || !slices.Contains(policy.AllowedDomains, "api.github.com:443") || env["GH_TOKEN"] != githubCLITokenSentinel {
			t.Errorf("%s: denied %v allowed %v env %v", tc.name, policy.DeniedDomains, policy.AllowedDomains, env)
		}
	}
}
