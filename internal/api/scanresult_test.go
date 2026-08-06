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

// TestUploadScanResult_NonScanRunRejected: the run→workspace/source linkage is
// TRUSTED server state (run.WorkspaceID/SourceID), not sandbox input. A
// matched-run upload from a run that is NOT governed at all (neither id set)
// fails closed (403) — an ordinary run has no business uploading scan facts.
func TestUploadScanResult_NonScanRunRejected(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	tok := h.mintRunToken(t, runID)
	// Same identity as the token, but a Store returning an ordinary run (no
	// WorkspaceID/SourceID), so the not-a-scan-run guard is reachable.
	srv := New(baseTestConfig(h, scanRunStore{run: types.AgentRun{ID: runID}}))
	w := do(t, srv, http.MethodPut,
		"/api/v1/internal/scan-results/"+runID.String(), tok, `{"has_devcontainer":true}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-scan-run upload: code = %d, want 403; body=%s", w.Code, w.Body.String())
	}
}

// TestUploadScanResult_WorkspaceTaskRejected pins the narrowed task gate: a
// "workspace scan" run — impossible in production since the three-tier
// retarget (every scan run now carries a SourceID and the task "source
// scan") — is refused here too instead of being silently routed to what used
// to be a dead workspace lane.
func TestUploadScanResult_WorkspaceTaskRejected(t *testing.T) {
	h := newHarness(t)
	wsID, runID := uuid.New(), uuid.New()
	srv := New(baseTestConfig(h, scanRunStore{run: types.AgentRun{ID: runID, Task: "workspace scan", WorkspaceID: &wsID}}))
	tok := h.mintRunToken(t, runID)
	w := do(t, srv, http.MethodPut,
		"/api/v1/internal/scan-results/"+runID.String(), tok, `{"has_devcontainer":true}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("workspace-task scan upload: code = %d, want 403 (task gate is source scan only); body=%s", w.Code, w.Body.String())
	}
}

// sourceScanUploadStore is the full happy-path store for the SOURCE lane: a
// governed scan run (Task "source scan" + SourceID) plus the fenced
// SetSourceScanResult write uploadSourceScanResult uses. Captures the
// persisted profile so tests can assert on it.
type sourceScanUploadStore struct {
	store.Store
	run    types.AgentRun
	saved  json.RawMessage
	status types.WorkspaceStatus
}

func (s *sourceScanUploadStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	return s.run, nil
}

func (s *sourceScanUploadStore) SetSourceScanResult(_ context.Context, id uuid.UUID, profile []byte, status types.WorkspaceStatus, _ uuid.UUID, _ map[string]types.WorkspaceRequirement) (types.Source, error) {
	s.saved = profile
	s.status = status
	return types.Source{ID: id, Status: status}, nil
}

// newSourceScanUploadSrv wires a Server over a sourceScanUploadStore with the
// given advisor seam (nil = feature off) and returns a valid run token for
// the scan run.
func newSourceScanUploadSrv(t *testing.T, adv func(context.Context, workspacescan.ScanFacts, workspacescan.WorkspaceProfile) workspacescan.WorkspaceProfile) (*Server, *sourceScanUploadStore, string) {
	t.Helper()
	h := newHarness(t)
	sourceID, runID := uuid.New(), uuid.New()
	st := &sourceScanUploadStore{run: types.AgentRun{ID: runID, Task: "source scan", SourceID: &sourceID}}
	cfg := baseTestConfig(h, st)
	cfg.ScanAIAdvisor = adv
	srv := New(cfg)
	return srv, st, h.mintRunToken(t, runID)
}

// facts that make ShouldAdvise() true (an unrecognized build sample) yet also
// carry a deterministic fact (a go.mod → language "Go", tools left empty) so the
// add-only test can prove the advisor never clobbers a deterministic fact.
const scanAdviseFacts = `{"manifests_found":[{"path":"go.mod","marker":"go.mod"}],"unrecognized_samples":[{"path":"BUILD.mystery","content":"cc_binary(name=\"x\")"}]}`

func auditAI(t *testing.T, evs []types.AuditEvent) (ran, changed bool) {
	t.Helper()
	for _, ev := range evs {
		if ev.Action != "source.scan" || ev.Outcome != "success" {
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
	t.Fatalf("no source.scan success audit event in %d events", len(evs))
	return
}

// (a) DISABLED (nil advisor) => the persisted profile is byte-identical to the
// deterministic DeriveProfile and no advisor runs (ai_advisor=false).
func TestUploadScanResult_AIDisabled_ByteIdentical(t *testing.T) {
	srv, st, tok := newSourceScanUploadSrv(t, nil)
	if w := do(t, srv, http.MethodPut, "/api/v1/internal/scan-results/"+st.run.ID.String(), tok, scanAdviseFacts); w.Code != http.StatusOK {
		t.Fatalf("upload code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var facts workspacescan.ScanFacts
	_ = json.Unmarshal([]byte(scanAdviseFacts), &facts)
	want := mustJSON(workspacescan.DeriveProfile(facts))
	if st.saved == nil || string(st.saved) != string(want) {
		t.Fatalf("disabled profile not byte-identical to deterministic derive\n got=%s\nwant=%s", st.saved, want)
	}
	if ran, _ := auditAI(t, srv.cfg.Audit.(*recRecorder).events); ran {
		t.Fatalf("ai_advisor=true with the advisor disabled")
	}
}

// (b) ENABLED + advisor FAILS OPEN (returns base unchanged, as AdviseProfile does
// on any error) => upload still 200, profile unchanged, ai_advisor=true but
// ai_changed=false.
func TestUploadScanResult_AIFailOpen(t *testing.T) {
	invoked := false
	adv := func(_ context.Context, _ workspacescan.ScanFacts, base workspacescan.WorkspaceProfile) workspacescan.WorkspaceProfile {
		invoked = true
		return base // fail-open: base returned unchanged
	}
	srv, st, tok := newSourceScanUploadSrv(t, adv)
	if w := do(t, srv, http.MethodPut, "/api/v1/internal/scan-results/"+st.run.ID.String(), tok, scanAdviseFacts); w.Code != http.StatusOK {
		t.Fatalf("upload code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !invoked {
		t.Fatal("advisor was not invoked though enabled + ShouldAdvise")
	}
	var facts workspacescan.ScanFacts
	_ = json.Unmarshal([]byte(scanAdviseFacts), &facts)
	if want := mustJSON(workspacescan.DeriveProfile(facts)); string(st.saved) != string(want) {
		t.Fatalf("fail-open must leave profile unchanged\n got=%s\nwant=%s", st.saved, want)
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
	srv, st, tok := newSourceScanUploadSrv(t, adv)
	if w := do(t, srv, http.MethodPut, "/api/v1/internal/scan-results/"+st.run.ID.String(), tok, scanAdviseFacts); w.Code != http.StatusOK {
		t.Fatalf("upload code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got workspacescan.WorkspaceProfile
	if err := json.Unmarshal(st.saved, &got); err != nil {
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
