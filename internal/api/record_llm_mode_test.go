// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/subscription"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// recordLLMModeStore is a minimal in-memory Store that, unlike fkGrantStore
// (whose UpdateRunStateIf always returns false), actually advances a run's
// state — so dispatchRun runs its FULL body (through resolveLLMTransport) and
// this test can observe what launchRecordRun ultimately persists as the
// session's llm_mode/model, not just its pre-dispatch guess.
type recordLLMModeStore struct {
	store.Store
	mu      sync.Mutex
	ws      types.Workspace
	sc      types.SiteConfig // returned by GetSiteConfig; zero value = today's "no site config"
	runs    map[uuid.UUID]types.AgentRun
	records []RecordTaskResult      // every SetWorkspaceRecordResult write, in order
	grants  []types.CredentialGrant // every CreateGrant call, in order
}

func newRecordLLMModeStore(ws types.Workspace) *recordLLMModeStore {
	return &recordLLMModeStore{ws: ws, runs: map[uuid.UUID]types.AgentRun{}}
}

func (s *recordLLMModeStore) CreateRun(_ context.Context, run types.AgentRun) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[run.ID] = run
	return run, nil
}
func (s *recordLLMModeStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return types.AgentRun{}, store.ErrNotFound
	}
	return r, nil
}
func (s *recordLLMModeStore) UpdateRunStateIf(_ context.Context, id uuid.UUID, from, to types.RunState) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok || r.State != from {
		return false, nil
	}
	r.State = to
	s.runs[id] = r
	return true, nil
}
func (s *recordLLMModeStore) SetSandboxRef(_ context.Context, id uuid.UUID, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[id]
	r.SandboxRef = ref
	s.runs[id] = r
	return nil
}
func (s *recordLLMModeStore) SetRunAgentExecID(context.Context, uuid.UUID, string) error { return nil }
func (s *recordLLMModeStore) SetWorkspaceBuiltImage(_ context.Context, _ uuid.UUID, imageRef, hash string) (types.Workspace, error) {
	ws := s.ws
	ws.ImageRef, ws.BuiltProfileHash = imageRef, hash
	return ws, nil
}
func (s *recordLLMModeStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return s.sc, nil
}
func (s *recordLLMModeStore) CreateGrant(_ context.Context, g types.CredentialGrant) (types.CredentialGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.runs[g.RunID]; !ok {
		return types.CredentialGrant{}, fmt.Errorf("insert credential_grants: foreign key violation: run %s has no agent_runs row", g.RunID)
	}
	s.grants = append(s.grants, g)
	return g, nil
}
func (s *recordLLMModeStore) ClaimWorkspaceActiveRun(_ context.Context, _ uuid.UUID, runID uuid.UUID, _ *uuid.UUID) (types.Workspace, bool, error) {
	ws := s.ws
	ws.ActiveRunID = &runID
	return ws, true, nil
}
func (s *recordLLMModeStore) ClearWorkspaceActiveRun(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	return true, nil
}
func (s *recordLLMModeStore) SetWorkspaceRecordResult(_ context.Context, _ uuid.UUID, _ string, result json.RawMessage, _ string) (types.Workspace, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var res RecordTaskResult
	_ = json.Unmarshal(result, &res)
	s.records = append(s.records, res)
	return s.ws, true, nil
}
func (s *recordLLMModeStore) lastRecord() RecordTaskResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.records[len(s.records)-1]
}

// TestLaunchRecordRun_ManagedSubscriptionCorrectsLLMMode is W20-llm-transport-
// matrix-2: launchRecordRun computes llm_mode/model BEFORE dispatch, from a
// mount-target check (specHasMountTarget(claudeCredTarget)) that only ever
// sees a HOST-STAGED resident subscription. The Wardyn-MANAGED subscription
// (no mount at all — compose-mode, gated on s.managedInjectReady/ManagedToken)
// is resolved later, inside dispatch's resolveLLMTransport — so a managed
// session's pre-dispatch guess is stuck at "none" even though the run
// actually authenticated via the managed subscription. The session entry
// must instead reflect the ACTUAL transport dispatch resolved.
func TestLaunchRecordRun_ManagedSubscriptionCorrectsLLMMode(t *testing.T) {
	h := newHarness(t)
	wsID := uuid.New()
	ws := types.Workspace{ID: wsID, Status: types.WorkspaceScanned} // no Sources: no clone grant to wire
	fake := newRecordLLMModeStore(ws)
	cfg := baseTestConfig(h, fake)
	cfg.Runner = &fakeRunner{}
	cfg.Broker = h.broker
	// No resident ~/.claude mount is blessed (DefaultPolicy carries none), so
	// the ONLY way this run authenticates is the Wardyn-managed subscription —
	// exactly the lane resolveLLMTransport's `managed` gate covers.
	cfg.ManagedToken = fakeSubProvider{tok: subscription.Token{Value: "managed-tok"}}
	srv := New(cfg)

	// confined=false: AllowAllEgress, satisfying resolveLLMTransport's managed
	// gate `(AllowAllEgress || len(AllowedDomains) > 0)`.
	_, _, err := srv.launchRecordRun(context.Background(), "alice@example.com", ws, "build", "build", false)
	if err != nil {
		t.Fatalf("launchRecordRun: %v", err)
	}

	got := fake.lastRecord()
	if got.LLMMode != "subscription" {
		t.Fatalf("session llm_mode = %q, want %q (managed subscription actually credentialed the run — "+
			"the pre-dispatch guess, which only detects a resident mount, must be corrected against dispatch's "+
			"resolved llmTransport)", got.LLMMode, "subscription")
	}
}

// TestLaunchRecordRun_HonorsSiteWideDefaultIntegration is
// W20-W20-llm-transport-matrix-1: model access resolves in three tiers
// (docs/OPERATIONS.md "Model access resolves") — run-explicit integration_id,
// then the workspace's own LLMCred.IntegrationRef binding, then the
// operator's site-wide DefaultFor:agent_runs integration. launchRecordRun
// used to gate its ENTIRE foldRunIntegration call on the workspace carrying
// its own binding, so an unbound workspace's record session never consulted
// tier 3 at all and fell straight to the generic ceiling/convention grant —
// silently skipping the operator's configured default integration. This
// workspace has NO LLMCred binding; the site config has one AI-provider
// integration marked DefaultFor: agent_runs, so the record session's minted
// credential grant must carry THAT integration's secret, not the convention
// fallback's.
func TestLaunchRecordRun_HonorsSiteWideDefaultIntegration(t *testing.T) {
	h := newHarness(t)
	wsID := uuid.New()
	ws := types.Workspace{ID: wsID, Status: types.WorkspaceScanned} // LLMCred nil: no workspace-level binding
	fake := newRecordLLMModeStore(ws)
	fake.sc = types.SiteConfig{
		Integrations: []types.Integration{{
			ID:         "corp-default-anthropic",
			Name:       "Corp default Anthropic key",
			Kind:       types.IntegrationKindAnthropicAPIKey,
			DefaultFor: []string{"agent_runs"},
			Secrets: []types.IntegrationSecret{{
				Role:       "api_key",
				SecretName: "corp-anthropic-key",
			}},
		}},
	}
	cfg := baseTestConfig(h, fake)
	cfg.Runner = &fakeRunner{}
	cfg.Broker = h.broker
	cfg.Secrets = &memSecrets{m: map[string][]byte{"corp-anthropic-key": []byte("sk-corp")}}
	srv := New(cfg)

	_, _, err := srv.launchRecordRun(context.Background(), "alice@example.com", ws, "build", "build", false)
	if err != nil {
		t.Fatalf("launchRecordRun: %v", err)
	}

	fake.mu.Lock()
	grants := fake.grants
	fake.mu.Unlock()
	for _, g := range grants {
		if g.Spec.Kind != types.GrantAPIKey {
			continue
		}
		var scope struct {
			SecretName string `json:"secret_name"`
		}
		_ = json.Unmarshal(g.Spec.Scope, &scope)
		if scope.SecretName == "corp-anthropic-key" {
			return // found: the site-wide default integration's own grant was minted
		}
	}
	t.Fatalf("no credential grant named the site-wide DefaultFor:agent_runs integration's secret "+
		"(corp-anthropic-key); got grants=%+v — an unbound workspace's record session must still "+
		"resolve tier 3 of model-access precedence, not skip straight to the convention fallback", grants)
}

// TestLaunchRecordRun_RepoDevcontainerImageWarnsMissingCLI is
// W20-W20-record-image-6: a workspace whose primary source is a repo
// carrying its OWN devcontainer builds that devcontainer AS-IS
// (resolveWorkspaceImage's repo-own-devcontainer lane) — which never bakes
// claude-code. The Record pane tells the operator to drive the agent in this
// sandbox regardless, so the session entry must carry a caveat naming what
// happened, instead of silently wiring model credentials for a CLI that may
// not be there.
func TestLaunchRecordRun_RepoDevcontainerImageWarnsMissingCLI(t *testing.T) {
	h := newHarness(t)
	wsID := uuid.New()
	profile := workspacescan.WorkspaceProfile{
		Languages: []string{"Go"}, Confidence: workspacescan.ConfidenceHigh, Source: workspacescan.SourceDeterministic,
		HasDevcontainer: true,
	}
	ws := types.Workspace{
		ID:      wsID,
		Status:  types.WorkspaceScanned,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "https://github.com/acme/widgets", Ref: "main"}},
		Profile: mustJSON(profile),
	}
	fake := newRecordLLMModeStore(ws)
	cfg := baseTestConfig(h, fake)
	cfg.Runner = &fakeRunner{}
	cfg.Broker = h.broker
	cfg.ImageBuilder = fakeImageBuilder{}
	srv := New(cfg)

	_, _, err := srv.launchRecordRun(context.Background(), "alice@example.com", ws, "build", "build", false)
	if err != nil {
		t.Fatalf("launchRecordRun: %v", err)
	}

	got := fake.lastRecord()
	found := false
	for _, c := range got.Caveats {
		if strings.Contains(c, "claude-code") {
			found = true
		}
	}
	if !found {
		t.Fatalf("session caveats = %v, want one naming claude-code's absence on a repo-own-devcontainer image", got.Caveats)
	}
}

// TestLaunchRecordRun_GeneratedDevcontainerImageHasNoCLICaveat is the
// counterpart: a workspace with NO repo-own devcontainer (the ordinary
// generated-toolchain lane, which DOES bake claude-code unconditionally)
// must carry no such caveat — the warning is specific to the repo-own-
// devcontainer lane, not a blanket note on every session.
func TestLaunchRecordRun_GeneratedDevcontainerImageHasNoCLICaveat(t *testing.T) {
	h := newHarness(t)
	wsID := uuid.New()
	ws := types.Workspace{ID: wsID, Status: types.WorkspaceScanned} // no Sources, no profile: convention image
	fake := newRecordLLMModeStore(ws)
	cfg := baseTestConfig(h, fake)
	cfg.Runner = &fakeRunner{}
	cfg.Broker = h.broker
	srv := New(cfg)

	_, _, err := srv.launchRecordRun(context.Background(), "alice@example.com", ws, "build", "build", false)
	if err != nil {
		t.Fatalf("launchRecordRun: %v", err)
	}

	got := fake.lastRecord()
	if len(got.Caveats) != 0 {
		t.Errorf("session caveats = %v, want none for a convention/generated-devcontainer image", got.Caveats)
	}
}
