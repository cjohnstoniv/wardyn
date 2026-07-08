// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/setup"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// setupTestStore is a minimal store.Store for deriveSetupItems tests: it embeds
// the interface (nil — any other method would panic if called) and overrides
// ONLY GetWorkspaceBySource, keyed by (kind, source), which is all
// referencedWorkspaces + the primary-git-repo lookup touch.
type setupTestStore struct {
	store.Store
	byKindSource map[string]types.Workspace
}

func (s setupTestStore) GetWorkspaceBySource(_ context.Context, kind types.WorkspaceKind, source string) (types.Workspace, error) {
	ws, ok := s.byKindSource[string(kind)+"\x00"+source]
	if !ok {
		return types.Workspace{}, store.ErrNotFound
	}
	return ws, nil
}

// setupTestRunner is a minimal runner.Runner for setupBackendItem tests: it
// embeds the interface (nil — any other method would panic if called) and
// overrides ONLY Capabilities, mirroring setupTestStore above.
type setupTestRunner struct {
	runner.Runner
	caps    runner.Capabilities
	capsErr error
}

func (r setupTestRunner) Capabilities(context.Context) (runner.Capabilities, error) {
	return r.caps, r.capsErr
}

func newSetupTestServer(workspaces ...types.Workspace) *Server {
	byKindSource := map[string]types.Workspace{}
	for _, ws := range workspaces {
		byKindSource[string(ws.Kind)+"\x00"+ws.Source] = ws
	}
	return &Server{cfg: Config{Store: setupTestStore{byKindSource: byKindSource}}}
}

func apiKeyGrant(host, secretName string) types.GrantSpec {
	scope, _ := json.Marshal(map[string]string{"host": host, "header": "x-api-key", "format": "%s", "secret_name": secretName})
	return types.GrantSpec{Kind: types.GrantAPIKey, Scope: scope}
}

func gitPATGrant(host, secretName string) types.GrantSpec {
	scope, _ := json.Marshal(map[string]string{"host": host, "secret_name": secretName})
	return types.GrantSpec{Kind: types.GrantGitPAT, Scope: scope}
}

func githubTokenGrant() types.GrantSpec {
	scope, _ := json.Marshal(map[string]any{"repos": []string{"octocat/Hello-World"}, "permissions": map[string]string{"contents": "read"}})
	return types.GrantSpec{Kind: types.GrantGitHubToken, Scope: scope}
}

func findItem(items []SetupItem, id string) (SetupItem, bool) {
	for _, it := range items {
		if it.ID == id {
			return it, true
		}
	}
	return SetupItem{}, false
}

// ── llm_access ──────────────────────────────────────────────────────────────

func TestDeriveSetupItems_LLMAccessReusesVerdict(t *testing.T) {
	srv := newSetupTestServer()
	run := composer.RunInput{Agent: "claude-code", Repo: "ephemeral"}

	// Provisioned: satisfied, no fix.
	items := srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), &composeLLMAccess{Provisioned: true, Note: "ok"}, nil, composeSubscriptionState{})
	it, ok := findItem(items, "llm_access:claude-code")
	if !ok {
		t.Fatal("expected an llm_access item")
	}
	if it.Status != "satisfied" || it.Fix != nil {
		t.Errorf("provisioned llm_access = %+v, want satisfied with no fix", it)
	}

	// Missing: destructive-relevant "missing" status + add_secret fix naming the
	// agent's provider secret.
	items = srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), &composeLLMAccess{Provisioned: false, Note: "no model access"}, nil, composeSubscriptionState{})
	it, ok = findItem(items, "llm_access:claude-code")
	if !ok {
		t.Fatal("expected an llm_access item")
	}
	if it.Status != "missing" || it.Fix == nil || it.Fix.Action != "add_secret" || it.Fix.SecretName != "anthropic-api-key" {
		t.Errorf("unprovisioned llm_access = %+v, want missing + add_secret(anthropic-api-key)", it)
	}

	// Nil llmAccess (non-LLM agent): no row at all.
	items = srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), nil, nil, composeSubscriptionState{})
	if _, ok := findItem(items, "llm_access:claude-code"); ok {
		t.Error("nil llmAccess must produce no llm_access row")
	}
}

// ── secret ──────────────────────────────────────────────────────────────────

func TestDeriveSetupItems_SecretPresentAbsent(t *testing.T) {
	srv := newSetupTestServer()
	run := composer.RunInput{Agent: "claude-code", Repo: "ephemeral"}
	spec := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{
		apiKeyGrant("api.anthropic.com", "anthropic-api-key"),
		gitPATGrant("dev.azure.com", "ado-pat"),
	}}

	items := srv.deriveSetupItems(context.Background(), run, spec, secretsWith("anthropic-api-key"), nil, nil, composeSubscriptionState{})

	present, ok := findItem(items, "secret:anthropic-api-key")
	if !ok || present.Status != "satisfied" || present.Fix != nil {
		t.Errorf("present secret = %+v, want satisfied with no fix", present)
	}
	absent, ok := findItem(items, "secret:ado-pat")
	if !ok || absent.Status != "missing" || absent.Fix == nil || absent.Fix.Action != "add_secret" || absent.Fix.SecretName != "ado-pat" {
		t.Errorf("absent secret = %+v, want missing + add_secret(ado-pat)", absent)
	}
}

// A git_pat and an api_key grant naming the SAME secret must produce ONE row,
// not two.
func TestDeriveSetupItems_SecretDedupsByName(t *testing.T) {
	srv := newSetupTestServer()
	run := composer.RunInput{Agent: "claude-code", Repo: "ephemeral"}
	spec := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{
		apiKeyGrant("api.anthropic.com", "shared-secret"),
		gitPATGrant("dev.azure.com", "shared-secret"),
	}}
	items := srv.deriveSetupItems(context.Background(), run, spec, secretsWith("shared-secret"), nil, nil, composeSubscriptionState{})
	n := 0
	for _, it := range items {
		if it.Kind == "secret" && it.ID == "secret:shared-secret" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("expected exactly one secret row for a name shared by two grants, got %d", n)
	}
}

// ── workspace ───────────────────────────────────────────────────────────────

func TestDeriveSetupItems_WorkspaceStatuses(t *testing.T) {
	ready := types.Workspace{ID: uuid.New(), Kind: types.WorkspaceKindLocalDir, Source: "/home/me/ready", Name: "ready", Status: types.WorkspaceReady}
	pending := types.Workspace{ID: uuid.New(), Kind: types.WorkspaceKindLocalDir, Source: "/home/me/pending", Name: "pending", Status: types.WorkspacePendingScan}
	errored := types.Workspace{ID: uuid.New(), Kind: types.WorkspaceKindLocalDir, Source: "/home/me/errored", Name: "errored", Status: types.WorkspaceError}
	srv := newSetupTestServer(ready, pending, errored)
	ro := true
	spec := types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{
		{Source: ready.Source, Target: "/home/agent/work", ReadOnly: &ro},
		{Source: pending.Source, Target: "/home/agent/work/pending", ReadOnly: &ro},
		{Source: errored.Source, Target: "/home/agent/work/errored", ReadOnly: &ro},
	}}
	run := composer.RunInput{Agent: "claude-code", Repo: "local:ready"}

	items := srv.deriveSetupItems(context.Background(), run, spec, secretsWith(), nil, nil, composeSubscriptionState{})

	got, ok := findItem(items, "workspace:"+ready.ID.String())
	if !ok || got.Status != "satisfied" {
		t.Errorf("ready workspace = %+v, want satisfied", got)
	}
	got, ok = findItem(items, "workspace:"+pending.ID.String())
	if !ok || got.Status != "unverified" || got.Fix == nil || got.Fix.Action != "scan_workspace" || got.Fix.WorkspaceID != pending.ID.String() {
		t.Errorf("pending workspace = %+v, want unverified + scan_workspace(%s)", got, pending.ID)
	}
	got, ok = findItem(items, "workspace:"+errored.ID.String())
	if !ok || got.Status != "unverified" || got.Fix == nil || got.Fix.Action != "scan_workspace" {
		t.Errorf("errored workspace = %+v, want unverified + scan_workspace", got)
	}
}

// The primary GIT workspace never lands in spec.WorkspaceRepos (applyWorkspaces
// only sets run.Repo for it) — deriveSetupItems must still surface it by
// looking it up directly via run.Repo.
func TestDeriveSetupItems_PrimaryGitWorkspaceResolvedFromRunRepo(t *testing.T) {
	primary := types.Workspace{ID: uuid.New(), Kind: types.WorkspaceKindRepo, Source: "octocat/Hello-World", Name: "Hello-World", Status: types.WorkspaceReady}
	srv := newSetupTestServer(primary)
	run := composer.RunInput{Agent: "claude-code", Repo: "octocat/Hello-World"}
	// No WorkspaceMounts/WorkspaceRepos: applyWorkspaces never adds the PRIMARY
	// git repo to WorkspaceRepos, so referencedWorkspaces alone would see nothing.
	items := srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), nil, nil, composeSubscriptionState{})
	got, ok := findItem(items, "workspace:"+primary.ID.String())
	if !ok || got.Status != "satisfied" {
		t.Errorf("primary git workspace = %+v (ok=%v), want a satisfied row resolved from run.Repo", got, ok)
	}
}

// The synthetic run.Repo values applyWorkspaces sets for the OTHER two
// workspace kinds ("local:<dir>", "ephemeral") must NEVER be treated as a real
// repo slug and looked up.
func TestDeriveSetupItems_SyntheticRunRepoGuarded(t *testing.T) {
	srv := newSetupTestServer() // empty store: any lookup at all would 404 harmlessly, but assert none is attempted via len(items)
	for _, repo := range []string{"local:proj", "ephemeral"} {
		run := composer.RunInput{Agent: "claude-code", Repo: repo}
		items := srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), nil, nil, composeSubscriptionState{})
		for _, it := range items {
			if it.Kind == "workspace" {
				t.Errorf("repo=%q must not produce a workspace row (synthetic value), got %+v", repo, it)
			}
		}
	}
}

// ── repo_credential ─────────────────────────────────────────────────────────

func TestDeriveSetupItems_RepoCredentialGitHubTokenUnverified(t *testing.T) {
	srv := newSetupTestServer()
	run := composer.RunInput{Agent: "claude-code", Repo: "octocat/Hello-World"}
	spec := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{githubTokenGrant()}}
	items := srv.deriveSetupItems(context.Background(), run, spec, secretsWith(), nil, nil, composeSubscriptionState{})
	got, ok := findItem(items, "repo_credential:github_token")
	if !ok || got.Status != "unverified" || got.Fix != nil {
		t.Errorf("github_token repo_credential = %+v (ok=%v), want unverified with no fix (mint-time brokered)", got, ok)
	}
}

func TestDeriveSetupItems_RepoCredentialGitPATPresentAbsent(t *testing.T) {
	srv := newSetupTestServer()
	run := composer.RunInput{Agent: "claude-code", Repo: "local:proj"}
	spec := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{gitPATGrant("dev.azure.com", "ado-pat")}}

	present := srv.deriveSetupItems(context.Background(), run, spec, secretsWith("ado-pat"), nil, nil, composeSubscriptionState{})
	got, ok := findItem(present, "repo_credential:git_pat:dev.azure.com")
	if !ok || got.Status != "satisfied" || got.Fix != nil {
		t.Errorf("git_pat repo_credential w/ secret = %+v, want satisfied", got)
	}

	absent := srv.deriveSetupItems(context.Background(), run, spec, secretsWith(), nil, nil, composeSubscriptionState{})
	got, ok = findItem(absent, "repo_credential:git_pat:dev.azure.com")
	if !ok || got.Status != "missing" || got.Fix == nil || got.Fix.SecretName != "ado-pat" {
		t.Errorf("git_pat repo_credential w/o secret = %+v, want missing + add_secret(ado-pat)", got)
	}
}

// ── egress (dropped) ─────────────────────────────────────────────────────────

func TestDeriveSetupItems_EgressDropped(t *testing.T) {
	srv := newSetupTestServer()
	run := composer.RunInput{Agent: "claude-code", Repo: "ephemeral"}
	items := srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), nil, []string{"evil.example.com"}, composeSubscriptionState{})
	got, ok := findItem(items, "egress:dropped:evil.example.com")
	if !ok || got.Status != "missing" || got.Fix == nil || got.Fix.Action != "none" {
		t.Errorf("dropped-domain item = %+v (ok=%v), want missing + fix action \"none\"", got, ok)
	}
	// No drops => no rows.
	items = srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), nil, nil, composeSubscriptionState{})
	for _, it := range items {
		if it.Kind == "egress" && it.ID != "egress:workspace" {
			t.Errorf("expected no dropped-egress rows with an empty diff, got %+v", it)
		}
	}
}

// ── egress (workspace, informational) ───────────────────────────────────────

func workspaceWithProfile(t *testing.T, egressDomains ...string) types.Workspace {
	t.Helper()
	profile, err := json.Marshal(map[string]any{"egress_domains": egressDomains, "confidence": "high"})
	if err != nil {
		t.Fatalf("marshal profile: %v", err)
	}
	return types.Workspace{
		ID: uuid.New(), Kind: types.WorkspaceKindLocalDir, Source: "/home/me/proj", Name: "proj",
		Status: types.WorkspaceReady, Profile: profile,
	}
}

func TestDeriveSetupItems_EgressWorkspaceInfoAlwaysSatisfiedAndCopiesDomains(t *testing.T) {
	ws := workspaceWithProfile(t, "registry.npmjs.org", "pypi.org")
	srv := newSetupTestServer(ws)
	ro := true
	spec := types.RunPolicySpec{
		AllowedDomains:  []string{"github.com"},
		WorkspaceMounts: []types.WorkspaceMount{{Source: ws.Source, Target: "/home/agent/work", ReadOnly: &ro}},
	}
	run := composer.RunInput{Agent: "claude-code", Repo: "local:proj"}

	items := srv.deriveSetupItems(context.Background(), run, spec, secretsWith(), nil, nil, composeSubscriptionState{})
	got, ok := findItem(items, "egress:workspace")
	if !ok || got.Status != "satisfied" {
		t.Fatalf("workspace-egress info row = %+v (ok=%v), want satisfied", got, ok)
	}
	// The real spec passed in must be untouched (unionWorkspaceEgress mutates in
	// place — deriveSetupItems must operate on a COPY).
	if len(spec.AllowedDomains) != 1 || spec.AllowedDomains[0] != "github.com" {
		t.Errorf("input spec.AllowedDomains mutated: %v, want unchanged [github.com]", spec.AllowedDomains)
	}
}

// No referenced workspaces => no informational row at all (nothing to union).
func TestDeriveSetupItems_EgressWorkspaceInfoAbsentWithNoWorkspaces(t *testing.T) {
	srv := newSetupTestServer()
	run := composer.RunInput{Agent: "claude-code", Repo: "ephemeral"}
	items := srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), nil, nil, composeSubscriptionState{})
	if _, ok := findItem(items, "egress:workspace"); ok {
		t.Error("expected no egress:workspace row when there are no referenced workspaces")
	}
}

// ── backend (F1) ─────────────────────────────────────────────────────────────

// No explicit class anywhere (empty run class, empty policy floor): nothing to
// check, no row — mirrors every OTHER test in this file (none set a
// ConfinementClass), so their assertions stay valid untouched by this addition.
func TestDeriveSetupItems_BackendAbsentWithNoExplicitClass(t *testing.T) {
	srv := newSetupTestServer()
	srv.cfg.Runner = setupTestRunner{caps: runner.Capabilities{ConfinementClasses: []types.ConfinementClass{types.CC1}}}
	run := composer.RunInput{Agent: "claude-code", Repo: "ephemeral"}
	items := srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), nil, nil, composeSubscriptionState{})
	for _, it := range items {
		if it.Kind == "backend" {
			t.Errorf("expected no backend row with no explicit class, got %+v", it)
		}
	}
}

func TestDeriveSetupItems_BackendSatisfied(t *testing.T) {
	srv := newSetupTestServer()
	srv.cfg.Runner = setupTestRunner{caps: runner.Capabilities{
		ConfinementClasses: []types.ConfinementClass{types.CC1, types.CC2},
		Resolved:           map[types.ConfinementClass]string{types.CC2: "oci/runsc"},
	}}
	run := composer.RunInput{Agent: "claude-code", Repo: "ephemeral", ConfinementClass: string(types.CC2)}
	items := srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), nil, nil, composeSubscriptionState{})
	got, ok := findItem(items, "backend:CC2")
	if !ok || got.Status != "satisfied" || got.Fix != nil || !strings.Contains(got.Detail, "oci/runsc") {
		t.Errorf("satisfied backend item = %+v (ok=%v), want satisfied, no fix, substrate in detail", got, ok)
	}
}

// Falls back to the CLAMPED POLICY's floor when the run's OWN class is empty
// (ClampRunConfinement leaves an empty run class alone when the floor rank is
// 0 — an empty policy floor is genuinely "nothing required", but a NON-empty
// one still describes what this run needs).
func TestDeriveSetupItems_BackendFallsBackToPolicyFloor(t *testing.T) {
	srv := newSetupTestServer()
	srv.cfg.Runner = setupTestRunner{caps: runner.Capabilities{ConfinementClasses: []types.ConfinementClass{types.CC1}}}
	run := composer.RunInput{Agent: "claude-code", Repo: "ephemeral"} // no ConfinementClass
	spec := types.RunPolicySpec{MinConfinementClass: types.CC2}
	items := srv.deriveSetupItems(context.Background(), run, spec, secretsWith(), nil, nil, composeSubscriptionState{})
	got, ok := findItem(items, "backend:CC2")
	if !ok || got.Status != "missing" {
		t.Errorf("policy-floor-derived backend item = %+v (ok=%v), want a missing CC2 row", got, ok)
	}
}

func TestDeriveSetupItems_BackendCC2NeedsSetup(t *testing.T) {
	srv := newSetupTestServer()
	srv.cfg.Runner = setupTestRunner{caps: runner.Capabilities{ConfinementClasses: []types.ConfinementClass{types.CC1}}}
	run := composer.RunInput{Agent: "claude-code", Repo: "ephemeral", ConfinementClass: string(types.CC2)}
	items := srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), nil, nil, composeSubscriptionState{})
	got, ok := findItem(items, "backend:CC2")
	if !ok || got.Status != "missing" || got.Fix == nil || got.Fix.Action != "none" || !strings.Contains(got.Detail, "wardyn setup wall") {
		t.Errorf("CC2-unavailable backend item = %+v (ok=%v), want missing + fix action \"none\" + fixable wall guidance", got, ok)
	}
}

// CC3 unavailable: fixable-here (needs setup) vs not-fixable-on-this-host (no
// /dev/kvm) is a REAL hardware probe (internal/setup, commit 74b4d0a) — mirror
// its live result rather than assuming this test host's hardware either way.
func TestDeriveSetupItems_BackendCC3SplitsOnKVM(t *testing.T) {
	srv := newSetupTestServer()
	srv.cfg.Runner = setupTestRunner{caps: runner.Capabilities{ConfinementClasses: []types.ConfinementClass{types.CC1, types.CC2}}}
	run := composer.RunInput{Agent: "claude-code", Repo: "ephemeral", ConfinementClass: string(types.CC3)}
	items := srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), nil, nil, composeSubscriptionState{})
	got, ok := findItem(items, "backend:CC3")
	if !ok || got.Status != "missing" || got.Fix == nil || got.Fix.Action != "none" {
		t.Fatalf("CC3-unavailable backend item = %+v (ok=%v), want missing + fix action \"none\"", got, ok)
	}
	if setup.DetectPlatform().KVM {
		if !strings.Contains(got.Detail, "wardyn setup vault") {
			t.Errorf("KVM-capable host: detail = %q, want the fixable `wardyn setup vault` guidance", got.Detail)
		}
	} else if !strings.Contains(got.Detail, "/dev/kvm") {
		t.Errorf("KVM-less host: detail = %q, want the not-fixable /dev/kvm reason", got.Detail)
	}
}

func TestDeriveSetupItems_BackendNoRunner(t *testing.T) {
	srv := newSetupTestServer()
	run := composer.RunInput{Agent: "claude-code", Repo: "ephemeral", ConfinementClass: string(types.CC1)}
	items := srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), nil, nil, composeSubscriptionState{})
	got, ok := findItem(items, "backend:CC1")
	if !ok || got.Status != "missing" || got.Fix == nil || got.Fix.Action != "none" {
		t.Errorf("no-runner backend item = %+v (ok=%v), want missing + fix action \"none\"", got, ok)
	}
}

func TestDeriveSetupItems_BackendCapabilitiesProbeError(t *testing.T) {
	srv := newSetupTestServer()
	srv.cfg.Runner = setupTestRunner{capsErr: errors.New("docker daemon unreachable")}
	run := composer.RunInput{Agent: "claude-code", Repo: "ephemeral", ConfinementClass: string(types.CC1)}
	items := srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), nil, nil, composeSubscriptionState{})
	got, ok := findItem(items, "backend:CC1")
	if !ok || got.Status != "unverified" || !strings.Contains(got.Detail, "docker daemon unreachable") {
		t.Errorf("probe-error backend item = %+v (ok=%v), want unverified carrying the probe error", got, ok)
	}
}

// ── config_pair (F2) ─────────────────────────────────────────────────────────

func TestDeriveSetupItems_ConfigPairSubscriptionMount(t *testing.T) {
	srv := newSetupTestServer()
	run := composer.RunInput{Agent: "claude-code", Repo: "ephemeral"}

	// Not requested this round: no row at all.
	items := srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), nil, nil, composeSubscriptionState{})
	if _, ok := findItem(items, "config_pair:use_subscription:claude_cred_mount"); ok {
		t.Error("expected no config_pair row when subscription mode wasn't requested")
	}

	// Requested and applyLLMCredMount actually injected the mounts: satisfied,
	// carrying its (possibly empty) reused warning verbatim.
	items = srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), nil, nil,
		composeSubscriptionState{Requested: true, Injected: true})
	it, ok := findItem(items, "config_pair:use_subscription:claude_cred_mount")
	if !ok || it.Status != "satisfied" || it.Fix == nil || it.Fix.Action != "none" {
		t.Errorf("injected config_pair = %+v (ok=%v), want satisfied + fix action \"none\"", it, ok)
	}

	// Requested but NOT applied (the silent-degrade case this item exists to
	// surface): missing, and the Detail is applyLLMCredMount's OWN reason —
	// reused verbatim, never reworded.
	reason := "subscription mode requested but the operator policy does not bless a Claude credential mount"
	items = srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), nil, nil,
		composeSubscriptionState{Requested: true, Injected: false, Warnings: []string{reason}})
	it, ok = findItem(items, "config_pair:use_subscription:claude_cred_mount")
	if !ok || it.Status != "missing" || it.Detail != reason {
		t.Errorf("degraded config_pair = %+v (ok=%v), want missing + detail == reused reason %q", it, ok, reason)
	}
}

// ── residency (F3) ───────────────────────────────────────────────────────────

func TestDeriveSetupItems_Residency(t *testing.T) {
	srv := newSetupTestServer()

	// llm_access, api-key mode (no Claude cred mount in the FINAL spec): proxy_injected.
	run := composer.RunInput{Agent: "claude-code", Repo: "ephemeral"}
	items := srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(),
		&composeLLMAccess{Provisioned: true, Note: "ok"}, nil, composeSubscriptionState{})
	it, ok := findItem(items, "llm_access:claude-code")
	if !ok || it.Residency != "proxy_injected" {
		t.Errorf("api-key llm_access residency = %+v (ok=%v), want proxy_injected", it, ok)
	}
	if strings.Contains(it.Detail, "fetch()") {
		t.Errorf("api-key llm_access must NOT carry the Node-proxy-env gap note, detail = %q", it.Detail)
	}

	// llm_access, subscription mode (the FINAL spec carries the Claude cred
	// mount): resident_mount, plus the one-sentence Node<24 fetch()/HTTP_PROXY
	// gap note (F4) since this is the ONE path where the CLI tunnels a direct
	// api.anthropic.com request through the sandbox's proxy env.
	subSpec := types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{{Target: claudeCredTarget}}}
	items = srv.deriveSetupItems(context.Background(), run, subSpec, secretsWith(),
		&composeLLMAccess{Provisioned: true, Note: "ok"}, nil, composeSubscriptionState{})
	it, ok = findItem(items, "llm_access:claude-code")
	if !ok || it.Residency != "resident_mount" {
		t.Errorf("subscription llm_access residency = %+v (ok=%v), want resident_mount", it, ok)
	}
	if !strings.Contains(it.Detail, "NODE_USE_ENV_PROXY") {
		t.Errorf("subscription llm_access must carry the Node-proxy-env gap note, detail = %q", it.Detail)
	}

	// secret: api_key-sourced row is proxy_injected; git_pat-sourced row carries
	// no residency of its own (its repo_credential sibling does).
	secretSpec := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{
		apiKeyGrant("api.anthropic.com", "anthropic-api-key"),
		gitPATGrant("dev.azure.com", "ado-pat"),
	}}
	items = srv.deriveSetupItems(context.Background(), run, secretSpec, secretsWith("anthropic-api-key", "ado-pat"), nil, nil, composeSubscriptionState{})
	it, ok = findItem(items, "secret:anthropic-api-key")
	if !ok || it.Residency != "proxy_injected" {
		t.Errorf("api_key secret residency = %+v (ok=%v), want proxy_injected", it, ok)
	}
	it, ok = findItem(items, "secret:ado-pat")
	if !ok || it.Residency != "" {
		t.Errorf("git_pat secret residency = %+v (ok=%v), want empty (belongs to its repo_credential sibling)", it, ok)
	}

	// repo_credential: both github_token and git_pat sub-cases are brokered_mint.
	repoSpec := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{githubTokenGrant(), gitPATGrant("dev.azure.com", "ado-pat")}}
	items = srv.deriveSetupItems(context.Background(), run, repoSpec, secretsWith("ado-pat"), nil, nil, composeSubscriptionState{})
	it, ok = findItem(items, "repo_credential:github_token")
	if !ok || it.Residency != "brokered_mint" {
		t.Errorf("github_token repo_credential residency = %+v (ok=%v), want brokered_mint", it, ok)
	}
	it, ok = findItem(items, "repo_credential:git_pat:dev.azure.com")
	if !ok || it.Residency != "brokered_mint" {
		t.Errorf("git_pat repo_credential residency = %+v (ok=%v), want brokered_mint", it, ok)
	}
}

// ── workspace_secret ────────────────────────────────────────────────────────

func needsWorkspace(name, source string, p workspacescan.WorkspaceProfile) types.Workspace {
	return types.Workspace{
		ID: uuid.New(), Name: name, Kind: types.WorkspaceKindLocalDir, Source: source,
		Status: types.WorkspaceReady, Profile: mustJSON(p),
	}
}

func mountSpec(source string) types.RunPolicySpec {
	return types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{{Source: source, Target: "/home/agent/work"}}}
}

// A workspace-declared REQUIRED secret surfaces as a workspace_secret row —
// advisory kind (never "secret", which the UI styles as launch-blocking),
// grounded to a storable name, satisfied/missing against presentSecrets.
// Optional needs produce NO checklist row (needs panel only).
func TestDeriveSetupItems_WorkspaceSecrets(t *testing.T) {
	ws := needsWorkspace("app", "/w", workspacescan.WorkspaceProfile{
		RequiredSecrets: []workspacescan.SecretNeed{
			{Name: "DATABASE_URL", Kind: "database"},
			{Name: "OIDC_CLIENT_SECRET", Kind: "oidc"},
			{Name: "STRIPE_SECRET_KEY", Kind: "stripe", Optional: true},
		},
		Confidence: "high", Source: "deterministic",
	})
	srv := newSetupTestServer(ws)
	run := composer.RunInput{Agent: "claude-code", Repo: "ephemeral"}

	items := srv.deriveSetupItems(context.Background(), run, mountSpec("/w"), secretsWith("database-url"), nil, nil, composeSubscriptionState{})

	sat, ok := findItem(items, "workspace_secret:database-url")
	if !ok || sat.Kind != "workspace_secret" || sat.Status != "satisfied" || sat.Fix != nil {
		t.Errorf("present workspace secret = %+v, want satisfied workspace_secret with no fix", sat)
	}
	miss, ok := findItem(items, "workspace_secret:oidc-client-secret")
	if !ok || miss.Status != "missing" || miss.Fix == nil || miss.Fix.Action != "add_secret" || miss.Fix.SecretName != "oidc-client-secret" {
		t.Errorf("absent workspace secret = %+v, want missing + add_secret(oidc-client-secret)", miss)
	}
	if !strings.Contains(miss.RequiredBy, "untrusted") {
		t.Errorf("workspace_secret RequiredBy must carry the untrusted-provenance label: %q", miss.RequiredBy)
	}
	if _, ok := findItem(items, "workspace_secret:stripe-secret-key"); ok {
		t.Error("optional needs must not produce checklist rows")
	}
}

// Row-per-key is survey-proven UX poison: required needs beyond the cap
// collapse into one summary row.
func TestDeriveSetupItems_WorkspaceSecretsCapped(t *testing.T) {
	var needs []workspacescan.SecretNeed
	for _, n := range []string{"A_KEY", "B_KEY", "C_KEY", "D_KEY", "E_KEY", "F_KEY", "G_KEY", "H_KEY"} {
		needs = append(needs, workspacescan.SecretNeed{Name: n})
	}
	ws := needsWorkspace("app", "/w", workspacescan.WorkspaceProfile{
		RequiredSecrets: needs, Confidence: "high", Source: "deterministic",
	})
	srv := newSetupTestServer(ws)
	items := srv.deriveSetupItems(context.Background(), composer.RunInput{Agent: "claude-code", Repo: "ephemeral"},
		mountSpec("/w"), secretsWith(), nil, nil, composeSubscriptionState{})

	rows := 0
	for _, it := range items {
		if it.Kind == "workspace_secret" && it.ID != "workspace_secret:more" {
			rows++
		}
	}
	if rows != maxWorkspaceSecretRows {
		t.Errorf("per-secret rows = %d, want cap %d", rows, maxWorkspaceSecretRows)
	}
	more, ok := findItem(items, "workspace_secret:more")
	if !ok || more.Status != "unverified" || more.Fix != nil || !strings.Contains(more.Label, "+3 more") {
		t.Errorf("summary row = %+v (ok=%v), want unverified '+3 more' with no fix", more, ok)
	}
}

// The workspace row's Detail carries the profile's service needs and the
// secret-file exposure warning; the egress row shows suggested hosts as
// explicitly NOT auto-allowed and never mutates the proposal's spec.
func TestDeriveSetupItems_WorkspaceNeedsDetails(t *testing.T) {
	ws := needsWorkspace("app", "/w", workspacescan.WorkspaceProfile{
		ServicesNeeded:     []string{"postgres", "redis"},
		SecretFilesPresent: []string{"backend/.env"},
		EgressDomains:      []string{"registry.npmjs.org"},
		SuggestedEgress:    []string{"evil.example.com"},
		Confidence:         "high", Source: "deterministic",
	})
	srv := newSetupTestServer(ws)
	spec := mountSpec("/w")
	items := srv.deriveSetupItems(context.Background(), composer.RunInput{Agent: "claude-code", Repo: "ephemeral"},
		spec, secretsWith(), nil, nil, composeSubscriptionState{})

	wsRow, ok := findItem(items, "workspace:"+ws.ID.String())
	if !ok || !strings.Contains(wsRow.Detail, "postgres, redis") || !strings.Contains(wsRow.Detail, "backend/.env") {
		t.Errorf("workspace row detail = %q, want services + secret-file warning", wsRow.Detail)
	}
	eg, ok := findItem(items, "egress:workspace")
	if !ok {
		t.Fatal("expected the workspace egress row")
	}
	if !strings.Contains(eg.Detail, "registry.npmjs.org") {
		t.Errorf("egress detail missing the launch-union preview: %q", eg.Detail)
	}
	if !strings.Contains(eg.Detail, "evil.example.com") || !strings.Contains(eg.Detail, "NOT auto-allowed") {
		t.Errorf("egress detail must show suggested hosts as not auto-allowed: %q", eg.Detail)
	}
	if len(spec.AllowedDomains) != 0 {
		t.Errorf("deriveSetupItems mutated the proposal's AllowedDomains: %v", spec.AllowedDomains)
	}
}
