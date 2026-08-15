// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/subscription"
	"github.com/cjohnstoniv/wardyn/internal/types"
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
	runs    map[uuid.UUID]types.AgentRun
	records []RecordTaskResult // every SetWorkspaceRecordResult write, in order
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
func (s *recordLLMModeStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, nil
}
func (s *recordLLMModeStore) CreateGrant(_ context.Context, g types.CredentialGrant) (types.CredentialGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.runs[g.RunID]; !ok {
		return types.CredentialGrant{}, fmt.Errorf("insert credential_grants: foreign key violation: run %s has no agent_runs row", g.RunID)
	}
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
