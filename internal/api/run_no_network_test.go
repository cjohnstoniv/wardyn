// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// noNetworkFold is a launch with the No network preset carrying every host source the fold can see: a
// workspace with a registry and a clone host, a repository on the operator's code host, an ssh_key, a
// git_pat, a github_token, a direct GitHub clone, a component and a model provider.
func noNetworkFold(t *testing.T, preset string) (runFold, *httptest.ResponseRecorder) {
	t.Helper()
	ws := types.Workspace{ID: uuid.New(), Name: "app", ApprovedEgress: []string{"registry.example"},
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "https://gitlab.example/team/app"}}}
	grants := []types.GrantSpec{
		{Kind: types.GrantSSHKey, Scope: mustJSON(map[string]string{"host": "github.com", "key_secret_ref": "deploy-key"})},
		gitPATGrant("dev.azure.com", "ado-pat"),
		{Kind: types.GrantGitHubToken, Scope: mustJSON(map[string]any{"repos": []string{"acme/app"}})},
	}
	req := createRunRequest{Agent: "claude-code", Repo: "https://ghes.corp.example/team/app"}
	w := httptest.NewRecorder()
	f := runFold{
		mode: foldCreate, w: w, r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil), req: &req,
		spec: types.RunPolicySpec{EligibleGrants: grants}, wsRefs: []types.Workspace{ws},
		scmSite:           types.SiteConfig{ScmHosts: []string{"ghes.corp.example"}},
		directGitHubAdded: []string{"github.com"},
		comps:             runComponents{attached: []attachedComponent{{snapshot: types.RunComponent{Name: "linear"}}}},
		mpChoice:          runProviderChoice{provider: types.ModelProvider{ID: "corp", Name: "Corp gateway"}, chosen: true},
		source:            policySourceRecord{wallPreset: builtinFrozen(t, preset)},
	}
	return f, w
}

// TestNoNetwork_CreateRefusesNamingEachSource: a No network launch that would need a host is refused 422
// no_network_needs_egress, and the refusal names every source. The step runs in all three doors
// (TestPreflightMirrorsLaunchGates pins it), so Review and the preview answer the same.
func TestNoNetwork_CreateRefusesNamingEachSource(t *testing.T) {
	for _, mode := range []foldMode{foldCreate, foldPreflight, foldPreview} {
		f, w := noNetworkFold(t, types.WallPresetNoNetwork)
		f.mode = mode
		if (&Server{}).stepNoNetwork(&f, &foldRecorder{}) {
			t.Fatalf("mode %d: a No network launch with hosts passed", mode)
		}
		var body errorBody
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != http.StatusUnprocessableEntity || body.Reason != reasonNoNetworkNeedsEgress {
			t.Fatalf("mode %d: %d %s, want 422 %s", mode, w.Code, w.Body, reasonNoNetworkNeedsEgress)
		}
		for _, src := range []string{derivedWorkspace, derivedSiteConfig, derivedSSH, derivedGitPAT, "github_direct", "component linear", "model provider Corp gateway"} {
			if !strings.Contains(body.Error, src) {
				t.Errorf("mode %d: the refusal does not name %q: %s", mode, src, body.Error)
			}
		}
	}
}

// A host still on the spec is named by the source the fold recorded adding it; and a No network launch
// with nothing that needs a host, or a launch with another preset, passes.
func TestNoNetwork_SpecHostsAndPasses(t *testing.T) {
	req := createRunRequest{Agent: "custom-image"}
	f := runFold{w: httptest.NewRecorder(), r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil), req: &req,
		spec:   types.RunPolicySpec{AllowedDomains: []string{"pypi.org"}},
		source: policySourceRecord{wallPreset: builtinFrozen(t, types.WallPresetNoNetwork)}}
	rec := &foldRecorder{}
	rec.add(fieldAllowed, "pypi.org", provSource{Kind: provKindWorkspace, Name: "app"}, provEffectAdded)
	var got []string
	noNetworkSpecSources(f.spec, rec, func(src string) { got = append(got, src) })
	if len(got) != 1 || got[0] != derivedWorkspace {
		t.Errorf("sources = %v, want [workspace]", got)
	}
	f.spec.AllowedDomains = nil
	if !(&Server{}).stepNoNetwork(&f, rec) {
		t.Error("a No network launch with no host source was refused")
	}
	sealed, _ := noNetworkFold(t, types.WallPresetSealed)
	if !(&Server{}).stepNoNetwork(&sealed, &foldRecorder{}) {
		t.Error("a Sealed launch was refused for its hosts")
	}
}

// TestNoNetwork_SealEmptiesEgress: whatever a step after resolve added, the No network preset's seal
// layer leaves no host and no open egress, and withholds every credential by host. Sealed's own
// layer keeps the allowlist.
func TestNoNetwork_SealEmptiesEgress(t *testing.T) {
	run := types.AgentRun{ID: uuid.New(), WallPreset: builtinFrozen(t, types.WallPresetNoNetwork)}
	layers := wallPresetSealLayer(run)
	spec := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com", "github.com", "registry.example"}, AllowAllEgress: true}
	sealPolicy(&spec, layers)
	if len(spec.AllowedDomains) != 0 || spec.AllowAllEgress {
		t.Fatalf("sealed allowed_domains %v allow_all_egress %v, want none and false", spec.AllowedDomains, spec.AllowAllEgress)
	}
	if w := (withholding{layers: layers}); !w.withheld("api.anthropic.com") || !w.withheld("github.com") {
		t.Error("a credential survived the No network seal")
	}
	sealedSpec := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}}
	sealPolicy(&sealedSpec, wallPresetSealLayer(types.AgentRun{ID: uuid.New(), WallPreset: builtinFrozen(t, types.WallPresetSealed)}))
	if len(sealedSpec.AllowedDomains) != 1 {
		t.Errorf("Sealed's seal changed the allowlist: %v", sealedSpec.AllowedDomains)
	}
}

// TestNoNetwork_ApprovalRefused: every egress approval for a No network run is refused, with the
// preset's refusal, whatever the store keeps.
func TestNoNetwork_ApprovalRefused(t *testing.T) {
	approve := func(preset string) (*httptest.ResponseRecorder, bool) {
		run := types.AgentRun{ID: uuid.New(), CreatedBy: "alice", WallPreset: builtinFrozen(t, preset)}
		w := httptest.NewRecorder()
		ap := types.ApprovalRequest{RunID: run.ID, Kind: types.ApprovalEgressDomain, RequestedScope: json.RawMessage(`{"host":"pypi.org"}`)}
		_, _, ok := (&Server{}).wallsAdmitApproval(w, httptest.NewRequest(http.MethodPost, "/api/v1/approvals/x/approve", nil), ap, run, true)
		return w, ok
	}
	w, ok := approve(types.WallPresetNoNetwork)
	var body errorBody
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if ok || w.Code != http.StatusForbidden || body.Reason != reasonWallPresetHost || !strings.Contains(body.Error, "No network") {
		t.Fatalf("approving for a No network run: ok %v %d %s, want 403 %s", ok, w.Code, w.Body, reasonWallPresetHost)
	}
	if _, ok := approve(types.WallPresetSealed); !ok {
		t.Error("a Sealed run's approval was refused by a store that keeps no walls")
	}
}

// TestNoNetwork_ProxyConfigSwitch: dispatch sets no_network from the run's preset, the switch survives
// the runner's mapping, and a revive on a runner replays it from the stored config.
func TestNoNetwork_ProxyConfigSwitch(t *testing.T) {
	if !noNetworkPreset(builtinFrozen(t, types.WallPresetNoNetwork)) || noNetworkPreset(builtinFrozen(t, types.WallPresetSealed)) || noNetworkPreset(nil) {
		t.Fatal("noNetworkPreset does not pick out No network alone")
	}
	stored := &types.WallPresetOnRun{ID: types.WallPresetNoNetwork, Scope: types.WallPresetScopePerson}
	if noNetworkPreset(stored) {
		t.Error("a stored preset with the built-in's id counted as No network")
	}
	if cfg := runner.ProxyConfigOf(uuid.New(), runner.ProxyConfig{NoNetwork: true}, runner.ProxyListenPort); !cfg.NoNetwork {
		t.Error("the runner's mapping dropped no_network")
	}
	if !localStoredSpec(proxy.Config{NoNetwork: true}).ProxyConfig.NoNetwork {
		t.Error("a runner revive dropped no_network")
	}
}

// TestNoNetwork_ReviveSealsAndKeepsTheSwitch: a revive of a No network run keeps the stored switch,
// and its seal leaves the replayed policy no host and withholds every credential the config carried.
func TestNoNetwork_ReviveSealsAndKeepsTheSwitch(t *testing.T) {
	f := newReviveFixture(t)
	f.st.run.WallPreset = builtinFrozen(t, types.WallPresetNoNetwork)
	f.run = f.st.run
	f.editConfig(t, func(c *proxy.Config) { c.NoNetwork = true })
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive: code %d, want 200", code)
	}
	cfg := f.newConfig(t)
	if !cfg.NoNetwork || len(cfg.Policy.AllowedDomains) != 0 || cfg.Policy.AllowAllEgress || len(cfg.Injection) != 0 {
		t.Errorf("revived no_network %v allowed %v allow_all %v injections %d, want true, none, false, none",
			cfg.NoNetwork, cfg.Policy.AllowedDomains, cfg.Policy.AllowAllEgress, len(cfg.Injection))
	}
}
