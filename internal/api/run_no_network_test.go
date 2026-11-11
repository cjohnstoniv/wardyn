// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
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
		spec: types.RunPolicySpec{EligibleGrants: grants, WorkspaceRepos: []types.WorkspaceRepo{{Repo: ws.Sources[0].Source}}}, wsRefs: []types.Workspace{ws},
		scmSite:           types.SiteConfig{ScmHosts: []string{"ghes.corp.example"}},
		directGitHubAdded: []string{"github.com"},
		comps: runComponents{attached: []attachedComponent{{snapshot: types.RunComponent{Name: "linear",
			Definition: types.ComponentDefinition{Hosts: []string{"api.linear.app"}}}}}},
		mpChoice: runProviderChoice{provider: types.ModelProvider{ID: "corp", Name: "Corp gateway"}, chosen: true},
		source:   policySourceRecord{wallPreset: builtinFrozen(t, preset)},
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
		for _, src := range []string{"repository", derivedWorkspace, derivedSiteConfig, derivedSSH, derivedGitPAT, "github_direct", "component linear", "model provider Corp gateway"} {
			if !strings.Contains(body.Error, src) {
				t.Errorf("mode %d: the refusal does not name %q: %s", mode, src, body.Error)
			}
		}
	}
}

// noNetworkModelLaunch drives door as an admin with a model run on kind k under sc (joinCreate's states: the
// owner's credential stored, the secret store wedged), plus extra body fields, and returns the answer and
// the audit rows it wrote.
func noNetworkModelLaunch(t *testing.T, k joinKind, sc joinScenario, door, extra string) (*httptest.ResponseRecorder, []types.AuditEvent) {
	t.Helper()
	p := k.record(sc)
	srv := providerRunFixture(t, types.SiteConfig{ModelProviders: providerBlock(p)}, &capStore{}, nil)
	srv.cfg.AgentImages = map[string]string{"claude-code": "wardyn/agent-claude-code:local"}
	srv.cfg.Runner = nil
	if sc.stored {
		if err := k.store(srv.cfg.Secrets.(*memSecrets).For(joinCreateOwner), p); err != nil {
			t.Fatal(err)
		}
	}
	if sc.wedged {
		srv.cfg.Secrets = wedgedSecrets{err: errors.New("age: no identity matched")}
	}
	body := `{"agent":"claude-code","task":"t","model_provider":"` + p.ID + `"` + extra + `}`
	w := do(t, srv, http.MethodPost, door, providerAdminToken(srv, joinCreateOwner), body)
	return w, srv.cfg.Audit.(*recRecorder).snapshot()
}

const noNetworkWallPreset = `,"wall_preset":"` + types.WallPresetNoNetwork + `"`

// noCredentialTrace fails t for any sign of the model credential having been read or answered: the word in
// the body, a model_credential or model_provider_unavailable audit row, or a sign-in renewal row.
func noCredentialTrace(t *testing.T, label string, w *httptest.ResponseRecorder, rows []types.AuditEvent) {
	t.Helper()
	if strings.Contains(strings.ToLower(w.Body.String()), "credential") {
		t.Errorf("%s: the refusal mentions the model credential: %s", label, w.Body)
	}
	for _, ev := range rows {
		d := strings.ToLower(string(ev.Data))
		if ev.Action == "harness.credential.refresh" || strings.Contains(d, llmRefusalAuditReason) || strings.Contains(d, "model_provider_unavailable") {
			t.Errorf("%s: audit row %s %s carries a model credential read", label, ev.Action, ev.Data)
		}
	}
}

// wantNoNetworkRefusal checks w is the 422 no_network_needs_egress body: exactly the keys error and reason,
// and the error is stepNoNetwork's sentence naming the model among its sources.
func wantNoNetworkRefusal(t *testing.T, label string, w *httptest.ResponseRecorder) {
	t.Helper()
	var keys map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &keys)
	if w.Code != http.StatusUnprocessableEntity || len(keys) != 2 || keys["reason"] != reasonNoNetworkNeedsEgress {
		t.Fatalf("%s: %d %s, want 422 with only error and reason %s", label, w.Code, w.Body, reasonNoNetworkNeedsEgress)
	}
	for _, part := range []string{"This run uses the No network wall preset, and these need a network host: ",
		"the claude-code harness's model", "Remove them, or launch without No network."} {
		if !strings.Contains(keys["error"], part) {
			t.Errorf("%s: error %q lacks %q", label, keys["error"], part)
		}
	}
}

// TestNoNetwork_ModelRunIsRefusedBeforeItsCredential: a No network launch of a model-backed harness is
// refused 422 no_network_needs_egress at create, Review and the preview alike, though the caller's model
// credential is missing (a model_credential refusal otherwise). The body is exactly the two keys error and
// reason: the message is stepNoNetwork's sentence naming its host sources, and says nothing of the
// credential, and no model_credential or model_provider_unavailable row is written. The same launch
// without No network still gets the model_credential 422 at create and Review.
//
// It catches stepModelProvider reading the credential first again (a model_credential body, or the row),
// and a skip that is too wide (the control launch losing its model_credential refusal).
func TestNoNetwork_ModelRunIsRefusedBeforeItsCredential(t *testing.T) {
	k := joinKinds()[0] // anthropic_api_key, serving claude-code; nothing is stored, so the credential is missing
	sc := joinScenario{chosen: true}
	for _, door := range []string{"/api/v1/runs", preflightDoor, previewDoor} {
		w, rows := noNetworkModelLaunch(t, k, sc, door, noNetworkWallPreset)
		wantNoNetworkRefusal(t, door, w)
		if !strings.Contains(w.Body.String(), "model provider "+k.p.ID) {
			t.Errorf("%s: the refusal does not name the model provider: %s", door, w.Body)
		}
		noCredentialTrace(t, door, w, rows)
	}
	for _, door := range []string{"/api/v1/runs", preflightDoor} {
		if w, _ := noNetworkModelLaunch(t, k, sc, door, ""); w.Code != http.StatusUnprocessableEntity || errorReason(w) != llmRefusalAuditReason {
			t.Errorf("%s without No network: %d %s, want 422 %s", door, w.Code, w.Body, llmRefusalAuditReason)
		}
	}
}

// TestNoNetwork_UnreadableCredentialIsStillNoNetwork: with the secret store failing every read, a No network
// model launch is still 422 no_network_needs_egress at create and Review, not the 503 an unreadable credential
// answers, and the body says nothing of it. Without No network the same launch is that 503.
//
// It catches the skip being applied after the credential read (so the read error answers 503 first), or the
// error of a skipped read still being returned.
func TestNoNetwork_UnreadableCredentialIsStillNoNetwork(t *testing.T) {
	k := joinKinds()[0]
	sc := joinScenario{stored: true, wedged: true, chosen: true}
	for _, door := range []string{"/api/v1/runs", preflightDoor} {
		w, rows := noNetworkModelLaunch(t, k, sc, door, noNetworkWallPreset)
		wantNoNetworkRefusal(t, door, w)
		noCredentialTrace(t, door, w, rows)
		if w, _ := noNetworkModelLaunch(t, k, sc, door, ""); w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s without No network: %d %s, want 503", door, w.Code, w.Body)
		}
	}
}

// TestNoNetwork_ExpiredSignInIsNotRenewed: the Bedrock SSO kind holds an expired but renewable sign-in. A
// No network launch at create is refused no_network_needs_egress with no CreateToken call, no
// harness.credential.refresh row and the stored pair untouched, and no run row. Without No network the same
// create renews once (the control, which also shows the row filter sees a renewal row).
//
// It catches the renewal still running before the refusal (a create-time renewal for a launch that will
// not start: an OIDC call, a refresh row and a rewritten pair), and a skip that also stops the ordinary
// renewal.
func TestNoNetwork_ExpiredSignInIsNotRenewed(t *testing.T) {
	launch := func(extra string) (*httptest.ResponseRecorder, []types.AuditEvent, *atomic.Int32, *Server) {
		srv := createRenewalFixture(t)
		calls := renewedOIDC(t)
		body := strings.TrimSuffix(createRenewalBody, "}") + extra + "}"
		w := do(t, srv, http.MethodPost, "/api/v1/runs", providerAdminToken(srv, createRenewalOwner), body)
		return w, srv.cfg.Audit.(*recRecorder).snapshot(), calls, srv
	}
	w, rows, calls, srv := launch(noNetworkWallPreset)
	wantNoNetworkRefusal(t, "create", w)
	noCredentialTrace(t, "create", w, rows)
	if calls.Load() != 0 {
		t.Errorf("CreateToken calls = %d, want 0: the sign-in was renewed for a launch that is refused", calls.Load())
	}
	if got, found := createRenewalStored(t, srv); !found || got.AccessToken != createRenewalBlob().AccessToken {
		t.Errorf("stored pair = %+v, want the old one untouched", got)
	}
	if n := runRowCount(srv); n != 0 {
		t.Errorf("run rows = %d, want none", n)
	}
	w, rows, calls, _ = launch("")
	if w.Code != http.StatusCreated || calls.Load() != 1 || !slices.ContainsFunc(rows, func(ev types.AuditEvent) bool { return ev.Action == "harness.credential.refresh" }) {
		t.Errorf("control create = %d %s, CreateToken calls %d, want 201, one renewal and a refresh row", w.Code, w.Body, calls.Load())
	}
}

// TestNoNetwork_ExtraHarnessCredentialIsNotRead: a No network model run that adds a second harness
// (codex-cli) on providers the caller has no key for passes the model-provider step with nothing written,
// so it reaches stepNoNetwork; the same fold without No network refuses with model_credential.
//
// It catches stepModelProvider handing judgeHarnessProviders the unmarked request instead of the marked
// one (the primary's read is skipped, the extra harness's key is read and refused), and the skip being
// dropped from the primary too.
func TestNoNetwork_ExtraHarnessCredentialIsNotRead(t *testing.T) {
	openai := keyProvider("openai", "codex-cli")
	openai.Kind = types.ModelProviderOpenAIAPIKey
	site := types.SiteConfig{ModelProviders: providerBlock(keyProvider("anthropic", "claude-code"), openai)}
	cs := &capStore{enf: map[string]bool{capModelProvider: true}, grants: []types.CapabilityGrant{
		grant(types.CapabilitySubjectAll, "", capModelProvider, "anthropic", types.CapabilityAllow),
		grant(types.CapabilitySubjectAll, "", capModelProvider, "openai", types.CapabilityAllow)}}
	srv := providerRunFixture(t, site, cs, nil)
	req := twoHarnessRequest("claude-code",
		client.IncludedTool{ID: "claude-code", Kind: client.IncludedToolHarness, ModelProvider: "anthropic"},
		client.IncludedTool{ID: "codex-cli", Kind: client.IncludedToolHarness, ModelProvider: "openai"})
	for _, tc := range []struct {
		name   string
		preset *types.WallPresetOnRun
	}{{"No network", builtinFrozen(t, types.WallPresetNoNetwork)}, {"no preset", nil}} {
		w := httptest.NewRecorder()
		f := runFold{mode: foldCreate, w: w, r: memberRequest(t), req: &req, harnesses: harnessSet(req), source: policySourceRecord{wallPreset: tc.preset}}
		ok := srv.stepModelProvider(&f, &foldRecorder{})
		if tc.preset != nil && (!ok || w.Body.Len() != 0) {
			t.Errorf("%s: ok=%v %d %s, want the step to pass with nothing written", tc.name, ok, w.Code, w.Body)
		}
		if tc.preset == nil && (ok || errorReason(w) != llmRefusalAuditReason) {
			t.Errorf("%s: ok=%v %d %s, want a model_credential refusal", tc.name, ok, w.Code, w.Body)
		}
	}
}

// TestNoNetwork_ExtraHarnessOnlyModelRunIsRefusedBeforeItsCredential: a run whose primary harness needs no
// model ("none", your own tools) and whose extra harness does (codex-cli on a granted provider the caller has
// no key for) is a valid set at every door. Under No network the model-provider step passes with nothing
// written and stepNoNetwork then refuses 422 no_network_needs_egress naming the extra's provider, with no
// word of a credential and no model_credential row. The same fold without the preset is refused
// model_credential naming codex-cli.
//
// It catches the credential skip being keyed on the primary harness alone (the extra's key is then read and
// refused first), and a refusal that names the credential.
func TestNoNetwork_ExtraHarnessOnlyModelRunIsRefusedBeforeItsCredential(t *testing.T) {
	openai := keyProvider("openai", "codex-cli")
	openai.Kind = types.ModelProviderOpenAIAPIKey
	cs := &capStore{enf: map[string]bool{capModelProvider: true},
		grants: []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capModelProvider, "openai", types.CapabilityAllow)}}
	srv := providerRunFixture(t, types.SiteConfig{ModelProviders: providerBlock(openai)}, cs, nil)
	req := twoHarnessRequest("",
		client.IncludedTool{ID: "none", Kind: client.IncludedToolHarness},
		client.IncludedTool{ID: "codex-cli", Kind: client.IncludedToolHarness, ModelProvider: "openai"})
	fold := func(preset *types.WallPresetOnRun) (runFold, *httptest.ResponseRecorder) {
		w := httptest.NewRecorder()
		return runFold{mode: foldCreate, w: w, r: memberRequest(t), req: &req, harnesses: harnessSet(req), source: policySourceRecord{wallPreset: preset}}, w
	}

	f, w := fold(builtinFrozen(t, types.WallPresetNoNetwork))
	if !srv.stepModelProvider(&f, &foldRecorder{}) || w.Body.Len() != 0 {
		t.Fatalf("model-provider step under No network: %d %s, want it to pass with nothing written", w.Code, w.Body)
	}
	if srv.stepNoNetwork(&f, &foldRecorder{}) {
		t.Fatal("a No network launch with a model harness's provider passed")
	}
	var keys map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &keys)
	if w.Code != http.StatusUnprocessableEntity || len(keys) != 2 || keys["reason"] != reasonNoNetworkNeedsEgress ||
		!strings.Contains(keys["error"], "model provider openai") {
		t.Errorf("answer %d %s, want 422 %s naming model provider openai", w.Code, w.Body, reasonNoNetworkNeedsEgress)
	}
	noCredentialTrace(t, "none + codex-cli", w, srv.cfg.Audit.(*recRecorder).snapshot())

	f, w = fold(nil)
	if srv.stepModelProvider(&f, &foldRecorder{}) || errorReason(w) != llmRefusalAuditReason || bodyTool(t, w) != "codex-cli" {
		t.Errorf("without No network: %d %s, want model_credential naming codex-cli", w.Code, w.Body)
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

// noNetworkRepoFold is a custom-image No network launch on a default-shaped policy: a github_token grant
// whose repositories are the empty template, a git_pat and an ssh_key, in a deployment whose site config
// names an SCM host. The resolved allowlist is empty, as No network's walls leave it.
func noNetworkRepoFold(t *testing.T, repo string, scmHosts ...string) (runFold, *httptest.ResponseRecorder) {
	t.Helper()
	grants := []types.GrantSpec{
		{Kind: types.GrantGitHubToken, Scope: mustJSON(map[string]any{"repos": []string{}})},
		gitPATGrant("dev.azure.com", "ado-pat"),
		{Kind: types.GrantSSHKey, Scope: mustJSON(map[string]string{"host": "github.com", "key_secret_ref": "deploy-key"})},
	}
	req := createRunRequest{Agent: "custom-image", Repo: repo}
	w := httptest.NewRecorder()
	return runFold{
		mode: foldCreate, w: w, r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil), req: &req,
		spec:    types.RunPolicySpec{EligibleGrants: grants},
		scmSite: types.SiteConfig{ScmHosts: scmHosts},
		source:  policySourceRecord{wallPreset: builtinFrozen(t, types.WallPresetNoNetwork)},
	}, w
}

// TestNoNetwork_GitGrantsCountOnlyWithARepository: the policy's git grants and the site's SCM hosts are not
// a source for a run that clones no repository, since the launcher can remove neither, so a default policy
// launches. With a repository they are, and the refusal names them.
func TestNoNetwork_GitGrantsCountOnlyWithARepository(t *testing.T) {
	for _, mode := range []foldMode{foldCreate, foldPreflight, foldPreview} {
		f, w := noNetworkRepoFold(t, "", "ghes.corp.example")
		f.mode = mode
		if !(&Server{}).stepNoNetwork(&f, &foldRecorder{}) {
			t.Fatalf("mode %d: a default policy with no repository was refused: %s", mode, w.Body)
		}
		f, w = noNetworkRepoFold(t, "https://ghes.corp.example/team/app", "ghes.corp.example")
		f.mode = mode
		if (&Server{}).stepNoNetwork(&f, &foldRecorder{}) {
			t.Fatalf("mode %d: a repository with git grants passed", mode)
		}
		var body errorBody
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		for _, src := range []string{derivedSiteConfig, derivedSSH, derivedGitPAT} {
			if !strings.Contains(body.Error, src) {
				t.Errorf("mode %d: the refusal does not name %q: %s", mode, src, body.Error)
			}
		}
	}
}

// TestNoNetwork_BrokeredRepositoryRefused: a GitHub repository cloned through the broker adds no host,
// in a deployment whose site config names no SCM host; it is still refused at create, naming the
// repository, rather than launching a run that loses its clone at dispatch.
func TestNoNetwork_BrokeredRepositoryRefused(t *testing.T) {
	f, w := noNetworkRepoFold(t, "acme/app")
	f.spec.EligibleGrants = f.spec.EligibleGrants[:1]
	if (&Server{}).stepNoNetwork(&f, &foldRecorder{}) {
		t.Fatal("a brokered GitHub repository passed No network")
	}
	var body errorBody
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(body.Error, "repository") {
		t.Fatalf("%d %s, want 422 naming the repository", w.Code, w.Body)
	}
}

// TestNoNetwork_FolderOnlyWorkspacePasses: a workspace attached for its folders alone (repos: []) keeps
// its repository source off the spec, clones nothing, and launches; with that repository selected onto the
// spec it is refused, naming the repository.
func TestNoNetwork_FolderOnlyWorkspacePasses(t *testing.T) {
	ws := types.Workspace{ID: uuid.New(), Name: "app", Sources: []types.WorkspaceSource{
		{Type: types.WorkspaceSourceTypeRepo, Source: "https://github.com/acme/app"},
		{Type: types.WorkspaceSourceTypeLocalDir, Source: "/home/dev/app"},
	}}
	f, w := noNetworkRepoFold(t, "")
	f.spec.EligibleGrants = f.spec.EligibleGrants[:1]
	f.wsRefs = []types.Workspace{ws}
	if !(&Server{}).stepNoNetwork(&f, &foldRecorder{}) {
		t.Fatalf("a folder-only workspace was refused: %s", w.Body)
	}
	f, w = noNetworkRepoFold(t, "")
	f.spec.EligibleGrants = f.spec.EligibleGrants[:1]
	f.wsRefs = []types.Workspace{ws}
	f.spec.WorkspaceRepos = []types.WorkspaceRepo{{Repo: "https://github.com/acme/app"}}
	if (&Server{}).stepNoNetwork(&f, &foldRecorder{}) || !strings.Contains(w.Body.String(), "repository") {
		t.Fatalf("a selected repository passed or was not named: %s", w.Body)
	}
}

// TestNoNetwork_FolderOnlyNonGitHubWorkspace: an operator-owned workspace whose repository is on a
// non-GitHub forge, attached for its folders alone, derives no clone host and launches in every door; with
// that repository selected it is refused, naming the workspace's clone host and the repository.
func TestNoNetwork_FolderOnlyNonGitHubWorkspace(t *testing.T) {
	ws := types.Workspace{ID: uuid.New(), Name: "app", Sources: []types.WorkspaceSource{
		{Type: types.WorkspaceSourceTypeRepo, Source: "https://gitlab.example/team/app"},
		{Type: types.WorkspaceSourceTypeLocalDir, Source: "/home/dev/app"},
	}}
	for _, mode := range []foldMode{foldCreate, foldPreflight, foldPreview} {
		f, w := noNetworkRepoFold(t, "")
		f.mode, f.wsRefs = mode, []types.Workspace{ws}
		if !(&Server{}).stepNoNetwork(&f, &foldRecorder{}) {
			t.Fatalf("mode %d: a folder-only workspace on gitlab.example was refused: %s", mode, w.Body)
		}
		f, w = noNetworkRepoFold(t, "")
		f.mode, f.wsRefs = mode, []types.Workspace{ws}
		f.spec.WorkspaceRepos = []types.WorkspaceRepo{{Repo: "https://gitlab.example/team/app"}}
		if (&Server{}).stepNoNetwork(&f, &foldRecorder{}) {
			t.Fatalf("mode %d: the workspace's selected repository passed", mode)
		}
		for _, src := range []string{derivedWorkspace, "repository"} {
			if !strings.Contains(w.Body.String(), src) {
				t.Errorf("mode %d: the refusal does not name %q: %s", mode, src, w.Body)
			}
		}
	}
}

// TestNoNetwork_ComponentsByWhatTheyNeed: a component that needs no host (config only, an env secret, or a
// local MCP server run from the image) does not refuse a No network launch; a remote MCP server does, though
// its host is never on the allowlist.
func TestNoNetwork_ComponentsByWhatTheyNeed(t *testing.T) {
	attach := func(defs ...types.ComponentDefinition) (bool, string) {
		f, w := noNetworkRepoFold(t, "")
		for i, d := range defs {
			f.comps.attached = append(f.comps.attached, attachedComponent{snapshot: types.RunComponent{Name: "c" + strconv.Itoa(i), Definition: d}})
		}
		return (&Server{}).stepNoNetwork(&f, &foldRecorder{}), w.Body.String()
	}
	offline := []types.ComponentDefinition{
		{Config: map[string]string{"REGION": "eu"}},
		{Secrets: []types.ComponentSecret{{SecretName: "token", Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryEnv, Var: "TOKEN"}}}},
		{MCP: &types.ComponentMCP{Transport: types.ComponentMCPTransportStdio, Launch: &types.ComponentMCPLaunch{Command: "/usr/local/bin/mcp-files"}}},
	}
	if ok, body := attach(offline...); !ok {
		t.Fatalf("components that need no host refused the launch: %s", body)
	}
	remote := types.ComponentDefinition{Hosts: []string{"mcp.example"},
		MCP: &types.ComponentMCP{Transport: types.ComponentMCPTransportHTTP, URL: "https://mcp.example/mcp"}}
	if raw := rawEgressHosts(remote); len(raw) != 0 {
		t.Fatalf("rawEgressHosts(remote MCP) = %v, want none", raw)
	}
	npx := types.ComponentDefinition{MCP: &types.ComponentMCP{Transport: types.ComponentMCPTransportStdio,
		Launch: &types.ComponentMCPLaunch{Command: "npx", Args: []string{"-y", "@example/mcp-server@1.4.2"}}}}
	byPath := func(cmd string) types.ComponentDefinition {
		return types.ComponentDefinition{MCP: &types.ComponentMCP{Transport: types.ComponentMCPTransportStdio,
			Launch: &types.ComponentMCPLaunch{Command: cmd, Args: []string{"mcp-server-files"}}}}
	}
	for name, d := range map[string]types.ComponentDefinition{"remote MCP": remote, "npx MCP": npx,
		"uvx by path": byPath("/usr/local/bin/uvx"), "pipx by path": byPath("/usr/bin/pipx"),
		"bunx by path": byPath("/usr/local/bin/bunx"), "pnpx by path": byPath("/usr/local/bin/pnpx"),
		"npx by path": byPath("/usr/local/bin/npx")} {
		if ok, body := attach(d); ok || !strings.Contains(body, "component c0") ||
			!strings.Contains(body, "launching its installed program directly does not") {
			t.Errorf("%s: passed %v, %s; want a refusal naming component c0 and the direct launch", name, ok, body)
		}
	}
}

// TestNoNetwork_LaunchFetches: a fetcher behind env or a shell, a container run and a deno run of a remote
// module download what they start; a program from the image, run directly or through a wrapper, does not.
func TestNoNetwork_LaunchFetches(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		args []string
		want bool
	}{
		{"npx", []string{"-y", "@example/mcp-server@1.4.2"}, true},
		{"/usr/local/bin/uvx", []string{"mcp-server-files"}, true},
		{"npx", []string{"--no-install", "mcp-server-files"}, false},
		{"npx", []string{"--no", "mcp-server-files"}, false},
		{"/usr/bin/npx", []string{"--yes=false", "mcp-server-files"}, false},
		{"npx", []string{"--offline", "-p", "@example/mcp-server@1.4.2", "mcp-server"}, false},
		{"/usr/local/bin/uvx", []string{"--offline", "mcp-server-files"}, false},
		{"/usr/local/bin/uvx", []string{"--from", "mcp-files==1.0", "--offline", "mcp-files"}, false},
		{"npx", []string{"-y", "@example/mcp-server@1.4.2", "--offline"}, true},
		{"npx", []string{"-p", "--offline", "mcp-server"}, true},
		{"/usr/local/bin/uvx", []string{"mcp-server-files", "--offline"}, true},
		{"/usr/bin/env", []string{"npx", "--no-install", "mcp-server-files"}, false},
		{"/usr/bin/env", []string{"npx", "-y", "@example/mcp-server@1.4.2"}, true},
		{"/usr/bin/env", []string{"-i", "PATH=/usr/bin", "uvx", "mcp-server-files"}, true},
		{"/bin/sh", []string{"-c", "uvx mcp-server-files --stdio"}, true},
		{"/bin/bash", []string{"-lc", "npx -y @example/mcp-server@1.4.2"}, true},
		{"/usr/bin/docker", []string{"run", "-i", "--rm", "mcp/files"}, true},
		{"/usr/bin/podman", []string{"--log-level=error", "run", "mcp/files"}, true},
		{"/usr/local/bin/deno", []string{"run", "-A", "npm:@example/mcp-server@1.4.2"}, true},
		{"/usr/local/bin/deno", []string{"run", "-A", "/opt/mcp/server.ts"}, false},
		{"/usr/local/bin/deno", []string{"run", "--config", "/opt/mcp/deno.json", "jsr:@example/mcp@1.0.0"}, true},
		{"/usr/local/bin/deno", []string{"run", "/opt/mcp/local.ts", "https://example.com/resource-id"}, false},
		{"/usr/local/bin/deno", []string{"run", "-A", "/opt/mcp/local.ts", "npm:not-a-module"}, false},
		{"/usr/local/bin/deno", []string{"run", "--lock", "/opt/mcp/deno.lock", "npm:@example/mcp-server@1.4.2"}, true},
		{"/usr/local/bin/deno", []string{"run", "--import", "/opt/mcp/setup.ts", "jsr:@example/mcp@1.0.0"}, true},
		{"/usr/local/bin/deno", []string{"-A", "npm:@example/mcp-server@1.4.2"}, true},
		{"/usr/local/bin/deno", []string{"-A", "/opt/mcp/local.ts", "https://example.com/resource-id"}, false},
		{"/usr/local/bin/deno", []string{"task", "start"}, false},
		{"/usr/bin/npm", []string{"exec", "-y", "@example/mcp-server@1.4.2"}, true},
		{"npm", []string{"x", "@example/mcp-server@1.4.2"}, true},
		{"/usr/local/bin/pnpm", []string{"dlx", "@example/mcp-server@1.4.2"}, true},
		{"/usr/local/bin/yarn", []string{"dlx", "@example/mcp-server@1.4.2"}, true},
		{"/usr/local/bin/bun", []string{"x", "@example/mcp-server@1.4.2"}, true},
		{"/usr/local/bin/uv", []string{"tool", "run", "mcp-server-files"}, true},
		{"/usr/bin/npm", []string{"run", "start"}, false},
		{"/usr/local/bin/uv", []string{"run", "/opt/mcp/server.py"}, false},
		{"/usr/local/bin/bun", []string{"/opt/mcp/server.ts"}, false},
		{"/usr/bin/docker", []string{"exec", "files", "mcp"}, false},
		{"/usr/bin/env", []string{"-u", "NODE_OPTIONS", "npx", "-y", "@example/mcp-server@1.4.2"}, true},
		{"/usr/bin/env", []string{"-C", "/srv", "FOO=1", "uvx", "mcp-server-files"}, true},
		{"/bin/sh", []string{"-c", "exec npx -y @example/mcp-server@1.4.2"}, true},
		{"/bin/sh", []string{"-c", "NODE_ENV=production uvx mcp-server-files"}, true},
		{"/bin/sh", []string{"-c", "cd /srv && npx -y @example/mcp-server@1.4.2"}, true},
		{"/bin/sh", []string{"-c", "cd /srv; uvx mcp-server-files"}, true},
		{"/bin/sh", []string{"-c", "echo npx; exec /usr/local/bin/mcp-files"}, false},
		{"/bin/sh", []string{"-c", ": 'documentation && npx -y example'; exec /usr/local/bin/mcp-files --stdio"}, false},
		{"/bin/sh", []string{"-c", "echo 'a; uvx mcp-server-files' >&2; exec /usr/local/bin/mcp-files"}, false},
		{"/bin/sh", []string{"-c", `echo "see | npx -y example"; exec /usr/local/bin/mcp-files`}, false},
		{"/bin/sh", []string{"-c", `echo done \; npx -y example`}, false},
		{"/bin/sh", []string{"-c", "exec /usr/local/bin/mcp-files 2>&1 # npx; uvx"}, false},
		{"/bin/sh", []string{"-c", "refresh_tools() { :; npx -y example; }; exec /usr/local/bin/mcp-files --stdio"}, false},
		{"/bin/sh", []string{"-c", "function refresh { npx -y example; }; exec /usr/local/bin/mcp-files"}, false},
		{"/bin/sh", []string{"-c", "if false; then npx -y example; fi; exec /usr/local/bin/mcp-files"}, false},
		{"/bin/sh", []string{"-c", "(: npx -y example); exec /usr/local/bin/mcp-files"}, false},
		{"/bin/sh", []string{"-c", "{ echo start; }; exec /usr/local/bin/mcp-files"}, false},
		// declined grammar: judged on the first command; a miss fails safe at launch
		{"/bin/sh", []string{"-c", "export T=$(cat /run/s); exec npx -y @example/mcp-server@1.4.2"}, false},
		// declined grammar: judged on the first command; a miss fails safe at launch
		{"/bin/sh", []string{"-c", `cd "$(dirname "$0")" && npx -y @example/mcp-server@1.4.2`}, false},
		// declined grammar: judged on the first command; a miss fails safe at launch
		{"/bin/sh", []string{"-c", "if [ -f x ]; then :; fi; exec npx -y @example/mcp-server@1.4.2"}, false},
		{"/bin/sh", []string{"-c", "true && npx -y @example/mcp-server@1.4.2"}, true},
		{"/bin/sh", []string{"-c", "echo '(starting)'; exec npx -y @example/mcp-server@1.4.2"}, true},
		{"/bin/sh", []string{"-c", "export A='x y'; exec npx -y @example/mcp-server@1.4.2"}, true},
		{"/bin/sh", []string{"-c", "cd /srv\nuvx mcp-server-files"}, true},
		{"/bin/sh", []string{"-c", "npx -y $(cat /etc/pkg)"}, true},
		{"/bin/sh", []string{"-c", "npx -y 'unclosed"}, true},
		{"/usr/bin/docker", []string{"--context", "c", "run", "mcp/files"}, true},
		{"/usr/bin/docker", []string{"-H", "unix:///run/d.sock", "container", "run", "mcp/files"}, true},
		{"/usr/bin/docker", []string{"--context", "run", "ps"}, false},
		{"/usr/bin/env", []string{"/usr/local/bin/mcp-files"}, false},
		{"/bin/sh", []string{"-c", "/usr/local/bin/mcp-files --stdio"}, false},
		{"/usr/local/bin/mcp-files", nil, false},
		{"/usr/bin/env", []string{"env", "env", "npx", "pkg@1.0.0"}, false},
	} {
		if got := launchFetches(tc.cmd, tc.args, 0); got != tc.want {
			t.Errorf("launchFetches(%s %v) = %v, want %v", tc.cmd, tc.args, got, tc.want)
		}
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
