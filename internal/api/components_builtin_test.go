// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
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
	body := componentBody(githubBuiltin("", "acme/api", "Acme/Web")) // GitHub names are case-insensitive

	wantFact := componentFact{Kind: types.ComponentGitProvider, Provider: "github", ID: "inline:0", Name: "GitHub: acme",
		Reason: "org", Status: componentUnknown, Requirements: []SetupItem{}, Lane: "app", Org: "acme",
		Repos: []string{"https://github.com/acme/api", "https://github.com/acme/web"}}
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
	// One owner spelling, which the broker's mint requires of a grant (splitRepos).
	wantScope := `{"builtin":"github","permissions":{"contents":"read"},"repos":["acme/api","acme/web"]}`
	if err := broker.ValidateGitHubScopeShape(json.RawMessage(wantScope)); err != nil {
		t.Fatalf("the broker refuses the scope: %v", err)
	}
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

	want := `{"access":"read","kind":"git_provider","ordinal":0,"org":"acme","provider":"github","repos":["acme/api","acme/web"],"source":"org"}`
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
		{name: "one repository twice in two cases", setup: with(ceiling("write"), githubBuiltin("", "acme/api", "ACME/API")),
			status: 400, reason: "component_ref_invalid", sentence: `components[0]: repos[1]: "ACME/API" is repos[0] again`},
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

// A built-in's repositories are read back from its grant and nowhere else: a
// policy's own github_token grant names repositories that met no provider door
// at launch, and adds none at revive or extension.
func TestBuiltinGitHubRepos_ReadsOnlyTheBuiltinGrant(t *testing.T) {
	f := newComponentFixture(t)
	ceiling := governanceCeiling{Spec: types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{Kind: types.GrantGitHubToken,
		Scope: json.RawMessage(`{"repos":[],"permissions":{"contents":"read"}}`)}}}}
	builtin, ok := f.srv.builtinGitHubGrant(httptest.NewRequest(http.MethodPost, "/", nil), ceiling, []string{"acme/api"}, "read")
	if !ok {
		t.Fatal("no grant")
	}
	policy := types.GrantSpec{Kind: types.GrantGitHubToken, Scope: json.RawMessage(`{"repos":["acme/web"],"permissions":{"contents":"write"}}`)}
	if got := builtinGitHubRepos([]types.GrantSpec{policy, builtin}); !slices.Equal(got, []string{"https://github.com/acme/api"}) {
		t.Errorf("builtinGitHubRepos = %v, want the built-in's repository only", got)
	}
}

// An override keyed to a built-in's organisation finds the entry the built-in
// brings, so its rules reach the git route for those repositories.
func TestPushRuleSets_ABuiltinBringsItsEntry(t *testing.T) {
	req := prReq(client.PushRuleOverride{Provider: "github", Org: "acme", DenyPaths: []string{"infra/"}})
	req.Components = []types.ComponentRef{{Builtin: types.ComponentBuiltinGitHub, Org: "acme", Repos: []string{"acme/api"}}}
	sets, _, err := (&Server{}).pushRuleSetsFor(prSpec(), req, types.SiteConfig{})
	if err != nil || len(sets) != 1 || sets[0].Org != "acme" || !slices.Equal(sets[0].Rules.DenyPaths, []string{".github/workflows/**", "infra/"}) {
		t.Errorf("sets = %+v err %v, want acme's entry with the policy's deny and the override's", sets, err)
	}
}

// builtinRunGrants is a run's grants as the gate writes them for a built-in
// GitHub component on capProviderRepo, or, with policy, as a policy's own
// github_token grant naming the same repository.
func builtinRunGrants(t *testing.T, policy bool) []types.GrantSpec {
	t.Helper()
	if policy {
		return []types.GrantSpec{{Kind: types.GrantGitHubToken, Scope: json.RawMessage(`{"repos":["` + capProviderRepo + `"],"permissions":{"contents":"read"}}`)}}
	}
	g, ok := (&Server{}).builtinGitHubGrant(httptest.NewRequest(http.MethodPost, "/", nil), governanceCeiling{Spec: types.RunPolicySpec{
		EligibleGrants: []types.GrantSpec{{Kind: types.GrantGitHubToken, Scope: json.RawMessage(`{"repos":[],"permissions":{"contents":"read"}}`)}}}},
		[]string{capProviderRepo}, "read")
	if !ok {
		t.Fatal("no built-in grant")
	}
	return []types.GrantSpec{g}
}

// builtinCredGrants is builtinRunGrants as the run's credential_grants rows.
func builtinCredGrants(t *testing.T, policy bool) []types.CredentialGrant {
	var out []types.CredentialGrant
	for _, g := range builtinRunGrants(t, policy) {
		out = append(out, types.CredentialGrant{ID: uuid.New(), Spec: g})
	}
	return out
}

// A built-in GitHub run's provider is a launch door like a workspace's: the
// owner losing workspace_provider refuses the revive. A policy's own grant on
// the same repository is today's run, which met no provider door and revives.
func TestRevive_RechecksABuiltinsProvider(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy bool
		want   int
	}{{"built-in", false, http.StatusForbidden}, {"policy grant", true, http.StatusOK}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviveFixture(t)
			f.srv.cfg.OIDC = &oidc.Authenticator{}
			f.srv.router = f.srv.routes()
			f.st.site = capProviderSite()
			f.st.credGrants = builtinCredGrants(t, tc.policy)
			f.st.caps = []types.CapabilityGrant{grant(types.CapabilitySubjectUser, ownerEmail, capWorkspaceProvider, capProviderRowID, types.CapabilityDeny)}
			code, body := f.reviveAs(t, true)
			if code != tc.want {
				t.Fatalf("revive = %d %s, want %d", code, body, tc.want)
			}
			if tc.want == http.StatusForbidden {
				f.assertReviveRefused(t, "capability_"+capWorkspaceProvider)
			}
		})
	}
}

// The same door at an extension, which keeps the sandbox and its credentials
// alive as a revive does, read from the run's grants: this run has no stored
// proxy config.
func TestExtend_RechecksABuiltinsProvider(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy bool
		want   int
	}{{"built-in", false, http.StatusForbidden}, {"policy grant", true, http.StatusOK}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEndWaitFixture(t, types.RunLimits{MaxEndAheadSec: 30 * 86400, UserChangesLimits: true})
			f.st.site = capProviderSite()
			f.st.credGrants = builtinCredGrants(t, tc.policy)
			f.st.caps = []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capWorkspaceProvider, capProviderRowID, types.CapabilityDeny)}
			w := doSSO(t, f.srv, http.MethodPatch, "/api/v1/runs/"+f.st.run.ID.String(), ownerSessionAs(t), endsAtBody(f.now.Add(7*24*time.Hour)))
			if w.Code != tc.want {
				t.Fatalf("extend = %d %s, want %d", w.Code, w.Body.String(), tc.want)
			}
		})
	}
}

// githubPATGrant is a policy's git_pat grant for github.com.
const githubPATGrant = `{"kind":"git_pat","scope":{"host":"github.com","secret_name":"` + compOperatorSecret + `"}}`

// A brokered forge is single-lane: a built-in GitHub component beside a
// policy's github.com git_pat is refused as the policy holding both grants is.
func TestRunComponents_BuiltinMeetsTheSingleLaneRule(t *testing.T) {
	f := newComponentFixture(t)
	control := `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC1","eligible_grants":[` + githubPATGrant +
		`,{"kind":"github_token","scope":{"repos":["acme/api"],"permissions":{"contents":"read"}}}]}}`
	if w := do(t, f.srv, http.MethodPost, componentDoors[1], adminToken, control); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), "BOTH a github_token grant and a git_pat grant") {
		t.Fatalf("control = %d %s, want the policy's 400", w.Code, w.Body.String())
	}
	body := `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC1","eligible_grants":[` + githubPATGrant +
		`]},"components":[{"builtin":"github","org":"acme","repos":["acme/api"]}]}`
	for _, door := range componentDoors {
		w := do(t, f.srv, http.MethodPost, door, adminToken, body)
		if got := decodeErrorBody(t, w); w.Code != http.StatusBadRequest || got.Reason != "component_ref_invalid" ||
			!strings.HasPrefix(got.Error, "components[0]: eligible_grants[0]: this policy declares BOTH a github_token grant and a git_pat grant") {
			t.Errorf("%s = %d %s, want 400 component_ref_invalid naming both lanes", door, w.Code, w.Body.String())
		}
	}
}

// The run's other GitHub repositories are never mapped to a built-in's grant,
// whose token is scoped to the built-in's own: with a policy's github_token
// they keep that grant, without one they stay unbrokered.
func TestRunComponents_BuiltinGrantServesOnlyItsRepos(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy string
	}{{"with a policy grant", `,"inline_policy":{"min_confinement_class":"CC1","eligible_grants":[{"kind":"github_token","scope":{"repos":[],"permissions":{"contents":"write"}}}]}`}, {"without one", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newComponentFixture(t)
			w := do(t, f.srv, http.MethodPost, componentDoors[0], adminToken, `{"agent":"claude-code","task":"t","repo":"https://github.com/other/app"`+
				tc.policy+`,"components":[{"builtin":"github","org":"acme","repos":["acme/api"]}]}`)
			if w.Code != http.StatusCreated {
				t.Fatalf("create = %d %s", w.Code, w.Body.String())
			}
			fr := f.srv.cfg.Runner.(*fakeRunner)
			fr.waitForSandbox(t)
			fr.mu.Lock()
			gg := fr.lastSpec.ProxyConfig.GitGrants
			fr.mu.Unlock()
			f.st.mu.Lock()
			builtin := map[uuid.UUID]bool{}
			for _, g := range f.st.grants {
				builtin[g.ID] = isBuiltinGitHubGrant(g.Spec)
			}
			f.st.mu.Unlock()
			if id, ok := gg["acme/api"]; !ok || !builtin[id] {
				t.Errorf("acme/api -> %v, want the built-in's grant", gg)
			}
			id, mapped := gg["other/app"]
			if mapped && builtin[id] {
				t.Errorf("other/app is mapped to the built-in's grant: %v", gg)
			}
			if mapped != (tc.policy != "") {
				t.Errorf("other/app mapped = %v, want %v (the policy's grant only)", mapped, tc.policy != "")
			}
		})
	}
}

// ListGrantsByRun answers the owner re-check's read of a run's grants
// (ownerCapabilityRefusal) for the capability fixture: it holds none.
func (s *capStore) ListGrantsByRun(context.Context, uuid.UUID) ([]types.CredentialGrant, error) {
	return nil, nil
}
