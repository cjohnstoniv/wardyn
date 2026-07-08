// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

type verifyStore struct {
	store.Store
	ws     types.Workspace
	run    types.AgentRun
	state  *types.Workspace
	hasRun bool
}

func (s *verifyStore) GetWorkspace(context.Context, uuid.UUID) (types.Workspace, error) {
	return s.ws, nil
}
func (s *verifyStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	if !s.hasRun {
		return types.AgentRun{}, store.ErrNotFound
	}
	return s.run, nil
}
func (s *verifyStore) SetWorkspaceImportState(_ context.Context, _ uuid.UUID, status types.WorkspaceStatus, active *uuid.UUID, vr json.RawMessage, vh string, va *time.Time) (types.Workspace, error) {
	ws := s.ws
	ws.Status = status
	ws.ActiveRunID = active
	ws.VerifyResult = vr
	ws.VerifiedProfileHash = vh
	ws.VerifiedAt = va
	s.state = &ws
	return ws, nil
}

func newVerifySrv(t *testing.T, fake store.Store) *Server {
	h := newHarness(t)
	return New(Config{Identity: h.idp, Audit: h.audit, AdminToken: adminToken,
		TrustDomain: "wardyn.local", ControlPlaneURL: "http://wardynd:8080", Store: fake})
}

func TestVerifyWorkspace_GuardsNoCommandsNoRunner(t *testing.T) {
	wsID := uuid.New()
	// No approved commands → 422.
	fake := &verifyStore{ws: types.Workspace{ID: wsID, Kind: types.WorkspaceKindLocalDir, Source: "/w", Status: types.WorkspaceScanned}}
	srv := newVerifySrv(t, fake)
	if w := do(t, srv, http.MethodPost, "/api/v1/workspaces/"+wsID.String()+"/verify", adminToken, ""); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("no-commands: code = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	// Commands present but no runner → 503 (honest "verify needs a runner").
	fake.ws.SetupCommands = mustJSON([]workspacescan.SetupCommand{{Stage: "install", Command: "npm ci"}})
	if w := do(t, srv, http.MethodPost, "/api/v1/workspaces/"+wsID.String()+"/verify", adminToken, ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("no-runner: code = %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

func TestUploadVerifyResult_CrossRunRejected(t *testing.T) {
	h := newHarness(t)
	tok := h.mintRunToken(t, uuid.New())
	other := uuid.New()
	w := do(t, h.srv, http.MethodPut, "/api/v1/internal/verify-results/"+other.String(), tok, `{"ok":true}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-run verify upload: code = %d, want 403", w.Code)
	}
}

func TestUploadVerifyResult_GreenSetsReadyVerified(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	wsID := uuid.New()
	tok := h.mintRunToken(t, runID)
	fake := &verifyStore{
		hasRun: true,
		run:    types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace verify"},
		ws:     types.Workspace{ID: wsID, Kind: types.WorkspaceKindLocalDir, Source: "/w", Status: types.WorkspaceVerifying, BuiltProfileHash: "abc123"},
	}
	srv := New(Config{Identity: h.idp, Audit: h.audit, AdminToken: adminToken, TrustDomain: "wardyn.local", ControlPlaneURL: "http://wardynd:8080", Store: fake})
	// A green upload (all steps exit 0).
	body := `{"ran":true,"ok":true,"done":true,"total":2,"steps":[{"stage":"install","command":"npm ci","exit_code":0},{"stage":"test","command":"npm test","exit_code":0}]}`
	w := do(t, srv, http.MethodPut, "/api/v1/internal/verify-results/"+runID.String(), tok, body)
	if w.Code != http.StatusNoContent {
		t.Fatalf("code = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if fake.state == nil || fake.state.Status != types.WorkspaceReady {
		t.Errorf("green verify should set status=ready, got %+v", fake.state)
	}
	if fake.state.VerifiedProfileHash != "abc123" || fake.state.VerifiedAt == nil {
		t.Errorf("green verify should stamp VerifiedProfileHash+VerifiedAt, got hash=%q at=%v", fake.state.VerifiedProfileHash, fake.state.VerifiedAt)
	}
	if fake.state.ActiveRunID != nil {
		t.Error("active_run_id should be cleared after verify completes")
	}
}

func TestSuggestVerifyFix_NoComposer404(t *testing.T) {
	// The default harness has no composer configured → 404 (feature not enabled).
	h := newHarness(t)
	id := uuid.New().String()
	if w := do(t, h.srv, http.MethodPost, "/api/v1/workspaces/"+id+"/verify/suggest-fix", adminToken, ""); w.Code != http.StatusNotFound {
		t.Errorf("no-composer suggest-fix: code = %d, want 404", w.Code)
	}
}

func TestUploadVerifyResult_ProgressKeepsVerifying(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	wsID := uuid.New()
	tok := h.mintRunToken(t, runID)
	fake := &verifyStore{
		hasRun: true,
		run:    types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace verify"},
		ws:     types.Workspace{ID: wsID, Kind: types.WorkspaceKindLocalDir, Source: "/w", Status: types.WorkspaceVerifying, BuiltProfileHash: "abc"},
	}
	srv := New(Config{Identity: h.idp, Audit: h.audit, AdminToken: adminToken, TrustDomain: "wardyn.local", ControlPlaneURL: "http://wardynd:8080", Store: fake})
	// A PROGRESS upload (done omitted/false): install done, build running.
	body := `{"ran":true,"total":2,"steps":[{"stage":"install","command":"npm ci","exit_code":0},{"stage":"build","command":"npm run build","running":true}]}`
	w := do(t, srv, http.MethodPut, "/api/v1/internal/verify-results/"+runID.String(), tok, body)
	if w.Code != http.StatusNoContent {
		t.Fatalf("progress upload code = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	// Stays verifying, keeps the run pointer, does NOT stamp verified.
	if fake.state == nil || fake.state.Status != types.WorkspaceVerifying {
		t.Errorf("progress upload must keep status=verifying, got %+v", fake.state)
	}
	if fake.state.ActiveRunID == nil || *fake.state.ActiveRunID != runID {
		t.Error("progress upload must keep the in-flight active_run_id")
	}
	if fake.state.VerifiedProfileHash != "" {
		t.Error("progress upload must not stamp verified markers")
	}
	// The partial result (with the running step) is persisted for the live UI.
	var vr workspacescan.VerifyResult
	if json.Unmarshal(fake.state.VerifyResult, &vr) != nil || len(vr.Steps) != 2 || !vr.Steps[1].Running {
		t.Errorf("partial verify_result not persisted with the running step: %s", fake.state.VerifyResult)
	}
}

func TestReconcileWorkspaceRun_StuckVerifyFailsCleanly(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	wsID := uuid.New()
	fake := &verifyStore{
		hasRun: true,
		run:    types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace verify"},
		ws:     types.Workspace{ID: wsID, Status: types.WorkspaceVerifying, ActiveRunID: &runID},
	}
	s := New(Config{Identity: h.idp, Audit: h.audit, AdminToken: adminToken, TrustDomain: "wardyn.local", ControlPlaneURL: "http://x", Store: fake})
	s.reconcileWorkspaceRun(context.Background(), runID)
	if fake.state == nil || fake.state.Status != types.WorkspaceVerifyFailed {
		t.Fatalf("stuck verify should reconcile to verify_failed, got %+v", fake.state)
	}
	if fake.state.ActiveRunID != nil {
		t.Error("active_run_id should be cleared")
	}
	var vr workspacescan.VerifyResult
	if json.Unmarshal(fake.state.VerifyResult, &vr) != nil || vr.OK || !vr.Done {
		t.Errorf("synthetic result should be a done failure: %s", fake.state.VerifyResult)
	}

	// If a DIFFERENT run now owns the workspace, this terminal run must NOT touch it.
	other := uuid.New()
	fake.state = nil
	fake.ws = types.Workspace{ID: wsID, Status: types.WorkspaceVerifying, ActiveRunID: &other}
	s.reconcileWorkspaceRun(context.Background(), runID)
	if fake.state != nil {
		t.Error("a terminal run must not reconcile a workspace owned by a newer run")
	}
}
