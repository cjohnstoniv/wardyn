// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── + scan-reconcile scoped write ────────────────────────────────

// scanReconcileStore serves a stuck `scanning` workspace + its scan run, records
// whether the reconcile used the scoped SetWorkspaceImportState (state) or the
// full-row UpdateWorkspace clobber anti-pattern (fullRowWrite).
type scanReconcileStore struct {
	store.Store
	importStateFake
	run          types.AgentRun
	fullRowWrite bool
}

func (s *scanReconcileStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	return s.run, nil
}
func (s *scanReconcileStore) GetWorkspace(context.Context, uuid.UUID) (types.Workspace, error) {
	return s.ws, nil
}

// SetWorkspaceImportState disambiguates the embedded nil store.Store vs
// importStateFake (both declare it) by routing to importStateFake explicitly.
func (s *scanReconcileStore) SetWorkspaceImportState(ctx context.Context, id uuid.UUID, status types.WorkspaceStatus, active *uuid.UUID, expectedActive *uuid.UUID, vr json.RawMessage, vh string, va *time.Time) (types.Workspace, bool, error) {
	return s.importStateFake.SetWorkspaceImportState(ctx, id, status, active, expectedActive, vr, vh, va)
}
func (s *scanReconcileStore) UpdateWorkspace(_ context.Context, _ uuid.UUID, ws types.Workspace) (types.Workspace, error) {
	s.fullRowWrite = true
	return ws, nil
}

// TestReconcileWorkspaceRun_StuckScanUsesScopedWrite is the regression:
// the scan-error reconcile branch must use the scoped SetWorkspaceImportState
// (status + cleared active_run_id only), NOT the full-row UpdateWorkspace that
// replayed a stale snapshot over every column. It also honors the same
// newer-run fence the verify branch does.
func TestReconcileWorkspaceRun_StuckScanUsesScopedWrite(t *testing.T) {
	h := newHarness(t)
	runID, wsID := uuid.New(), uuid.New()
	fake := &scanReconcileStore{
		run: types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace scan"},
		importStateFake: importStateFake{ws: types.Workspace{
			ID: wsID, Status: types.WorkspaceScanning, ActiveRunID: &runID,
			// A profile the scoped write must preserve, never revert via a stale replay.
			Profile: mustJSON(map[string]any{"languages": []string{"Go"}}),
		}},
	}
	cfg := baseTestConfig(h, fake)
	cfg.ControlPlaneURL = "http://x"
	s := New(cfg)

	s.reconcileWorkspaceRun(context.Background(), runID)

	if fake.fullRowWrite {
		t.Error("scan reconcile used full-row UpdateWorkspace (the stale-snapshot clobber anti-pattern); it must use the scoped SetWorkspaceImportState like the verify branch")
	}
	if fake.state == nil || fake.state.Status != types.WorkspaceError {
		t.Fatalf("stuck scan should reconcile to error via a scoped write, got %+v", fake.state)
	}
	if fake.state.ActiveRunID != nil {
		t.Error("active_run_id should be cleared")
	}
	if len(fake.state.Profile) == 0 {
		t.Error("scoped write must preserve the concurrently-landed profile, not clobber it")
	}

	// A DIFFERENT run now owns the workspace: this terminal run must not touch it.
	other := uuid.New()
	fake.state = nil
	fake.fullRowWrite = false
	fake.ws = types.Workspace{ID: wsID, Status: types.WorkspaceScanning, ActiveRunID: &other}
	s.reconcileWorkspaceRun(context.Background(), runID)
	if fake.state != nil || fake.fullRowWrite {
		t.Error("a terminal run must not reconcile a workspace owned by a newer run")
	}
}

// ─── capture must not truncate at 1000 events ──────────────────────────

// auditLimitRecordStore wraps recordStore to capture the LIMIT reconcileRecordRun
// passes to QueryAuditEvents. The real store silently caps a 0/negative limit at
// 1000 rows, so an explicit high bound is the load-bearing guarantee.
type auditLimitRecordStore struct {
	*recordStore
	gotLimit int
}

func (s *auditLimitRecordStore) QueryAuditEvents(ctx context.Context, runID uuid.UUID, limit int) ([]types.AuditEvent, error) {
	s.gotLimit = limit
	return s.recordStore.QueryAuditEvents(ctx, runID, limit)
}

// TestReconcileRecordRun_CaptureUsesHighAuditLimit is the regression: a
// recording with >1000 audit events must be captured with an explicit high bound,
// not the silent 1000 default that would truncate the later egress out of the
// derived observations. (The fake returns every event regardless of the limit;
// the assertion is on the LIMIT ARG, which is what the real store honors.)
func TestReconcileRecordRun_CaptureUsesHighAuditLimit(t *testing.T) {
	h := newHarness(t)
	runID, wsID := uuid.New(), uuid.New()
	evs := make([]types.AuditEvent, 0, 1500)
	for i := 0; i < 1500; i++ {
		evs = append(evs, egressAllowEvent(runID, fmt.Sprintf("h%04d.example.com", i)))
	}
	base := &recordStore{
		run:             types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace record", State: types.RunCompleted},
		importStateFake: importStateFake{ws: recordingWorkspace(wsID, runID, "build")},
		events:          evs,
	}
	fake := &auditLimitRecordStore{recordStore: base}
	srv := New(baseTestConfig(h, fake))

	srv.reconcileRecordRun(context.Background(), runID)

	if fake.gotLimit <= 1000 {
		t.Fatalf("capture queried audit events with limit=%d; must pass an explicit high bound (the real store silently caps a 0/negative limit at 1000)", fake.gotLimit)
	}
	if fake.gotLimit != maxCaptureAuditEvents {
		t.Errorf("capture audit limit = %d, want maxCaptureAuditEvents=%d", fake.gotLimit, maxCaptureAuditEvents)
	}
	if res := base.savedResult(t, "build"); res.Status != recordStatusRecorded {
		t.Fatalf("a >1000-event capture should record successfully, got status=%q", res.Status)
	}
}

// ─── launch abort finalizes + revokes the persisted run ────────────────

// recordAbortStore drives launchRecordRun to the CreateGrant-failure abort path
// (after CreateRun) and records the run finalize + slot release the abort must do.
type recordAbortStore struct {
	store.Store
	ws            types.Workspace
	createdRunID  uuid.UUID
	stateFrom     types.RunState
	stateTo       types.RunState
	stateUpdated  bool
	clearedActive bool
	grantErr      error
}

func (s *recordAbortStore) ClaimWorkspaceActiveRun(_ context.Context, _ uuid.UUID, runID uuid.UUID, _ *uuid.UUID) (types.Workspace, bool, error) {
	ws := s.ws
	ws.ActiveRunID = &runID
	return ws, true, nil
}
func (s *recordAbortStore) SetWorkspaceRecordResult(_ context.Context, _ uuid.UUID, _ string, _ json.RawMessage, _ string) (types.Workspace, bool, error) {
	return s.ws, true, nil
}
func (s *recordAbortStore) CreateRun(_ context.Context, run types.AgentRun) (types.AgentRun, error) {
	s.createdRunID = run.ID
	return run, nil
}
func (s *recordAbortStore) CreateGrant(_ context.Context, _ types.CredentialGrant) (types.CredentialGrant, error) {
	return types.CredentialGrant{}, s.grantErr
}
func (s *recordAbortStore) UpdateRunStateIf(_ context.Context, _ uuid.UUID, from, to types.RunState) (bool, error) {
	s.stateFrom, s.stateTo, s.stateUpdated = from, to, true
	return true, nil
}
func (s *recordAbortStore) ClearWorkspaceActiveRun(_ context.Context, _ uuid.UUID, _ uuid.UUID) (bool, error) {
	s.clearedActive = true
	return true, nil
}

// TestLaunchRecordRun_CreateGrantFailureFinalizesRun is the regression: when
// CreateGrant fails AFTER CreateRun, the persisted RunPending run must be
// finalized RunFailed and the revoke cascade must run (broker revocation of the
// minted run) — not left orphaned with un-revoked grants. The abort happens
// before dispatch, so no sandbox goroutine is spawned.
func TestLaunchRecordRun_CreateGrantFailureFinalizesRun(t *testing.T) {
	h := newHarness(t)
	wsID := uuid.New()
	fake := &recordAbortStore{
		ws:       types.Workspace{ID: wsID, Kind: types.WorkspaceKindLocalDir, Source: "/w", Status: types.WorkspaceScanned},
		grantErr: errors.New("grant store down"),
	}
	cfg := baseTestConfig(h, fake)
	cfg.Runner = &fakeRunner{}
	cfg.Broker = h.broker
	// A present provider secret + a ceiling that does NOT bless a subscription mount
	// forces the api-key branch, whose CreateGrant then fails.
	cfg.Secrets = &memSecrets{m: map[string][]byte{"anthropic-api-key": []byte("sk-ant-test")}}
	cfg.DefaultPolicy = types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, MinConfinementClass: types.CC2}
	srv := New(cfg)

	_, _, err := srv.launchRecordRun(context.Background(), "alice@example.com", fake.ws, "build", "build", recordModeInteractive, false)
	if err == nil {
		t.Fatal("expected launchRecordRun to fail when CreateGrant errors")
	}
	if !fake.stateUpdated || fake.stateFrom != types.RunPending || fake.stateTo != types.RunFailed {
		t.Errorf("aborted launch must finalize the run RunPending→RunFailed; got updated=%v from=%q to=%q",
			fake.stateUpdated, fake.stateFrom, fake.stateTo)
	}
	if len(h.broker.revoked) == 0 || h.broker.revoked[len(h.broker.revoked)-1] != fake.createdRunID {
		t.Errorf("aborted launch must run the revoke cascade for run %s; broker.revoked=%v", fake.createdRunID, h.broker.revoked)
	}
	if !fake.clearedActive {
		t.Error("aborted launch must release the workspace import-step slot")
	}
}

// ─── verify/scan settle when the run goes terminal during dispatch ──────

// TestSettleTerminalLaunch_StuckVerifyRunSettles is the regression for the
// synchronous path: a verify run CAS'd to terminal FAILED during dispatch (before
// Exec, so no completion watcher and no reconcile hook ever fires) must settle its
// workspace out of `verifying` — record already self-healed here, verify/scan did
// not. The counterfactual: without the settleTerminalLaunch call the 3 launch fns
// now make, the workspace stays `verifying` forever (fake.state stays nil).
func TestSettleTerminalLaunch_StuckVerifyRunSettles(t *testing.T) {
	h := newHarness(t)
	runID, wsID := uuid.New(), uuid.New()
	fake := &scanReconcileStore{
		run: types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace verify", State: types.RunFailed},
		importStateFake: importStateFake{ws: types.Workspace{
			ID: wsID, Status: types.WorkspaceVerifying, ActiveRunID: &runID,
		}},
	}
	cfg := baseTestConfig(h, fake)
	cfg.ControlPlaneURL = "http://x"
	s := New(cfg)

	s.settleTerminalLaunch(context.Background(), runID, fake.run)

	if fake.state == nil || fake.state.Status != types.WorkspaceVerifyFailed {
		t.Fatalf("a dispatch-time terminal verify run must settle the workspace to verify_failed; got %+v", fake.state)
	}
	if fake.state.ActiveRunID != nil {
		t.Error("settle must clear active_run_id")
	}
}

// TestSettleTerminalLaunch_NonTerminalRunNoOp guards the other direction: a run
// still coming up RUNNING must NOT be settled — its terminal transition belongs to
// the completion watcher/kill, and settling it here would double-finalize.
func TestSettleTerminalLaunch_NonTerminalRunNoOp(t *testing.T) {
	h := newHarness(t)
	runID, wsID := uuid.New(), uuid.New()
	fake := &scanReconcileStore{
		run: types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace verify", State: types.RunRunning},
		importStateFake: importStateFake{ws: types.Workspace{
			ID: wsID, Status: types.WorkspaceVerifying, ActiveRunID: &runID,
		}},
	}
	s := New(baseTestConfig(h, fake))

	s.settleTerminalLaunch(context.Background(), runID, fake.run)

	if fake.state != nil {
		t.Errorf("a still-RUNNING run must not be settled (the watcher owns its terminal transition); got %+v", fake.state)
	}
}

// TestRepairStaleWorkspaceRuns_HealsStuckVerify is the regression for the
// crash-window catch-all: if wardynd died between the terminal CAS and the
// synchronous settle, the next status read must heal a workspace stuck `verifying`
// behind a terminal run — the same repair-on-read record already had.
func TestRepairStaleWorkspaceRuns_HealsStuckVerify(t *testing.T) {
	h := newHarness(t)
	runID, wsID := uuid.New(), uuid.New()
	fake := &scanReconcileStore{
		run: types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace verify", State: types.RunFailed},
		importStateFake: importStateFake{ws: types.Workspace{
			ID: wsID, Status: types.WorkspaceVerifying, ActiveRunID: &runID,
		}},
	}
	s := New(baseTestConfig(h, fake))

	s.repairStaleWorkspaceRuns(context.Background(), fake.ws)

	if fake.state == nil || fake.state.Status != types.WorkspaceVerifyFailed {
		t.Fatalf("repair-on-read must heal a workspace stuck `verifying` behind a terminal run; got %+v", fake.state)
	}
}
