// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// TestUploadScanResult_CrossRunRejected mirrors the recording upload's cross-run
// guard: a run token minted for run A must not be able to PUT scan facts under
// run B's id. The run-id mismatch is rejected (403) BEFORE any workspace lookup or
// body parse, so this holds with no Store wired.
func TestUploadScanResult_CrossRunRejected(t *testing.T) {
	h := newHarness(t)
	tok := h.mintRunToken(t, uuid.New())
	otherRun := uuid.New()
	w := do(t, h.srv, http.MethodPut,
		"/api/v1/internal/scan-results/"+otherRun.String(), tok, `{"has_devcontainer":true}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-run scan upload: code = %d, want 403", w.Code)
	}
}

// scanRunStore is a minimal store.Store returning a fixed run from GetRun (the
// only method the not-a-scan-run guard needs); every other method would panic.
type scanRunStore struct {
	store.Store
	run types.AgentRun
}

func (s scanRunStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) { return s.run, nil }

// TestUploadScanResult_NonScanRunRejected: the run→workspace linkage is TRUSTED
// server state (run.WorkspaceID), not sandbox input. A matched-run upload from a
// run that is NOT a governed scan run (nil WorkspaceID) fails closed (403) — an
// ordinary run has no business uploading scan facts.
func TestUploadScanResult_NonScanRunRejected(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	tok := h.mintRunToken(t, runID)
	// Same identity as the token, but a Store returning an ordinary run (no
	// WorkspaceID), so the not-a-scan-run guard is reachable.
	srv := New(baseTestConfig(h, scanRunStore{run: types.AgentRun{ID: runID}})) // WorkspaceID == nil
	w := do(t, srv, http.MethodPut,
		"/api/v1/internal/scan-results/"+runID.String(), tok, `{"has_devcontainer":true}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-scan-run upload: code = %d, want 403; body=%s", w.Code, w.Body.String())
	}
}

// scanUploadStore is the full happy-path store: a governed scan run (Task
// "workspace scan" + WorkspaceID) plus the workspace row the handler updates. It
// captures the persisted workspace so tests can assert on the derived profile.
type scanUploadStore struct {
	store.Store
	run   types.AgentRun
	ws    types.Workspace
	saved *types.Workspace
}

func (s *scanUploadStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	return s.run, nil
}
func (s *scanUploadStore) GetWorkspace(context.Context, uuid.UUID) (types.Workspace, error) {
	return s.ws, nil
}
func (s *scanUploadStore) UpdateWorkspace(_ context.Context, _ uuid.UUID, ws types.Workspace) (types.Workspace, error) {
	s.saved = &ws
	return ws, nil
}

// SetWorkspaceScanResult is the scoped, slot-releasing profile write the upload
// handler now uses instead of the full-row UpdateWorkspace: it captures the
// derived profile, flips status=scanned, and clears the active-run pointer.
func (s *scanUploadStore) SetWorkspaceScanResult(_ context.Context, _ uuid.UUID, profile json.RawMessage, _ uuid.UUID) (types.Workspace, bool, error) {
	ws := s.ws
	ws.Profile = profile
	ws.Status = types.WorkspaceScanned
	ws.ActiveRunID = nil
	s.saved = &ws
	return ws, true, nil
}

// newScanUploadSrv wires a Server over a scanUploadStore with the given advisor
// seam (nil = feature off) and returns a valid run token for the scan run.
func newScanUploadSrv(t *testing.T, adv func(context.Context, workspacescan.ScanFacts, workspacescan.WorkspaceProfile) workspacescan.WorkspaceProfile) (*Server, *scanUploadStore, string, uuid.UUID) {
	t.Helper()
	h := newHarness(t)
	wsID, runID := uuid.New(), uuid.New()
	st := &scanUploadStore{
		run: types.AgentRun{ID: runID, Task: "workspace scan", WorkspaceID: &wsID},
		// active_run_id == the scan run: the upload handler's fence requires the run
		// to still own the workspace's import-step slot.
		ws: types.Workspace{ID: wsID, Kind: types.WorkspaceKindLocalDir, Source: "/w", Status: types.WorkspaceScanning, ActiveRunID: &runID},
	}
	cfg := baseTestConfig(h, st)
	cfg.ScanAIAdvisor = adv
	srv := New(cfg)
	return srv, st, h.mintRunToken(t, runID), wsID
}

// facts that make ShouldAdvise() true (an unrecognized build sample) yet also
// carry a deterministic fact (a go.mod → language "Go", tools left empty) so the
// add-only test can prove the advisor never clobbers a deterministic fact.
const scanAdviseFacts = `{"manifests_found":[{"path":"go.mod","marker":"go.mod"}],"unrecognized_samples":[{"path":"BUILD.mystery","content":"cc_binary(name=\"x\")"}]}`

func auditAI(t *testing.T, evs []types.AuditEvent) (ran, changed bool) {
	t.Helper()
	for _, ev := range evs {
		if ev.Action != "workspace.scan" || ev.Outcome != "success" {
			continue
		}
		var d struct {
			AIAdvisor bool `json:"ai_advisor"`
			AIChanged bool `json:"ai_changed"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatalf("audit data: %v", err)
		}
		return d.AIAdvisor, d.AIChanged
	}
	t.Fatalf("no workspace.scan success audit event in %d events", len(evs))
	return
}

// (a) DISABLED (nil advisor) => the persisted profile is byte-identical to the
// deterministic DeriveProfile and no advisor runs (ai_advisor=false).
func TestUploadScanResult_AIDisabled_ByteIdentical(t *testing.T) {
	srv, st, tok, wsID := newScanUploadSrv(t, nil)
	if w := do(t, srv, http.MethodPut, "/api/v1/internal/scan-results/"+st.run.ID.String(), tok, scanAdviseFacts); w.Code != http.StatusNoContent {
		t.Fatalf("upload code = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	var facts workspacescan.ScanFacts
	_ = json.Unmarshal([]byte(scanAdviseFacts), &facts)
	want := mustJSON(workspacescan.DeriveProfile(facts))
	if st.saved == nil || string(st.saved.Profile) != string(want) {
		t.Fatalf("disabled profile not byte-identical to deterministic derive\n got=%s\nwant=%s", st.saved.Profile, want)
	}
	if ran, _ := auditAI(t, srv.cfg.Audit.(*recRecorder).events); ran {
		t.Fatalf("ai_advisor=true with the advisor disabled")
	}
	_ = wsID
}

// (b) ENABLED + advisor FAILS OPEN (returns base unchanged, as AdviseProfile does
// on any error) => upload still 204, profile unchanged, ai_advisor=true but
// ai_changed=false.
func TestUploadScanResult_AIFailOpen(t *testing.T) {
	invoked := false
	adv := func(_ context.Context, _ workspacescan.ScanFacts, base workspacescan.WorkspaceProfile) workspacescan.WorkspaceProfile {
		invoked = true
		return base // fail-open: base returned unchanged
	}
	srv, st, tok, _ := newScanUploadSrv(t, adv)
	if w := do(t, srv, http.MethodPut, "/api/v1/internal/scan-results/"+st.run.ID.String(), tok, scanAdviseFacts); w.Code != http.StatusNoContent {
		t.Fatalf("upload code = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if !invoked {
		t.Fatal("advisor was not invoked though enabled + ShouldAdvise")
	}
	var facts workspacescan.ScanFacts
	_ = json.Unmarshal([]byte(scanAdviseFacts), &facts)
	if want := mustJSON(workspacescan.DeriveProfile(facts)); string(st.saved.Profile) != string(want) {
		t.Fatalf("fail-open must leave profile unchanged\n got=%s\nwant=%s", st.saved.Profile, want)
	}
	ran, changed := auditAI(t, srv.cfg.Audit.(*recRecorder).events)
	if !ran || changed {
		t.Fatalf("audit ai_advisor/ai_changed = %v/%v, want true/false", ran, changed)
	}
}

// (c) ENABLED + advisor RETURNS ADDITIONS => the add-only merge is persisted
// (deterministic HasDockerfile preserved, empty Tools gap-filled), NeedsReview is
// raised, and ai_changed=true. The fake stands in for AdviseProfile's merged
// output (ai.go's merge rules themselves are covered by ai_test.go).
func TestUploadScanResult_AIAdditions(t *testing.T) {
	adv := func(_ context.Context, _ workspacescan.ScanFacts, base workspacescan.WorkspaceProfile) workspacescan.WorkspaceProfile {
		out := base
		if len(out.Tools) == 0 {
			out.Tools = []string{"bazel"}
			out.NeedsReview = true
			out.Source = workspacescan.SourceAIAssisted
		}
		return out
	}
	srv, st, tok, _ := newScanUploadSrv(t, adv)
	if w := do(t, srv, http.MethodPut, "/api/v1/internal/scan-results/"+st.run.ID.String(), tok, scanAdviseFacts); w.Code != http.StatusNoContent {
		t.Fatalf("upload code = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	var got workspacescan.WorkspaceProfile
	if err := json.Unmarshal(st.saved.Profile, &got); err != nil {
		t.Fatalf("persisted profile: %v", err)
	}
	if len(got.Tools) != 1 || got.Tools[0] != "bazel" {
		t.Fatalf("advisor addition not persisted: tools=%v", got.Tools)
	}
	if len(got.Languages) != 1 || got.Languages[0] != "Go" {
		t.Fatalf("add-only violated: deterministic language fact lost: %v", got.Languages)
	}
	if !got.NeedsReview || got.Source != workspacescan.SourceAIAssisted {
		t.Fatalf("needs_review/source = %v/%q, want true/ai_assisted", got.NeedsReview, got.Source)
	}
	if ran, changed := auditAI(t, srv.cfg.Audit.(*recRecorder).events); !ran || !changed {
		t.Fatalf("audit ai_advisor/ai_changed = %v/%v, want true/true", ran, changed)
	}
}

// a scan upload whose run no longer owns the workspace's import-step slot
// (active_run_id points at a DIFFERENT run) is fenced with 409 and writes nothing —
// a superseded / lagging scan can never clobber a fresher profile. Mirrors the
// verify lane's superseded-run fence.
func TestUploadScanResult_SupersededRunFenced(t *testing.T) {
	h := newHarness(t)
	wsID, runID, other := uuid.New(), uuid.New(), uuid.New()
	st := &scanUploadStore{
		run: types.AgentRun{ID: runID, Task: "workspace scan", WorkspaceID: &wsID},
		ws:  types.Workspace{ID: wsID, Kind: types.WorkspaceKindLocalDir, Source: "/w", Status: types.WorkspaceScanning, ActiveRunID: &other},
	}
	srv := New(baseTestConfig(h, st))
	tok := h.mintRunToken(t, runID)
	w := do(t, srv, http.MethodPut, "/api/v1/internal/scan-results/"+runID.String(), tok, `{"has_devcontainer":true}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("superseded scan upload: code = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	if st.saved != nil {
		t.Errorf("a fenced upload must persist nothing, got %+v", st.saved)
	}
}

// a successful scan upload RELEASES the import-step slot (clears
// active_run_id) so the scan-run reconcile self-heal correctly no-ops once the
// profile has landed — otherwise the slot leaks and reconcile can later mark a
// successfully-scanned workspace `error`.
func TestUploadScanResult_SuccessClearsActiveRun(t *testing.T) {
	srv, st, tok, _ := newScanUploadSrv(t, nil)
	if w := do(t, srv, http.MethodPut, "/api/v1/internal/scan-results/"+st.run.ID.String(), tok, scanAdviseFacts); w.Code != http.StatusNoContent {
		t.Fatalf("upload code = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if st.saved == nil {
		t.Fatal("success must persist the derived profile")
	}
	if st.saved.Status != types.WorkspaceScanned {
		t.Errorf("success status = %q, want scanned", st.saved.Status)
	}
	if st.saved.ActiveRunID != nil {
		t.Errorf("success must release the import-step slot (active_run_id cleared), got %v", st.saved.ActiveRunID)
	}
}
