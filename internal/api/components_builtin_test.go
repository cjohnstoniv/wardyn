// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// githubBuiltin is a built-in GitHub component reference for acme.
func githubBuiltin(access string, repos ...string) map[string]any {
	ref := map[string]any{"builtin": "github", "org": "acme", "repos": repos}
	if access != "" {
		ref["access"] = access
	}
	return ref
}

// githubCeiling gives the fixture's deployment ceiling a github_token grant
// that allows contents at level, with an empty repository list (the shipped
// default's: a policy may name no repository).
func githubCeiling(f *componentFixture, level string, approval bool, ttl int) {
	f.srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{{Kind: types.GrantGitHubToken, RequiresApproval: approval, TTLSeconds: ttl,
		Scope: json.RawMessage(`{"repos":[],"permissions":{"contents":"` + level + `"}}`)}}
}

// githubRows makes the deployment's provider policy one GitHub row admitting
// acme with lanes.
func githubRows(f *componentFixture, lanes ...types.GitLane) {
	f.st.siteConfig = adoSite(types.GitProvider{ID: "gh", Kind: types.GitProviderGitHub,
		BaseURLs: []string{"https://github.com/acme"}, Lanes: lanes})
}

// A built-in GitHub component reaches its repositories through the git broker
// and nothing else: it becomes one github_token grant held to the ceiling, the
// broker serves exactly its repositories, dispatch denies GitHub's own hosts,
// and Review and the preview show it.
func TestRunComponents_BuiltinGitHubIsBrokered(t *testing.T) {
	f := newComponentFixture(t)
	githubCeiling(f, "write", true, 1800)
	githubRows(f, types.GitLaneApp)
	body := componentBody(githubBuiltin("", "acme/api", "Acme/web"))

	wantFact := componentFact{Kind: types.ComponentGitProvider, Provider: "github", ID: "inline:0", Name: "GitHub: acme",
		Reason: "org", Status: componentUnknown, Requirements: []SetupItem{}, Lane: "app", Org: "acme",
		Repos: []string{"https://github.com/acme/api", "https://github.com/Acme/web"}}
	for _, door := range componentDoors[1:] {
		w := f.ask(t, door, body)
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d %s, want 200", door, w.Code, w.Body.String())
		}
		var got struct {
			Components []componentFact `json:"components"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(got.Components, func(c componentFact) bool { return c.ID == "inline:0" })
		if i < 0 {
			t.Fatalf("%s components = %+v, want the built-in's row", door, got.Components)
		}
		fact := got.Components[i]
		if len(fact.RepoAccess) != 2 || fact.RepoAccess[0].Access != "read" || !fact.RepoAccess[1].CanWrite {
			t.Errorf("%s repo_access = %+v, want read with write offered", door, fact.RepoAccess)
		}
		fact.RepoAccess = nil
		if a, b := mustJSON(fact), mustJSON(wantFact); string(a) != string(b) {
			t.Errorf("%s fact =\n %s\nwant\n %s", door, a, b)
		}
	}

	w := f.ask(t, componentDoors[0], body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", w.Code, w.Body.String())
	}
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	// The ceiling's approval and TTL bind it as they bind a policy's grant.
	wantScope := `{"permissions":{"contents":"read"},"repos":["acme/api","Acme/web"]}`
	var found bool
	f.st.mu.Lock()
	for _, g := range f.st.grants {
		if g.RunID == created.ID && string(g.Spec.Scope) == wantScope {
			found = g.Spec.Kind == types.GrantGitHubToken && g.Spec.RequiresApproval && g.Spec.TTLSeconds == 1800
		}
	}
	f.st.mu.Unlock()
	if !found {
		t.Errorf("no github_token grant %s with the ceiling's approval and TTL", wantScope)
	}
	if _, ok := f.st.snapshots[created.ID]; ok {
		t.Errorf("a built-in wrote a component snapshot row: %+v", f.st.snapshots[created.ID])
	}

	fr := f.srv.cfg.Runner.(*fakeRunner)
	fr.waitForSandbox(t)
	fr.mu.Lock()
	spec := fr.lastSpec
	fr.mu.Unlock()
	if got := slices.Sorted(maps.Keys(spec.ProxyConfig.GitGrants)); !slices.Equal(got, []string{"acme/api", "acme/web"}) {
		t.Errorf("broker repos = %v, want exactly the component's", got)
	}
	if got := spec.Env["WARDYN_GIT_BROKER_REPOS"]; got != "acme/api acme/web" {
		t.Errorf("WARDYN_GIT_BROKER_REPOS = %q: the helper would mint into the sandbox", got)
	}
	for _, h := range gitBrokerManagedHosts {
		if !slices.Contains(spec.ProxyConfig.Policy.DeniedDomains, h) {
			t.Errorf("denied_domains %v lacks %q: a direct route to GitHub stays open", spec.ProxyConfig.Policy.DeniedDomains, h)
		}
	}

	want := `{"access":"read","kind":"git_provider","ordinal":0,"org":"acme","provider":"github","repos":["acme/api","Acme/web"],"source":"org"}`
	if got := f.componentAudit(0, "run.component.attach"); !slices.Equal(got, []string{created.ID.String() + " " + want}) {
		t.Errorf("run.component.attach = %v, want %s", got, want)
	}
	for _, e := range f.rec.snapshot() {
		if e.Action == "run.create" && e.Outcome == "success" && !strings.Contains(string(e.Data), `"components":[`+want+`]`) {
			t.Errorf("run.create = %s, want components [%s]", e.Data, want)
		}
	}
}

func TestRunComponents_BuiltinRefusals(t *testing.T) {
	ceiling := func(level string) func(f *componentFixture) {
		return func(f *componentFixture) { githubCeiling(f, level, false, 0) }
	}
	with := func(set func(*componentFixture), r ...any) func(*componentFixture) []any {
		return func(f *componentFixture) []any { set(f); return r }
	}
	for _, c := range []componentCase{
		{name: "a repository of another organisation", setup: with(ceiling("write"), githubBuiltin("", "acme/api", "other/x")),
			status: 400, reason: "component_ref_invalid", sentence: `components[0]: repos[1]: "other/x" is not a repository of acme`},
		{name: "one organisation twice", setup: with(ceiling("write"), githubBuiltin("", "acme/api"),
			map[string]any{"builtin": "github", "org": "ACME", "repos": []string{"ACME/web"}}),
			status: 400, reason: "component_ref_invalid", sentence: "components[1]: the same built-in component as components[0]"},
		{name: "no GitHub grant in the ceiling", setup: refs(githubBuiltin("", "acme/api")), status: 422,
			reason: "component_builtin_access_above_ceiling", sentence: "components[0]: access: your organisation does not allow read access to GitHub repositories on your runs."},
		{name: "write above the ceiling", setup: with(ceiling("read"), githubBuiltin("write", "acme/api")), status: 422,
			reason: "component_builtin_access_above_ceiling", sentence: "components[0]: access: your organisation does not allow write access to GitHub repositories on your runs."},
		{name: "GitHub on the deny list", setup: with(func(f *componentFixture) {
			ceiling("write")(f)
			f.srv.cfg.DefaultPolicy.DeniedDomains = []string{"api.github.com"}
		}, githubBuiltin("", "acme/api")), status: 422, reason: "component_host_denied", sentence: "components[0]: GitHub is blocked by your organisation."},
		{name: "a repository no provider admits", setup: with(func(f *componentFixture) {
			ceiling("write")(f)
			githubRows(f, types.GitLaneApp)
		}, map[string]any{"builtin": "github", "org": "other", "repos": []string{"other/x"}}), status: 403, reason: "workspace_repo_not_admitted"},
		{name: "the provider's App lane is off", setup: with(func(f *componentFixture) {
			ceiling("write")(f)
			githubRows(f, types.GitLanePAT)
		}, githubBuiltin("", "acme/api")), status: 422, reason: "component_builtin_lane_off"},
		{name: "the provider is not granted", setup: with(func(f *componentFixture) {
			ceiling("write")(f)
			githubRows(f, types.GitLaneApp)
			f.cs.grants = []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capWorkspaceProvider, "gh", types.CapabilityDeny)}
		}, githubBuiltin("", "acme/api")), status: 403, reason: "capability_workspace_provider"},
		{name: "azure devops is not available yet", setup: refs(map[string]any{"builtin": "azure_devops", "org": "acme"}),
			status: 422, reason: "request_field_unavailable",
			sentence: "components[0]: built-in azure_devops access is not available on this server yet, so the run was not created."},
	} {
		t.Run(c.name, c.run)
	}
}

// A refused built-in is audited at launch with its position and reason, and a
// refused dry run is not.
func TestRunComponents_BuiltinRefusalIsAudited(t *testing.T) {
	f := newComponentFixture(t)
	githubCeiling(f, "read", false, 0)
	body := componentBody(inlineComponent([]string{"ok.example"}), githubBuiltin("write", "acme/api"))
	for i, door := range componentDoors {
		before := len(f.rec.snapshot())
		if w := f.ask(t, door, body); w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s = %d %s, want 422", door, w.Code, w.Body.String())
		}
		rows := f.componentAudit(before, "run.component.refuse")
		want := []string{` {"ordinal":1,"reason":"component_builtin_access_above_ceiling","source":"org"}`}
		if i > 0 {
			want = nil
		}
		if !slices.Equal(rows, want) {
			t.Errorf("%s run.component.refuse = %v, want %v", door, rows, want)
		}
	}
}
