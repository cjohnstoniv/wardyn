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
// ONLY ListWorkspaces, which is what referencedWorkspaces + the primary-repo
// lookup in deriveSetupItems both build their (kind,source)->workspace index
// from (workspace_refs.go's indexWorkspacesBySource, scanning every
// workspace's Sources) now that a workspace is a composition rather than one
// row per kind+source.
type setupTestStore struct {
	store.Store
	all []types.Workspace
}

func (s setupTestStore) ListWorkspaces(context.Context) ([]types.Workspace, error) {
	return s.all, nil
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
	return &Server{cfg: Config{Store: setupTestStore{all: workspaces}}}
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

func secretsWith(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
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
	items := srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), &composeLLMAccess{Provisioned: true, Note: "ok"})
	it, ok := findItem(items, "llm_access:claude-code")
	if !ok {
		t.Fatal("expected an llm_access item")
	}
	if it.Status != "satisfied" || it.Fix != nil {
		t.Errorf("provisioned llm_access = %+v, want satisfied with no fix", it)
	}

	// Missing: destructive-relevant "missing" status + add_secret fix naming the
	// agent's provider secret.
	items = srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), &composeLLMAccess{Provisioned: false, Note: "no model access"})
	it, ok = findItem(items, "llm_access:claude-code")
	if !ok {
		t.Fatal("expected an llm_access item")
	}
	if it.Status != "missing" || it.Fix == nil || it.Fix.Action != "add_secret" || it.Fix.SecretName != "anthropic-api-key" {
		t.Errorf("unprovisioned llm_access = %+v, want missing + add_secret(anthropic-api-key)", it)
	}

	// Nil llmAccess (non-LLM agent): no row at all.
	items = srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), nil)
	if _, ok := findItem(items, "llm_access:claude-code"); ok {
		t.Error("nil llmAccess must produce no llm_access row")
	}
}

// TestDeriveSetupItems_LLMAccessFixNamesTheRunsActualGrantSecret is
// W15-W15b-composer-pipeline-6: an integration-bound run's api_key grant can
// carry a NON-convention secret name (applyIntegrationCreds grants the
// integration's own secret, e.g. via its DisplayName), not the provider
// convention default. The "add_secret" fix used to always name the
// convention secret (anthropic-api-key) regardless — an operator who added
// THAT secret would see the checklist go green while the run still
// authenticates through the integration's own (still-missing) secret,
// unaffected by what they just added.
func TestDeriveSetupItems_LLMAccessFixNamesTheRunsActualGrantSecret(t *testing.T) {
	srv := newSetupTestServer()
	run := composer.RunInput{Agent: "claude-code", Repo: "ephemeral"}
	spec := types.RunPolicySpec{
		EligibleGrants: []types.GrantSpec{{
			Kind: types.GrantAPIKey,
			Scope: mustJSON(map[string]string{
				"host": "api.anthropic.com", "header": "x-api-key", "format": "%s",
				"secret_name": "acme-integration-anthropic-key",
			}),
		}},
	}
	items := srv.deriveSetupItems(context.Background(), run, spec, secretsWith(), &composeLLMAccess{Provisioned: false, Note: "no model access"})
	it, ok := findItem(items, "llm_access:claude-code")
	if !ok {
		t.Fatal("expected an llm_access item")
	}
	if it.Fix == nil || it.Fix.SecretName != "acme-integration-anthropic-key" {
		t.Errorf("fix = %+v, want add_secret naming the run's ACTUAL grant secret (acme-integration-anthropic-key), "+
			"not the provider convention default", it.Fix)
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

	items := srv.deriveSetupItems(context.Background(), run, spec, secretsWith("anthropic-api-key"), nil)

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
	items := srv.deriveSetupItems(context.Background(), run, spec, secretsWith("shared-secret"), nil)
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
	readyPath, pendingPath, erroredPath := "/home/me/ready", "/home/me/pending", "/home/me/errored"
	ready := types.Workspace{ID: uuid.New(), Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: readyPath}}, Name: "ready", Status: types.WorkspaceScanned}
	pending := types.Workspace{ID: uuid.New(), Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: pendingPath}}, Name: "pending", Status: types.WorkspacePendingScan}
	errored := types.Workspace{ID: uuid.New(), Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: erroredPath}}, Name: "errored", Status: types.WorkspaceError}
	srv := newSetupTestServer(ready, pending, errored)
	ro := true
	spec := types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{
		{Source: readyPath, Target: "/home/agent/work", ReadOnly: &ro},
		{Source: pendingPath, Target: "/home/agent/work/pending", ReadOnly: &ro},
		{Source: erroredPath, Target: "/home/agent/work/errored", ReadOnly: &ro},
	}}
	run := composer.RunInput{Agent: "claude-code", Repo: "local:ready"}

	items := srv.deriveSetupItems(context.Background(), run, spec, secretsWith(), nil)

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

// applyWorkspaces (compose.go) now puts the PRIMARY git repo into
// spec.WorkspaceRepos too (not just run.Repo) — deriveSetupItems must surface
// it through the ORDINARY referencedWorkspaces resolution, with no special
// run.Repo-keyed lookup involved.
func TestDeriveSetupItems_PrimaryGitWorkspaceResolvedFromWorkspaceRepos(t *testing.T) {
	primary := types.Workspace{
		ID:      uuid.New(),
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "octocat/Hello-World"}},
		Name:    "Hello-World", Status: types.WorkspaceScanned,
	}
	srv := newSetupTestServer(primary)
	run := composer.RunInput{Agent: "claude-code", Repo: "octocat/Hello-World"}
	// Mirrors what the FIXED applyWorkspaces now produces for a git-primary
	// selection: the repo lands in WorkspaceRepos, not just the run.Repo label.
	spec := types.RunPolicySpec{WorkspaceRepos: []types.WorkspaceRepo{{Repo: "octocat/Hello-World"}}}
	items := srv.deriveSetupItems(context.Background(), run, spec, secretsWith(), nil)
	got, ok := findItem(items, "workspace:"+primary.ID.String())
	if !ok || got.Status != "satisfied" {
		t.Errorf("primary git workspace = %+v (ok=%v), want a satisfied row resolved from spec.WorkspaceRepos", got, ok)
	}
}

// One derivation, not two: deriveSetupItems must trust spec.WorkspaceRepos/
// WorkspaceMounts ALONE for its workspace rows — never a fallback keyed on
// run.Repo (the fixup this pins the removal of). A bare spec must show no
// workspace row even when run.Repo names a slug that WOULD resolve if such a
// fallback still existed — the exact regression a resurrected fixup would
// reintroduce.
func TestDeriveSetupItems_WorkspaceRowsComeOnlyFromSpec(t *testing.T) {
	primary := types.Workspace{
		ID:      uuid.New(),
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "octocat/Hello-World"}},
		Name:    "Hello-World", Status: types.WorkspaceScanned,
	}
	srv := newSetupTestServer(primary)
	for _, repo := range []string{"octocat/Hello-World", "local:proj", "ephemeral"} {
		run := composer.RunInput{Agent: "claude-code", Repo: repo}
		items := srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), nil)
		for _, it := range items {
			if it.Kind == "workspace" {
				t.Errorf("repo=%q with an EMPTY spec must not produce a workspace row, got %+v", repo, it)
			}
		}
	}
}

// ── repo_credential ─────────────────────────────────────────────────────────

func TestDeriveSetupItems_RepoCredentialGitHubTokenUnverified(t *testing.T) {
	srv := newSetupTestServer()
	run := composer.RunInput{Agent: "claude-code", Repo: "octocat/Hello-World"}
	spec := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{githubTokenGrant()}}
	items := srv.deriveSetupItems(context.Background(), run, spec, secretsWith(), nil)
	got, ok := findItem(items, "repo_credential:github_token")
	if !ok || got.Status != "unverified" || got.Fix != nil {
		t.Errorf("github_token repo_credential = %+v (ok=%v), want unverified with no fix (mint-time brokered)", got, ok)
	}
}

func TestDeriveSetupItems_RepoCredentialGitPATPresentAbsent(t *testing.T) {
	srv := newSetupTestServer()
	run := composer.RunInput{Agent: "claude-code", Repo: "local:proj"}
	spec := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{gitPATGrant("dev.azure.com", "ado-pat")}}

	present := srv.deriveSetupItems(context.Background(), run, spec, secretsWith("ado-pat"), nil)
	got, ok := findItem(present, "repo_credential:git_pat:dev.azure.com")
	if !ok || got.Status != "satisfied" || got.Fix != nil {
		t.Errorf("git_pat repo_credential w/ secret = %+v, want satisfied", got)
	}

	absent := srv.deriveSetupItems(context.Background(), run, spec, secretsWith(), nil)
	got, ok = findItem(absent, "repo_credential:git_pat:dev.azure.com")
	if !ok || got.Status != "missing" || got.Fix == nil || got.Fix.SecretName != "ado-pat" {
		t.Errorf("git_pat repo_credential w/o secret = %+v, want missing + add_secret(ado-pat)", got)
	}
}
const workspaceWithProfilePath = "/home/me/proj"

func workspaceWithProfile(t *testing.T, egressDomains ...string) types.Workspace {
	t.Helper()
	profile, err := json.Marshal(map[string]any{"egress_domains": egressDomains, "confidence": "high"})
	if err != nil {
		t.Fatalf("marshal profile: %v", err)
	}
	return types.Workspace{
		ID:      uuid.New(),
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: workspaceWithProfilePath}},
		Name:    "proj", Status: types.WorkspaceScanned, Profile: profile,
	}
}

func TestDeriveSetupItems_EgressWorkspaceInfoAlwaysSatisfiedAndCopiesDomains(t *testing.T) {
	ws := workspaceWithProfile(t, "registry.npmjs.org", "pypi.org")
	srv := newSetupTestServer(ws)
	ro := true
	spec := types.RunPolicySpec{
		AllowedDomains:  []string{"github.com"},
		WorkspaceMounts: []types.WorkspaceMount{{Source: workspaceWithProfilePath, Target: "/home/agent/work", ReadOnly: &ro}},
	}
	run := composer.RunInput{Agent: "claude-code", Repo: "local:proj"}

	items := srv.deriveSetupItems(context.Background(), run, spec, secretsWith(), nil)
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
	items := srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), nil)
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
	items := srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), nil)
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
	items := srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), nil)
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
	items := srv.deriveSetupItems(context.Background(), run, spec, secretsWith(), nil)
	got, ok := findItem(items, "backend:CC2")
	if !ok || got.Status != "missing" {
		t.Errorf("policy-floor-derived backend item = %+v (ok=%v), want a missing CC2 row", got, ok)
	}
}

func TestDeriveSetupItems_BackendCC2NeedsSetup(t *testing.T) {
	srv := newSetupTestServer()
	srv.cfg.Runner = setupTestRunner{caps: runner.Capabilities{ConfinementClasses: []types.ConfinementClass{types.CC1}}}
	run := composer.RunInput{Agent: "claude-code", Repo: "ephemeral", ConfinementClass: string(types.CC2)}
	items := srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), nil)
	got, ok := findItem(items, "backend:CC2")
	if !ok || got.Status != "missing" || got.Fix == nil || got.Fix.Action != "none" || !strings.Contains(got.Detail, "wardyn setup wall") {
		t.Errorf("CC2-unavailable backend item = %+v (ok=%v), want missing + fix action \"none\" + fixable wall guidance", got, ok)
	}
}

// TestDeriveSetupItems_BackendNonContiguousClassesMirrorsLaunchGate: the backend
// row is the designated surrogate for the launch gate (preflight.go deliberately
// does not duplicate the 422), so it must answer with the gate's MEMBERSHIP rule,
// never a rank compare. A Kata-only host advertises the non-contiguous set
// [CC1, CC3] — the docker driver is tested to emit exactly that — on which
// create-run 422s a CC2 run. A rank compare (bestClass=CC3 >= CC2) called that
// row "satisfied" and sent the operator into a guaranteed 422.
func TestDeriveSetupItems_BackendNonContiguousClassesMirrorsLaunchGate(t *testing.T) {
	srv := newSetupTestServer()
	srv.cfg.Runner = setupTestRunner{caps: runner.Capabilities{
		ConfinementClasses: []types.ConfinementClass{types.CC1, types.CC3},
		Resolved:           map[types.ConfinementClass]string{types.CC3: "oci/kata-runtime"},
	}}
	run := composer.RunInput{Agent: "claude-code", Repo: "ephemeral", ConfinementClass: string(types.CC2)}
	items := srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), nil)
	got, ok := findItem(items, "backend:CC2")
	if !ok {
		t.Fatal("no backend:CC2 row for an explicit CC2 run")
	}
	if got.Status != "missing" {
		t.Errorf("CC2 on a [CC1,CC3] host: status = %q, want \"missing\" (create-run 422s here)", got.Status)
	}
	if !strings.Contains(got.Detail, "wardyn setup wall") {
		t.Errorf("detail = %q, want the fixable `wardyn setup wall` guidance", got.Detail)
	}
}

// CC3 unavailable: fixable-here (needs setup) vs not-fixable-on-this-host (no
// /dev/kvm) is a REAL hardware probe (internal/setup, commit 74b4d0a) — mirror
// its live result rather than assuming this test host's hardware either way.
func TestDeriveSetupItems_BackendCC3SplitsOnKVM(t *testing.T) {
	srv := newSetupTestServer()
	srv.cfg.Runner = setupTestRunner{caps: runner.Capabilities{ConfinementClasses: []types.ConfinementClass{types.CC1, types.CC2}}}
	run := composer.RunInput{Agent: "claude-code", Repo: "ephemeral", ConfinementClass: string(types.CC3)}
	items := srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), nil)
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
	// Pin the single-source wiring: the CC3 copy must come from
	// setup.VaultKVMDetail, not a re-inlined literal. Reverting to the bare
	// "a hardware/VM limit no install can fix" string fails this.
	if got.Detail != setup.VaultKVMDetail() {
		t.Errorf("CC3 detail = %q, want it sourced verbatim from setup.VaultKVMDetail() = %q", got.Detail, setup.VaultKVMDetail())
	}
}

func TestDeriveSetupItems_BackendNoRunner(t *testing.T) {
	srv := newSetupTestServer()
	run := composer.RunInput{Agent: "claude-code", Repo: "ephemeral", ConfinementClass: string(types.CC1)}
	items := srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), nil)
	got, ok := findItem(items, "backend:CC1")
	if !ok || got.Status != "missing" || got.Fix == nil || got.Fix.Action != "none" {
		t.Errorf("no-runner backend item = %+v (ok=%v), want missing + fix action \"none\"", got, ok)
	}
}

func TestDeriveSetupItems_BackendCapabilitiesProbeError(t *testing.T) {
	srv := newSetupTestServer()
	srv.cfg.Runner = setupTestRunner{capsErr: errors.New("docker daemon unreachable")}
	run := composer.RunInput{Agent: "claude-code", Repo: "ephemeral", ConfinementClass: string(types.CC1)}
	items := srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(), nil)
	got, ok := findItem(items, "backend:CC1")
	if !ok || got.Status != "unverified" || !strings.Contains(got.Detail, "docker daemon unreachable") {
		t.Errorf("probe-error backend item = %+v (ok=%v), want unverified carrying the probe error", got, ok)
	}
}
func TestDeriveSetupItems_Residency(t *testing.T) {
	srv := newSetupTestServer()

	// llm_access, api-key mode (no Claude cred mount in the FINAL spec): proxy_injected.
	run := composer.RunInput{Agent: "claude-code", Repo: "ephemeral"}
	items := srv.deriveSetupItems(context.Background(), run, types.RunPolicySpec{}, secretsWith(),
		&composeLLMAccess{Provisioned: true, Note: "ok"})
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
		&composeLLMAccess{Provisioned: true, Note: "ok"})
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
	items = srv.deriveSetupItems(context.Background(), run, secretSpec, secretsWith("anthropic-api-key", "ado-pat"), nil)
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
	items = srv.deriveSetupItems(context.Background(), run, repoSpec, secretsWith("ado-pat"), nil)
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
		ID:      uuid.New(),
		Name:    name,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: source}},
		Status:  types.WorkspaceScanned, Profile: mustJSON(p),
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

	items := srv.deriveSetupItems(context.Background(), run, mountSpec("/w"), secretsWith("database-url"), nil)

	sat, ok := findItem(items, "workspace_secret:database-url")
	if !ok || sat.Kind != "workspace_secret" || sat.Status != "satisfied" || sat.Fix != nil {
		t.Errorf("present workspace secret = %+v, want satisfied workspace_secret with no fix", sat)
	}
	miss, ok := findItem(items, "workspace_secret:oidc-client-secret")
	if !ok || miss.Status != "missing" || miss.Fix == nil || miss.Fix.Action != "add_secret" || miss.Fix.SecretName != "oidc-client-secret" {
		t.Errorf("absent workspace secret = %+v, want missing + add_secret(oidc-client-secret)", miss)
	}
	// Exact match, not just a substring: a plain noun phrase, like every other
	// RequiredBy in this file — compose-review.tsx prepends "Required by ", so a
	// value starting with its own "declared by"/"required by" would double up
	// (see reconcile-workspace-first.md item 3; the sibling contract-required
	// branch is pinned in workspace_requirements_fold_test.go).
	if want := "workspace app (untrusted content, names only)"; miss.RequiredBy != want {
		t.Errorf("workspace_secret RequiredBy = %q, want %q", miss.RequiredBy, want)
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
		mountSpec("/w"), secretsWith(), nil)

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
		spec, secretsWith(), nil)

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
