// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func prSpec(repos ...string) types.RunPolicySpec {
	spec := types.RunPolicySpec{PushRules: &types.PushRulesSpec{DenyPaths: []string{".github/workflows/**"}, MaxFileSizeMiB: 5}}
	for _, r := range repos {
		spec.WorkspaceRepos = append(spec.WorkspaceRepos, types.WorkspaceRepo{Repo: r, Target: "/work/" + r})
	}
	return spec
}

func prReq(adds ...client.PushRuleOverride) *createRunRequest {
	return &createRunRequest{Overrides: &client.RunOverrides{PushRules: adds}}
}

// The policy's and the ceiling's rules are laid into each entry's set first; the
// request may only add to them, and its set for an entry replaces nothing.
func TestPushRuleSets_RequestOnlyAddsAndAdminDenyStays(t *testing.T) {
	s := &Server{}
	sets, _, err := s.pushRuleSetsFor(prSpec("https://github.com/Acme/api.git", "https://github.com/other/web.git"),
		prReq(client.PushRuleOverride{Provider: "github", Org: "ACME", DenyPaths: []string{"infra/"}, RequireReviewPaths: []string{"ci/"}}), types.SiteConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 2 {
		t.Fatalf("sets = %+v, want one per entry", sets)
	}
	byOrg := map[string]types.PushRuleSet{}
	for _, set := range sets {
		byOrg[set.Org] = set
	}
	acme, other := byOrg["acme"].Rules, byOrg["other"].Rules
	if !slices.Equal(acme.DenyPaths, []string{".github/workflows/**", "infra/"}) || !slices.Equal(acme.RequireReviewPaths, []string{"ci/"}) || acme.MaxFileSizeMiB != 5 {
		t.Errorf("acme = %+v, want the policy's deny first, the person's added, the policy's bounds kept", acme)
	}
	if !slices.Equal(other.DenyPaths, []string{".github/workflows/**"}) || len(other.RequireReviewPaths) != 0 {
		t.Errorf("other = %+v, want only the policy's rules: the request named acme", other)
	}
	// A request cannot remove or loosen: with nothing but its own adds the admin's deny is still in.
	only, _, err := s.pushRuleSetsFor(prSpec("https://github.com/acme/api.git"),
		prReq(client.PushRuleOverride{Provider: "github", Org: "acme", RequireReviewPaths: []string{"docs/"}}), types.SiteConfig{})
	if err != nil || len(only) != 1 || !slices.Contains(only[0].Rules.DenyPaths, ".github/workflows/**") {
		t.Errorf("sets = %+v err %v, want the admin deny kept beside a person's review path", only, err)
	}
}

// A run with no SCM entry carries no push rules, whatever the policy and the
// request hold; a request for an entry the run does not bring adds nothing.
func TestPushRuleSets_NoSCMEntryCarriesNone(t *testing.T) {
	s := &Server{}
	sets, _, err := s.pushRuleSetsFor(prSpec(), prReq(client.PushRuleOverride{Provider: "github", Org: "acme", DenyPaths: []string{"infra/"}}), types.SiteConfig{})
	if err != nil || len(sets) != 0 {
		t.Errorf("no repository: sets = %+v err %v, want none", sets, err)
	}
	sets, _, err = s.pushRuleSetsFor(prSpec("https://github.com/acme/api.git"),
		prReq(client.PushRuleOverride{Provider: "github", Org: "elsewhere", DenyPaths: []string{"infra/"}}), types.SiteConfig{})
	if err != nil || len(sets) != 1 || sets[0].Org != "acme" || slices.Contains(sets[0].Rules.DenyPaths, "infra/") {
		t.Errorf("an entry the run does not bring: sets = %+v err %v, want only acme's policy rules", sets, err)
	}
	if sets, _, err = s.pushRuleSetsFor(types.RunPolicySpec{WorkspaceRepos: prSpec("acme/api").WorkspaceRepos}, nil, types.SiteConfig{}); err != nil || sets != nil {
		t.Errorf("no rules anywhere: sets = %+v err %v, want nil", sets, err)
	}
}

// An Azure DevOps Server row's base path is recorded on the set, so the proxy
// derives the collection the way the row spells it; hosted entries record none.
func TestPushRuleSets_ADOServerRecordsTheRowBasePath(t *testing.T) {
	sc := types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{{
		ID: "tfs", Kind: types.GitProviderAzureDevOps, BaseURLs: []string{"https://tfs.corp.example/tfs"}}}}}
	sets, _, err := (&Server{}).pushRuleSetsFor(prSpec("https://tfs.corp.example/tfs/Coll/Proj/_git/app"), nil, sc)
	if err != nil || len(sets) != 1 {
		t.Fatalf("sets = %+v err %v", sets, err)
	}
	if got := sets[0]; got.Provider != "azure_devops" || got.Host != "tfs.corp.example" || got.Org != "coll" || got.BasePath != "/tfs" {
		t.Errorf("set = %+v", got)
	}
	hosted, _, err := (&Server{}).pushRuleSetsFor(prSpec("https://dev.azure.com/acme/Proj/_git/app"), nil, types.SiteConfig{})
	if err != nil || len(hosted) != 1 || hosted[0].BasePath != "" || hosted[0].Org != "acme" || hosted[0].Host != "dev.azure.com" {
		t.Errorf("hosted = %+v err %v", hosted, err)
	}
	// A prefix with no base path on the row is ambiguous: the entry cannot be keyed, and the run is refused.
	bare := types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{{
		ID: "tfs", Kind: types.GitProviderAzureDevOps, BaseURLs: []string{"https://tfs.corp.example"}}}}}
	if _, _, err := (&Server{}).pushRuleSetsFor(prSpec("https://tfs.corp.example/tfs/Coll/Proj/_git/app"), nil, bare); err == nil {
		t.Error("a path prefix with no recorded base path derived an entry")
	}
}

// One Azure DevOps Server host serving two collections, one base URL each: a repository in the second
// collection is keyed under its own base URL, never the first one on the host.
func TestPushRuleSets_ADOServerPicksTheBaseURLTheRepositoryIsUnder(t *testing.T) {
	sc := types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{{
		ID: "tfs", Kind: types.GitProviderAzureDevOps,
		BaseURLs: []string{"https://tfs.corp.example/tfs/CollA", "https://tfs.corp.example/tfs/CollB"}}}}}
	sets, _, err := (&Server{}).pushRuleSetsFor(prSpec("https://tfs.corp.example/tfs/CollB/Proj/_git/app", "https://tfs.corp.example/tfs/CollA/Proj/_git/web"), nil, sc)
	if err != nil || len(sets) != 2 {
		t.Fatalf("sets = %+v err %v, want one per collection", sets, err)
	}
	if sets[0].Org != "collb" || sets[0].BasePath != "/tfs/CollB" || sets[1].Org != "colla" || sets[1].BasePath != "/tfs/CollA" {
		t.Errorf("sets = %+v, want each collection under its own base URL", sets)
	}
}

// Dispatch hands the sidecar the per-entry sets and takes the run-wide block off
// its policy (the proxy refuses both together); a run with no sets keeps the block.
func TestPushRuleSets_DispatchCarriesSetsInsteadOfTheRunWideBlock(t *testing.T) {
	sets := []types.PushRuleSet{{Provider: "github", Host: "github.com", Org: "acme",
		Rules: types.PushRulesSpec{DenyPaths: []string{"infra/"}, RequireReviewPaths: []string{"ci/"}}}}
	for _, tc := range []struct {
		name       string
		sets       []types.PushRuleSet
		wantPolicy bool
	}{{"with sets", sets, false}, {"without sets", nil, true}} {
		fr := &fakeRunner{}
		srv, _, _, run := dispatchTeardownFixture(t, fr, types.RunPending)
		run.Task = ""
		srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
			RunToken: "run-token", Image: "wardyn/claude-code:latest",
			Policy:       types.RunPolicySpec{PushRules: &types.PushRulesSpec{DenyPaths: []string{".github/**"}}},
			PushRuleSets: tc.sets,
		})
		pc := fr.lastSpec.ProxyConfig
		if (pc.Policy.PushRules != nil) != tc.wantPolicy || !slices.EqualFunc(pc.PushRuleSets, tc.sets, func(a, b types.PushRuleSet) bool { return a.Key() == b.Key() }) {
			t.Errorf("%s: policy push_rules %v, sets %+v", tc.name, pc.Policy.PushRules, pc.PushRuleSets)
		}
		// The run's base rules ride beside the sets, for an entry the run reaches but did not bring.
		if (pc.PushRuleBase != nil) != !tc.wantPolicy || pc.PushRuleBase != nil && !slices.Equal(pc.PushRuleBase.DenyPaths, []string{".github/**"}) {
			t.Errorf("%s: push_rule_base %+v", tc.name, pc.PushRuleBase)
		}
		if got := pc.Unattended; got != (tc.sets != nil) {
			t.Errorf("%s: unattended = %v, want it set only when a set holds a review path", tc.name, got)
		}
	}
}

// The Azure DevOps Server row's base path reaches the sidecar's sets through the
// fold itself: the step reads the site config for it (the autonomy step's
// snapshot is not taken yet at that point in the order).
func TestPushRuleSets_FoldReadsTheRowBasePathFromTheSiteConfig(t *testing.T) {
	srv, st := pvFixture(t, &capStore{})
	st.siteConfig = types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{{
		ID: "tfs", Kind: types.GitProviderAzureDevOps, BaseURLs: []string{"https://tfs.corp.example/tfs"}}}}}
	body := `{"agent":"claude-code","task":"t","repo":"https://tfs.corp.example/tfs/Coll/Proj/_git/app",` +
		`"inline_policy":{"min_confinement_class":"CC1","push_rules":{"deny_paths":[".github/**"]}}}`
	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	waitForRecAudit(t, st.audit, ovOnlyRun(t, st), "run.policy.resolve", "success")
	runner := srv.cfg.Runner.(*fakeRunner)
	runner.mu.Lock()
	pc := runner.lastSpec.ProxyConfig
	runner.mu.Unlock()
	if len(pc.PushRuleSets) != 1 || pc.PushRuleSets[0].BasePath != "/tfs" || pc.PushRuleSets[0].Org != "coll" {
		t.Fatalf("sets = %+v, want the Coll collection under /tfs", pc.PushRuleSets)
	}
	if pc.PushRuleBase == nil || !slices.Equal(pc.PushRuleBase.DenyPaths, []string{".github/**"}) {
		t.Errorf("base = %+v, want the policy's rules for an entry the run did not bring", pc.PushRuleBase)
	}
}
