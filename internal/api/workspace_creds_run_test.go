// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// These tests drive the WHOLE create path (POST /api/v1/runs -> handleCreateRun)
// and assert the run's PERSISTED grants / dispatched policy. That end-to-end
// framing is the point: applyWorkspaceCreds itself was always correct — it ran
// 52 lines AFTER persistRunGrants had already snapshotted spec.EligibleGrants,
// so a per-workspace binding never reached the run. A test calling
// applyWorkspaceCreds directly passes against that bug and proves nothing.

// credRunStore is the minimal in-memory Store the create+dispatch path drives:
// one run cell, the grants it persists, and container workspaces by source.
// Deliberately not the PG harness — this invariant must be provable without
// WARDYN_TEST_PG.
type credRunStore struct {
	store.Store
	mu     sync.Mutex
	runs   map[uuid.UUID]types.AgentRun
	grants []types.CredentialGrant
	ws     map[string]types.Workspace // container image ref -> onboarded workspace
	state  types.RunState
}

func newCredRunStore(ws types.Workspace) *credRunStore {
	return &credRunStore{
		runs:  map[uuid.UUID]types.AgentRun{},
		ws:    map[string]types.Workspace{ws.Source: ws},
		state: types.RunPending,
	}
}

func (s *credRunStore) ListWorkspaces(context.Context) ([]types.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]types.Workspace, 0, len(s.ws))
	for _, w := range s.ws {
		out = append(out, w)
	}
	return out, nil
}

func (s *credRunStore) CreateRun(_ context.Context, run types.AgentRun) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[run.ID] = run
	return run, nil
}

func (s *credRunStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[id]
	if !ok {
		return types.AgentRun{}, store.ErrNotFound
	}
	run.State = s.state
	return run, nil
}

func (s *credRunStore) ListRuns(context.Context) ([]types.AgentRun, error) { return nil, nil }

func (s *credRunStore) UpdateRunStateIf(_ context.Context, _ uuid.UUID, from, to types.RunState) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != from {
		return false, nil
	}
	s.state = to
	return true, nil
}

func (s *credRunStore) CreateGrant(_ context.Context, g types.CredentialGrant) (types.CredentialGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants = append(s.grants, g)
	return g, nil
}

func (s *credRunStore) GetWorkspaceBySource(_ context.Context, kind types.WorkspaceKind, source string) (types.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w, ok := s.ws[source]; ok && w.Kind == kind {
		return w, nil
	}
	return types.Workspace{}, store.ErrNotFound
}

func (s *credRunStore) SetRunImage(context.Context, uuid.UUID, string) error { return nil }
func (s *credRunStore) SetSandboxRef(context.Context, uuid.UUID, string) error {
	return nil
}
func (s *credRunStore) SetRunAgentExecID(context.Context, uuid.UUID, string) error { return nil }
func (s *credRunStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, nil
}

// apiKeyGrants returns the persisted api_key grants' injection rules.
func (s *credRunStore) apiKeyGrants() []egress.InjectionRule {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []egress.InjectionRule
	for _, g := range s.grants {
		if g.Spec.Kind != types.GrantAPIKey {
			continue
		}
		if rule, err := injectionRuleFromScope(g.Spec.Scope); err == nil {
			out = append(out, rule)
		}
	}
	return out
}

// stubImageBuilder stands in for the BYOI wrap a container workspace needs
// (req.Image is rejected up front without an ImageBuilder wired).
type stubImageBuilder struct{}

func (stubImageBuilder) BuildDevcontainer(context.Context, string, string, string) (string, error) {
	return "", nil
}
func (stubImageBuilder) BuildFromDevcontainerFiles(context.Context, map[string]string, string) (string, error) {
	return "", nil
}
func (stubImageBuilder) FinalizeBase(_ context.Context, base, out string) (string, error) {
	_ = base
	return out, nil
}

const (
	credRunImage = "acme/dev-image:1"
	credRunDir   = "/srv/acme"
)

// containerWorkspace is an onboarded CONTAINER workspace carrying a cred binding
// (picked by a run via --image). localDirWorkspace is the mount-source twin,
// picked via the policy's workspace_mounts — the two ways a run acquires a
// primary workspace, and both must inherit its binding.
func containerWorkspace(cred *types.WorkspaceLLMCred) types.Workspace {
	return types.Workspace{
		ID: uuid.New(), Kind: types.WorkspaceKindContainer, Source: credRunImage,
		Name: "acme dev image", Status: types.WorkspaceScanned, LLMCred: cred,
	}
}

func localDirWorkspace(cred *types.WorkspaceLLMCred) types.Workspace {
	return types.Workspace{
		ID: uuid.New(), Kind: types.WorkspaceKindLocalDir, Source: credRunDir,
		Name: "acme src", Status: types.WorkspaceScanned, LLMCred: cred,
	}
}

// credRunServer wires a Server over credRunStore. runner may be nil (create
// only, no dispatch).
func credRunServer(t *testing.T, st *credRunStore, secrets *memSecrets, rn *fakeRunner) (*Server, *recRecorder) {
	t.Helper()
	h := newHarness(t)
	audit := &recRecorder{}
	cfg := baseTestConfig(h, st)
	cfg.Audit = audit
	cfg.Broker = h.broker
	cfg.Secrets = secrets
	cfg.ImageBuilder = stubImageBuilder{}
	if rn != nil {
		cfg.Runner = rn
	}
	cfg.DefaultPolicy = types.RunPolicySpec{
		AllowedDomains:      []string{"api.anthropic.com"},
		MinConfinementClass: types.CC1,
	}
	return New(cfg), audit
}

// createCredRun POSTs a run that picks the container workspace by image.
func createCredRun(t *testing.T, srv *Server, inlinePolicy string) {
	t.Helper()
	body := `{"agent":"claude-code","task":"do the thing","image":"` + credRunImage +
		`","inline_policy":` + inlinePolicy + `}`
	createCredRunBody(t, srv, body)
}

// createCredRunMount POSTs a run that picks the local_dir workspace by mounting
// it (no --image, so dispatch skips the BYOI selftest gate).
func createCredRunMount(t *testing.T, srv *Server, inlinePolicy string) {
	t.Helper()
	createCredRunBody(t, srv, `{"agent":"claude-code","task":"do the thing","inline_policy":`+inlinePolicy+`}`)
}

func createCredRunBody(t *testing.T, srv *Server, body string) {
	t.Helper()
	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create run: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
}

// anthropicAPIKeyPolicy is an inline policy that ALREADY brokers an anthropic
// api-key grant — the competing grant a managed/bedrock binding must displace.
const anthropicAPIKeyPolicy = `{"allowed_domains":["api.anthropic.com"],"min_confinement_class":"CC1",` +
	`"eligible_grants":[{"kind":"api_key","scope":{"host":"api.anthropic.com","header":"x-api-key","format":"%s","secret_name":"anthropic-api-key"},"ttl_seconds":3600}]}`

const plainPolicy = `{"allowed_domains":["api.anthropic.com"],"min_confinement_class":"CC1"}`

// anthropicAPIKeyMountPolicy is anthropicAPIKeyPolicy plus the workspace mount
// that makes the local_dir workspace this run's PRIMARY.
const anthropicAPIKeyMountPolicy = `{"allowed_domains":["api.anthropic.com"],"min_confinement_class":"CC1",` +
	`"workspace_mounts":[{"source":"` + credRunDir + `","target":"/home/agent/work"}],` +
	`"eligible_grants":[{"kind":"api_key","scope":{"host":"api.anthropic.com","header":"x-api-key","format":"%s","secret_name":"anthropic-api-key"},"ttl_seconds":3600}]}`

// credsAuditCount counts the run.workspace.creds events (must fire exactly once).
func credsAuditCount(audit *recRecorder) (int, string) {
	n, mode := 0, ""
	for _, ev := range audit.events {
		if ev.Action != "run.workspace.creds" {
			continue
		}
		n++
		var d struct {
			Mode string `json:"mode"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		mode = d.Mode
	}
	return n, mode
}

// TestCreateRun_WorkspaceAPIKeyCred_ReachesPersistedGrants: a workspace bound to
// its OWN api-key secret must have that grant PERSISTED for the run. Before the
// create-path reorder the binding landed after persistRunGrants had snapshotted
// the spec, so no grant (and no proxy injection) existed: the run silently fell
// back to the control-plane managed subscription — billing the operator instead
// of the workspace's key — or failed auth outright.
func TestCreateRun_WorkspaceAPIKeyCred_ReachesPersistedGrants(t *testing.T) {
	st := newCredRunStore(containerWorkspace(&types.WorkspaceLLMCred{
		Mode: types.WorkspaceLLMCredAPIKey, APIKeySecret: "acme-anthropic-key",
	}))
	srv, audit := credRunServer(t, st, &memSecrets{m: map[string][]byte{
		"acme-anthropic-key": []byte("sk-acme"),
	}}, nil)

	createCredRun(t, srv, plainPolicy)

	rules := st.apiKeyGrants()
	if len(rules) != 1 || rules[0].SecretName != "acme-anthropic-key" || rules[0].Host != "api.anthropic.com" {
		t.Fatalf("persisted api_key grants = %+v, want exactly one for api.anthropic.com naming the WORKSPACE secret", rules)
	}
	if n, mode := credsAuditCount(audit); n != 1 || mode != "api_key" {
		t.Errorf("run.workspace.creds events = %d (mode %q), want exactly 1 with mode=api_key", n, mode)
	}
}

// TestCreateRun_WorkspaceManagedCred_DisplacesAPIKeyGrant: a workspace bound to
// the Wardyn-managed subscription must DROP a competing anthropic api-key grant
// from the run's persisted grants — otherwise the api key is injected and wins,
// and the operator's chosen transport is silently ignored (dispatch's managed
// gate is suppressed by exactly this pre-existing injection).
func TestCreateRun_WorkspaceManagedCred_DisplacesAPIKeyGrant(t *testing.T) {
	st := newCredRunStore(containerWorkspace(&types.WorkspaceLLMCred{
		Mode: types.WorkspaceLLMCredManaged,
	}))
	srv, audit := credRunServer(t, st, &memSecrets{m: map[string][]byte{
		"anthropic-api-key": []byte("sk-operator"),
	}}, nil)

	createCredRun(t, srv, anthropicAPIKeyPolicy)

	if rules := st.apiKeyGrants(); len(rules) != 0 {
		t.Fatalf("persisted api_key grants = %+v, want none — a managed binding must displace the competing anthropic api-key grant", rules)
	}
	if n, mode := credsAuditCount(audit); n != 1 || mode != "managed" {
		t.Errorf("run.workspace.creds events = %d (mode %q), want exactly 1 with mode=managed", n, mode)
	}
}

// TestCreateRun_WorkspaceBedrockCred_DisplacesAPIKeyGrantAndAllowsRegion: the
// bedrock twin of the managed case, plus the per-workspace REGION wire-through —
// the workspace's region/model must beat the server's global Bedrock config at
// dispatch, and its regional hosts must be egress-allowed.
func TestCreateRun_WorkspaceBedrockCred_DisplacesAPIKeyGrantAndAllowsRegion(t *testing.T) {
	const wsRegion, wsModel = "eu-central-1", "eu.anthropic.claude-sonnet-4-5-20250929-v1:0"
	st := newCredRunStore(localDirWorkspace(&types.WorkspaceLLMCred{
		Mode:    types.WorkspaceLLMCredBedrock,
		Bedrock: &types.WorkspaceBedrockRef{Region: wsRegion, Model: wsModel},
	}))
	fr := &fakeRunner{}
	srv, audit := credRunServer(t, st, &memSecrets{m: map[string][]byte{
		"anthropic-api-key":          []byte("sk-operator"),
		bedrockAccessKeyIDSecret:     []byte("AKIATESTTESTTESTTEST"),
		bedrockSecretAccessKeySecret: []byte("wJalrXUtnFEMItesttesttesttesttesttestKEY"),
	}}, fr)
	// The GLOBAL Bedrock config the workspace binding must override.
	srv.cfg.BedrockRegion = "us-east-1"
	srv.cfg.BedrockModel = "us.anthropic.claude-sonnet-4-5-20250929-v1:0"

	createCredRunMount(t, srv, anthropicAPIKeyMountPolicy)

	if rules := st.apiKeyGrants(); len(rules) != 0 {
		t.Fatalf("persisted api_key grants = %+v, want none — a bedrock binding must displace the competing anthropic api-key grant", rules)
	}
	if n, mode := credsAuditCount(audit); n != 1 || mode != "bedrock" {
		t.Errorf("run.workspace.creds events = %d (mode %q), want exactly 1 with mode=bedrock", n, mode)
	}

	if fr.createCalls != 1 {
		t.Fatalf("CreateSandbox calls = %d, want 1", fr.createCalls)
	}
	spec := fr.lastSpec
	if got := spec.Env["AWS_REGION"]; got != wsRegion {
		t.Errorf("Env[AWS_REGION] = %q, want the WORKSPACE region %q (global us-east-1 is only the fallback)", got, wsRegion)
	}
	if got := spec.Env["ANTHROPIC_MODEL"]; got != wsModel {
		t.Errorf("Env[ANTHROPIC_MODEL] = %q, want the WORKSPACE model %q", got, wsModel)
	}
	for _, want := range []string{
		bedrockRuntimeHost(wsRegion), bedrockControlHost(wsRegion),
	} {
		if !domainAllowedExact(spec.ProxyConfig.Policy.AllowedDomains, want) {
			t.Errorf("AllowedDomains missing %q; got %v", want, spec.ProxyConfig.Policy.AllowedDomains)
		}
	}
	if domainAllowedExact(spec.ProxyConfig.Policy.AllowedDomains, bedrockRuntimeHost("us-east-1")) {
		t.Errorf("AllowedDomains carries the GLOBAL region's bedrock host; the workspace override must replace it: %v",
			spec.ProxyConfig.Policy.AllowedDomains)
	}
}

// TestValidateWorkspaceLLMCred_Rejections pins the two write-time guards: an
// api_key binding may not name a reserved/sink secret (the sink refuses to
// resolve it, so the run would fail the proxy closed — and it must never be a
// route to inject Wardyn's own signing key / OAuth tokens), and a bedrock
// binding may not half-override (region without model or vice versa).
func TestValidateWorkspaceLLMCred_Rejections(t *testing.T) {
	for _, name := range []string{"wardyn-signing-key", bedrockSecretAccessKeySecret} {
		cred := &types.WorkspaceLLMCred{Mode: types.WorkspaceLLMCredAPIKey, APIKeySecret: name}
		if msg := validateWorkspaceLLMCred(cred); msg == "" {
			t.Errorf("api_key binding naming the sink-reserved %q was accepted; want rejection", name)
		}
	}
	half := &types.WorkspaceLLMCred{
		Mode: types.WorkspaceLLMCredBedrock, Bedrock: &types.WorkspaceBedrockRef{Region: "eu-central-1"},
	}
	if msg := validateWorkspaceLLMCred(half); msg == "" {
		t.Error("bedrock binding with a region but no model was accepted; want rejection (a region-scoped inference profile 403s at invoke)")
	}
	both := &types.WorkspaceLLMCred{
		Mode:    types.WorkspaceLLMCredBedrock,
		Bedrock: &types.WorkspaceBedrockRef{Region: "eu-central-1", Model: "eu.anthropic.claude-sonnet-4-5-20250929-v1:0"},
	}
	if msg := validateWorkspaceLLMCred(both); msg != "" {
		t.Errorf("complete bedrock override rejected: %s", msg)
	}
	if msg := validateWorkspaceLLMCred(&types.WorkspaceLLMCred{Mode: types.WorkspaceLLMCredBedrock}); msg != "" {
		t.Errorf("bedrock binding with no override (inherit the global config) rejected: %s", msg)
	}
}
