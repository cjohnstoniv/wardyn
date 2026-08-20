// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/groundtruth"
	"github.com/cjohnstoniv/wardyn/internal/recordmode"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// importStateFake implements store.Store's SetWorkspaceImportState by mutating
// a copy of the embedded workspace snapshot and recording the write. The
// scan/record test fakes below otherwise duplicated this method
// byte-for-byte, so they embed this instead.
type importStateFake struct {
	ws    types.Workspace
	state *types.Workspace // last SetWorkspaceImportState write (nil = untouched)
}

// SetWorkspaceImportState models the REAL store's fence: the write applies only
// while the import-step slot still holds expectedActive (SQL: active_run_id IS
// NOT DISTINCT FROM $4, so nil matches NULL). A guard miss returns the current
// row with applied=false and writes NOTHING — tests that assert a stale writer
// is refused depend on this, so keep it faithful to store.go.
func (s *importStateFake) SetWorkspaceImportState(_ context.Context, _ uuid.UUID, status types.WorkspaceStatus, active *uuid.UUID, expectedActive *uuid.UUID) (types.Workspace, bool, error) {
	cur := s.ws
	if s.state != nil {
		cur = *s.state // reflect any earlier applied write
	}
	if !samePtrUUID(cur.ActiveRunID, expectedActive) {
		return cur, false, nil
	}
	ws := cur
	ws.Status = status
	ws.ActiveRunID = active
	s.state = &ws
	return ws, true, nil
}

// samePtrUUID is IS NOT DISTINCT FROM for *uuid.UUID (nil == nil).
func samePtrUUID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// recordStore fakes exactly the store surface the record lane touches.
type recordStore struct {
	store.Store
	importStateFake
	run           types.AgentRun
	events        []types.AuditEvent
	grants        []types.CredentialGrant
	saved         json.RawMessage // current record_results blob (per-key upserts land here)
	claimedRun    *uuid.UUID      // last ClaimWorkspaceActiveRun run id
	clearedActive bool            // ClearWorkspaceActiveRun called
	// heartbeat backs LatestAuditEventByAction — reconcileRecordRun's
	// ebpfGroundtruthCaveat call (W20-W20-groundtruth-mapper-4) reads this to
	// stamp the sensor's coverage state onto the capture. nil (the default)
	// means "no heartbeat ever" (ErrNotFound), same as a host with no sensor.
	heartbeat *types.AuditEvent
	// mergeErr, when set, is what MergeWorkspaceRequirements returns instead
	// of applying `add` — the bug-record-1 regression seam (a store error on
	// the widening AFTER the promotion marker CAS already committed).
	mergeErr error
}

// LatestAuditEventByAction backs ebpfGroundtruthCaveat's heartbeat lookup.
// Every recordStore-based test now exercises this (reconcileRecordRun calls
// it unconditionally on every capture) — default to "no heartbeat ever" so
// the many existing tests that don't care about sensor health stay
// unaffected; a test that DOES sets s.heartbeat first.
func (s *recordStore) LatestAuditEventByAction(context.Context, string) (types.AuditEvent, error) {
	if s.heartbeat != nil {
		return *s.heartbeat, nil
	}
	return types.AuditEvent{}, store.ErrNotFound
}

func (s *recordStore) GetWorkspace(context.Context, uuid.UUID) (types.Workspace, error) {
	ws := s.ws
	if s.saved != nil {
		ws.RecordResults = s.saved
	}
	return ws, nil
}

// SetWorkspaceImportState resolves the otherwise-ambiguous selector between the
// embedded nil store.Store interface and importStateFake (both declare this
// method) by routing to importStateFake's shared implementation explicitly.
func (s *recordStore) SetWorkspaceImportState(ctx context.Context, id uuid.UUID, status types.WorkspaceStatus, active *uuid.UUID, expectedActive *uuid.UUID) (types.Workspace, bool, error) {
	return s.importStateFake.SetWorkspaceImportState(ctx, id, status, active, expectedActive)
}

// MergeWorkspaceRequirements mirrors the real scoped writer: atomic || into
// the overlay, guarded by the 256-key cap (ErrConflict past it).
func (s *recordStore) MergeWorkspaceRequirements(_ context.Context, _ uuid.UUID, add map[string]types.WorkspaceRequirement) (types.Workspace, error) {
	if s.mergeErr != nil {
		return types.Workspace{}, s.mergeErr
	}
	if len(s.ws.Requirements) >= 256 {
		return types.Workspace{}, store.ErrConflict
	}
	if s.ws.Requirements == nil {
		s.ws.Requirements = map[string]types.WorkspaceRequirement{}
	}
	for k, v := range add {
		s.ws.Requirements[k] = v
	}
	return s.ws, nil
}

func (s *recordStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	if s.run.ID == uuid.Nil {
		return types.AgentRun{}, store.ErrNotFound
	}
	return s.run, nil
}
func (s *recordStore) QueryAuditEvents(context.Context, uuid.UUID, int) ([]types.AuditEvent, error) {
	return s.events, nil
}
func (s *recordStore) ListGrantsByRun(context.Context, uuid.UUID) ([]types.CredentialGrant, error) {
	return s.grants, nil
}

// SetWorkspaceRecordResult mirrors the store's per-key upsert + status-guard
// (CAS) semantics so guard behavior is exercisable through the fake.
func (s *recordStore) SetWorkspaceRecordResult(_ context.Context, _ uuid.UUID, task string, blob json.RawMessage, onlyIfStatus string) (types.Workspace, bool, error) {
	cur := s.saved
	if cur == nil {
		cur = s.ws.RecordResults
	}
	m := map[string]json.RawMessage{}
	if len(cur) > 0 {
		_ = json.Unmarshal(cur, &m)
	}
	if onlyIfStatus != "" {
		var entry struct {
			Status string `json:"status"`
		}
		if raw, ok := m[task]; ok {
			_ = json.Unmarshal(raw, &entry)
		}
		if entry.Status != onlyIfStatus {
			ws := s.ws
			ws.RecordResults = cur
			return ws, false, nil
		}
	}
	m[task] = blob
	s.saved = mustJSON(m)
	ws := s.ws
	ws.RecordResults = s.saved
	return ws, true, nil
}
func (s *recordStore) ClaimWorkspaceActiveRun(_ context.Context, _ uuid.UUID, runID uuid.UUID, _ *uuid.UUID) (types.Workspace, bool, error) {
	s.claimedRun = &runID
	ws := s.ws
	ws.ActiveRunID = &runID
	return ws, true, nil
}
func (s *recordStore) ClearWorkspaceActiveRun(_ context.Context, _ uuid.UUID, _ uuid.UUID) (bool, error) {
	s.clearedActive = true
	return true, nil
}
func (s *recordStore) SetWorkspaceBuiltImage(_ context.Context, _ uuid.UUID, imageRef, hash string) (types.Workspace, error) {
	ws := s.ws
	ws.ImageRef, ws.BuiltProfileHash = imageRef, hash
	return ws, nil
}
func (s *recordStore) SetWorkspaceApprovedEgress(_ context.Context, _ uuid.UUID, domains []string) (types.Workspace, error) {
	s.ws.ApprovedEgress = domains
	return s.ws, nil
}

func (s *recordStore) savedResult(t *testing.T, task string) RecordTaskResult {
	t.Helper()
	m := map[string]RecordTaskResult{}
	if err := json.Unmarshal(s.saved, &m); err != nil {
		t.Fatalf("unmarshal saved record_results: %v (blob=%s)", err, s.saved)
	}
	res, ok := m[task]
	if !ok {
		t.Fatalf("task %q not in saved record_results %s", task, s.saved)
	}
	return res
}

// recordingWorkspace returns a workspace with one in-flight recording for task.
func recordingWorkspace(wsID, runID uuid.UUID, task string) types.Workspace {
	return types.Workspace{
		ID:      wsID,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/w", Target: "/home/agent/work"}},
		Status:  types.WorkspaceScanned, ActiveRunID: &runID,
		RecordResults: mustJSON(map[string]RecordTaskResult{
			task: {RunID: runID, Mode: "auto", Status: recordStatusRecording},
		}),
	}
}

func egressAllowEvent(runID uuid.UUID, host string) types.AuditEvent {
	return types.AuditEvent{RunID: &runID, Action: "egress.allow", Outcome: "success",
		Target: host, Data: mustJSON(map[string]any{"host": host, "method": "GET"})}
}

// newTestSrv builds a bare *Server over a caller-supplied fake store, reusing
// newHarness's identity/audit wiring. Shared by the record-endpoint tests
// below (formerly named newVerifySrv — a generic helper, not verify-specific).
func newTestSrv(t *testing.T, fake store.Store) *Server {
	h := newHarness(t)
	return New(baseTestConfig(h, fake))
}

func TestReconcileRecordRun_EmptyCaptureIsFailureNeverNoEgress(t *testing.T) {
	h := newHarness(t)
	runID, wsID := uuid.New(), uuid.New()
	fake := &recordStore{
		run:             types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace record", State: types.RunCompleted},
		importStateFake: importStateFake{ws: recordingWorkspace(wsID, runID, "build")},
	}
	srv := New(baseTestConfig(h, fake))

	srv.reconcileRecordRun(context.Background(), runID)

	res := fake.savedResult(t, "build")
	if res.Status != recordStatusFailed {
		t.Errorf("status = %q, want record_failed (zero evidence is a FAILURE, not 'needs no egress')", res.Status)
	}
	if res.FailureHint == "" {
		t.Error("empty capture must carry the control-plane-reachability failure hint")
	}
	if len(res.Caveats) == 0 {
		t.Error("capture must carry the seed-ahead masking caveat")
	}
	if !fake.clearedActive {
		t.Error("active_run_id should be conditionally cleared (ClearWorkspaceActiveRun)")
	}
	if fake.state != nil {
		t.Errorf("record touched SetWorkspaceImportState (status/verify fields) — must never: %+v", fake.state)
	}
}

// TestReconcileRecordRun_NeverStartedGetsDispatchHintNotNetworkingGuess is
// W20-W20-capture-store-4: a record run whose SandboxRef is EMPTY never
// reached CreateSandbox at all — it failed on the dispatch side (image build,
// resource limits, a concurrent kill racing dispatch), never got a chance to
// observe egress. Blaming the operator's proxy/WSL2 networking
// (recordEmptyCaptureHint) sends them on a wasted mirrored-networking detour
// for a session that never even tried to reach the control plane; the hint
// must instead name the actual dispatch-side failure.
func TestReconcileRecordRun_NeverStartedGetsDispatchHintNotNetworkingGuess(t *testing.T) {
	h := newHarness(t)
	runID, wsID := uuid.New(), uuid.New()
	fake := &recordStore{
		run: types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace record", State: types.RunFailed}, // SandboxRef: "" (never dispatched)
		events: []types.AuditEvent{
			{RunID: &runID, Action: "run.dispatch", Outcome: "failure",
				Data: mustJSON(map[string]any{"error": "resolve workspace image: build timed out"})},
		},
		importStateFake: importStateFake{ws: recordingWorkspace(wsID, runID, "build")},
	}
	srv := New(baseTestConfig(h, fake))

	srv.reconcileRecordRun(context.Background(), runID)

	res := fake.savedResult(t, "build")
	if res.Status != recordStatusFailed {
		t.Fatalf("status = %q, want record_failed", res.Status)
	}
	if res.FailureHint == recordEmptyCaptureHint {
		t.Fatalf("a run that never started got the networking-guess hint instead of the dispatch failure reason: %q", res.FailureHint)
	}
	if !strings.Contains(res.FailureHint, "build timed out") {
		t.Errorf("failure_hint = %q, want it to name the audited dispatch failure (%q)", res.FailureHint, "build timed out")
	}
}

// TestReconcileRecordRun_ReachedRunningKeepsNetworkingHint is the
// counterpart: a run that actually got a sandbox (SandboxRef set) and STILL
// captured zero egress evidence keeps the networking-reachability hint — that
// guess is honest for a run that reached RUNNING.
func TestReconcileRecordRun_ReachedRunningKeepsNetworkingHint(t *testing.T) {
	h := newHarness(t)
	runID, wsID := uuid.New(), uuid.New()
	fake := &recordStore{
		run:             types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace record", State: types.RunCompleted, SandboxRef: "agent-run-abc"},
		importStateFake: importStateFake{ws: recordingWorkspace(wsID, runID, "build")},
	}
	srv := New(baseTestConfig(h, fake))

	srv.reconcileRecordRun(context.Background(), runID)

	res := fake.savedResult(t, "build")
	if res.FailureHint != recordEmptyCaptureHint {
		t.Errorf("failure_hint = %q, want the networking-reachability hint for a run that reached RUNNING", res.FailureHint)
	}
}

func TestReconcileRecordRun_CapturesObservationsAndSecretNames(t *testing.T) {
	h := newHarness(t)
	runID, wsID, grantID := uuid.New(), uuid.New(), uuid.New()
	fake := &recordStore{
		run: types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace record",
			State: types.RunCompleted, ConfinementClass: types.CC3},
		importStateFake: importStateFake{ws: recordingWorkspace(wsID, runID, "build")},
		events: []types.AuditEvent{
			egressAllowEvent(runID, "registry.npmjs.org"),
			egressAllowEvent(runID, "api.stripe.com"),
			{RunID: &runID, Action: "credential.mint", Outcome: "success",
				Data: mustJSON(map[string]any{"grant_id": grantID.String()})},
		},
		grants: []types.CredentialGrant{{ID: grantID, RunID: runID,
			Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: mustJSON(map[string]any{"secret_name": "STRIPE_SECRET_KEY"})}}},
	}
	srv := New(baseTestConfig(h, fake))

	srv.reconcileRecordRun(context.Background(), runID)

	res := fake.savedResult(t, "build")
	if res.Status != recordStatusRecorded {
		t.Fatalf("status = %q, want recorded (hint=%s)", res.Status, res.FailureHint)
	}
	if res.Observations == nil || len(res.Observations.Domains) != 2 {
		t.Fatalf("observations = %+v, want 2 domains", res.Observations)
	}
	if res.Observations.Domains[0].Host != "api.stripe.com" || res.Observations.Domains[1].Host != "registry.npmjs.org" {
		t.Errorf("domains = %+v, want sorted [api.stripe.com registry.npmjs.org]", res.Observations.Domains)
	}
	if len(res.SecretNamesMinted) != 1 || res.SecretNamesMinted[0] != "STRIPE_SECRET_KEY" {
		t.Errorf("secret names = %v, want [STRIPE_SECRET_KEY] (api_key grant scope resolution)", res.SecretNamesMinted)
	}
	if !res.KernelSensorBlind {
		t.Error("CC3 run must surface kernel_sensor_blind")
	}
}

// TestReconcileRecordRun_StampsEbpfGroundtruthCaveat is
// W20-W20-groundtruth-mapper-4: before this, a capture's Caveats never said
// anything about the host eBPF sensor's own coverage — that state lived only
// on the admin-only /healthz endpoint, nowhere an operator reviewing a
// recording would see it. reconcileRecordRun must stamp the SAME state
// ebpfGroundtruthCaveat computes (which /healthz's ebpfGroundtruthStatus also
// reads) onto RecordTaskResult.Caveats for every non-healthy sensor state.
func TestReconcileRecordRun_StampsEbpfGroundtruthCaveat(t *testing.T) {
	h := newHarness(t)
	runID, wsID := uuid.New(), uuid.New()
	hb := groundtruth.HeartbeatEventWithDropped(0, 9, 0, map[string]uint64{groundtruth.ActionProcessExec: 9}) // partial: 2 kinds never arrived
	hb.Time = time.Now()
	fake := &recordStore{
		run: types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace record",
			State: types.RunCompleted, ConfinementClass: types.CC1},
		importStateFake: importStateFake{ws: recordingWorkspace(wsID, runID, "build")},
		events:          []types.AuditEvent{egressAllowEvent(runID, "registry.npmjs.org")},
		heartbeat:       &hb,
	}
	srv := New(baseTestConfig(h, fake))

	srv.reconcileRecordRun(context.Background(), runID)

	res := fake.savedResult(t, "build")
	found := false
	for _, c := range res.Caveats {
		if strings.Contains(c, "kernel ground truth") && strings.Contains(c, "partial") {
			found = true
		}
	}
	if !found {
		t.Fatalf("caveats = %v, want one naming the partial eBPF sensor coverage", res.Caveats)
	}
}

func TestReconcileRecordRun_IgnoresNonRecordAndSupersededRuns(t *testing.T) {
	h := newHarness(t)
	runID, wsID := uuid.New(), uuid.New()
	// A verify run must never be captured into record_results.
	fake := &recordStore{
		run:             types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace verify"},
		importStateFake: importStateFake{ws: recordingWorkspace(wsID, runID, "build")},
	}
	srv := New(baseTestConfig(h, fake))
	srv.reconcileRecordRun(context.Background(), runID)
	if fake.saved != nil {
		t.Error("verify run wrote record_results")
	}
	// A record run whose task entry now points at a NEWER run is superseded.
	fake2 := &recordStore{
		run:             types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace record"},
		importStateFake: importStateFake{ws: recordingWorkspace(wsID, uuid.New() /* newer run owns the entry */, "build")},
	}
	srv2 := New(baseTestConfig(h, fake2))
	srv2.reconcileRecordRun(context.Background(), runID)
	if fake2.saved != nil {
		t.Error("superseded run wrote record_results")
	}
}

func TestRecordWorkspace_Guards(t *testing.T) {
	wsID := uuid.New()
	ws := types.Workspace{
		ID:      wsID,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/w", Target: "/home/agent/work"}},
		Status:  types.WorkspaceScanned,
	}
	fake := &recordStore{importStateFake: importStateFake{ws: ws}}
	srv := newTestSrv(t, fake)
	url := "/api/v1/workspaces/" + wsID.String() + "/record"

	// Empty/blank session name → 400 (a session must be named).
	if w := do(t, srv, http.MethodPost, url, adminToken, `{"name":"  "}`); w.Code != http.StatusBadRequest {
		t.Errorf("blank name: code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	// A name with no usable [a-z0-9] characters slugs to "" → 400.
	if w := do(t, srv, http.MethodPost, url, adminToken, `{"name":"***"}`); w.Code != http.StatusBadRequest {
		t.Errorf("unsluggable name: code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	// Valid name but no runner configured → 503.
	if w := do(t, srv, http.MethodPost, url, adminToken, `{"name":"build & test"}`); w.Code != http.StatusServiceUnavailable {
		t.Errorf("no-runner: code = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	// A confined VERIFY session request parses (503 no-runner, NOT 400) — the
	// request struct must carry `confined` or DisallowUnknownFields would reject it,
	// silently breaking the confined-verify path.
	if w := do(t, srv, http.MethodPost, url, adminToken, `{"name":"verify build & test","confined":true}`); w.Code != http.StatusServiceUnavailable {
		t.Errorf("confined request: code = %d, want 503 (parsed, no runner); body=%s", w.Code, w.Body.String())
	}
}

// TestRecordWorkspace_ClaimedButNotYetCreatedRunIsBusy is W20-W20-capture-
// store-5: ClaimWorkspaceActiveRun CAS's ws.ActiveRunID onto the workspace
// BEFORE Store.CreateRun persists the claiming run's own row (the clone-grant
// FK needs the run row first). A second record request landing in that
// window used to see GetRun(ActiveRunID) => ErrNotFound and read the old
// `gerr == nil && !isTerminalRunState(...)` busy-check as "not busy" — an
// indeterminate GetRun error must be treated as busy (only a CONFIRMED
// terminal run may proceed), or the serial import-step gate is jumped into
// two concurrent open-egress sandboxes.
func TestRecordWorkspace_ClaimedButNotYetCreatedRunIsBusy(t *testing.T) {
	wsID, claimingRunID := uuid.New(), uuid.New()
	ws := types.Workspace{
		ID:          wsID,
		Sources:     []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/w", Target: "/home/agent/work"}},
		Status:      types.WorkspaceScanned,
		ActiveRunID: &claimingRunID, // claimed, but its run row doesn't exist yet
	}
	// fake.run stays the zero value: GetRun(claimingRunID) => ErrNotFound,
	// exactly the "claimed but not yet CreateRun'd" window.
	fake := &recordStore{importStateFake: importStateFake{ws: ws}}
	srv := newTestSrv(t, fake)
	srv.cfg.Runner = &fakeRunner{} // reach the busy-check, not the no-runner 503
	url := "/api/v1/workspaces/" + wsID.String() + "/record"

	w := do(t, srv, http.MethodPost, url, adminToken, `{"name":"second session"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("second request against a claimed-but-not-yet-created run: code = %d, want 409 (busy) — "+
			"an indeterminate GetRun must not read as 'not busy'; body=%s", w.Code, w.Body.String())
	}
}

// recordSessionKey slugs an operator-chosen session name into a stable map key.
func TestRecordSessionKey_Slugs(t *testing.T) {
	cases := map[string]string{
		"build & test":      "build-test",
		"  Agent Dev Loop ": "agent-dev-loop",
		"deploy/dry-run":    "deploy-dry-run",
		"***":               "",
		"":                  "",
	}
	for in, want := range cases {
		if got := recordSessionKey(in); got != want {
			t.Errorf("recordSessionKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGetWorkspace_ReturnsRecordResultsNoDerivedTasks(t *testing.T) {
	wsID := uuid.New()
	fake := &recordStore{importStateFake: importStateFake{ws: types.Workspace{
		ID:      wsID,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/w", Target: "/home/agent/work"}},
		Status:  types.WorkspaceScanned,
		RecordResults: mustJSON(map[string]RecordTaskResult{
			"build-test": {RunID: uuid.New(), Label: "build & test", Mode: recordModeInteractive, Status: recordStatusRecorded},
		})}}}
	srv := newTestSrv(t, fake)
	w := do(t, srv, http.MethodGet, "/api/v1/workspaces/"+wsID.String(), adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("get: code = %d", w.Code)
	}
	var got struct {
		RecordTasks   []any                       `json:"record_tasks"`
		RecordResults map[string]RecordTaskResult `json:"record_results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// No derived taxonomy anymore — sessions come from record_results.
	if len(got.RecordTasks) != 0 {
		t.Errorf("record_tasks should be gone, got %+v", got.RecordTasks)
	}
	if s, ok := got.RecordResults["build-test"]; !ok || s.Label != "build & test" {
		t.Fatalf("record_results[build-test].Label = %+v, want the named session", got.RecordResults["build-test"])
	}
}

func TestPromoteRecordEgress_MergeRules(t *testing.T) {
	runID, wsID := uuid.New(), uuid.New()
	obs := recordmode.Observations{Domains: []recordmode.DomainObservation{
		{Host: "api.stripe.com", AllowCount: 3},        // promote
		{Host: "already.example.com", AllowCount: 1},   // dup vs approved → no-op
		{Host: "evil.example.com", DenyCount: 2},       // denied-only → never
		{Host: "pending.example.com", PendingCount: 1}, // pending-only → never
		// The metadata IP is denied by the UNCONDITIONAL guard even under
		// allow-all, so it can only ever appear deny-only → excluded here.
		{Host: "169.254.169.254", DenyCount: 1},
		{Host: "localhost", AllowCount: 1}, // not a ValidApprovedHost shape → skipped
	}}
	fake := &recordStore{importStateFake: importStateFake{ws: types.Workspace{
		ID:      wsID,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/w", Target: "/home/agent/work"}},
		Status:  types.WorkspaceScanned, ApprovedEgress: []string{"already.example.com"},
		RecordResults: mustJSON(map[string]RecordTaskResult{
			"build": {RunID: runID, Mode: "auto", Status: recordStatusRecorded, Observations: &obs},
		})}}}
	srv := newTestSrv(t, fake)
	w := do(t, srv, http.MethodPost, "/api/v1/workspaces/"+wsID.String()+"/record/build/promote-egress", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	// Promotion writes the requirements contract now: exactly ONE new row
	// (allow-only, valid-host-only, deduped against the legacy approved lane),
	// and the legacy ApprovedEgress column is READ-ONLY — never written again.
	if len(fake.ws.Requirements) != 1 {
		t.Fatalf("requirements = %v, want exactly the one promoted row", fake.ws.Requirements)
	}
	if row := fake.ws.Requirements["egress:api.stripe.com"]; row.Level != "required" || row.Provenance != "operator_set" {
		t.Errorf("egress:api.stripe.com = %+v, want required/operator_set", row)
	}
	if len(fake.ws.ApprovedEgress) != 1 || fake.ws.ApprovedEgress[0] != "already.example.com" {
		t.Errorf("approved = %v, want the legacy lane untouched", fake.ws.ApprovedEgress)
	}
	if res := fake.savedResult(t, "build"); !res.EgressPromoted {
		t.Error("egress_promoted marker not set")
	}

	// A failed recording can never be promoted.
	fake.ws.RecordResults = mustJSON(map[string]RecordTaskResult{
		"build": {RunID: runID, Status: recordStatusFailed, Observations: &obs},
	})
	fake.saved = nil
	if w := do(t, srv, http.MethodPost, "/api/v1/workspaces/"+wsID.String()+"/record/build/promote-egress", adminToken, ""); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("failed-recording promote: code = %d, want 422; body=%s", w.Code, w.Body.String())
	}
}

// TestPromoteRecordEgress_MergeFailureDoesNotLeaveMarkerSet is the
// bug-record-1 regression: on base 17455349, the EgressPromoted marker CAS
// is written and committed BEFORE MergeWorkspaceRequirements — so a store
// error from the merge (a concurrent-cap race, or any other failure) left
// the record claiming a widening that never landed. The fix must return the
// merge error to the caller (never a 200) AND leave the persisted marker at
// its pre-click value, so a retry does not skip re-attempting the merge on
// the mistaken belief it already happened.
func TestPromoteRecordEgress_MergeFailureDoesNotLeaveMarkerSet(t *testing.T) {
	runID, wsID := uuid.New(), uuid.New()
	obs := recordmode.Observations{Domains: []recordmode.DomainObservation{
		{Host: "api.stripe.com", AllowCount: 3},
	}}
	fake := &recordStore{importStateFake: importStateFake{ws: types.Workspace{
		ID:      wsID,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/w", Target: "/home/agent/work"}},
		Status:  types.WorkspaceScanned,
		RecordResults: mustJSON(map[string]RecordTaskResult{
			"build": {RunID: runID, Mode: "auto", Status: recordStatusRecorded, Observations: &obs},
		})}},
		mergeErr: errors.New("store: transient write failure"),
	}
	srv := newTestSrv(t, fake)
	w := do(t, srv, http.MethodPost, "/api/v1/workspaces/"+wsID.String()+"/record/build/promote-egress", adminToken, "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500 when the requirements merge fails; body=%s", w.Code, w.Body.String())
	}
	if len(fake.ws.Requirements) != 0 {
		t.Errorf("requirements = %v, want untouched — the merge never applied", fake.ws.Requirements)
	}
	if res := fake.savedResult(t, "build"); res.EgressPromoted {
		t.Error("egress_promoted marker must NOT be left set when the widening it claims never landed")
	}
}

// staleReadStore serves a fixed stale workspace row from GetWorkspace while
// delegating writes (and their guards) to the embedded recordStore. Used only
// by TestPromoteRecordEgress_GuardMissConflicts below, to model the row the
// operator's GET saw going stale before their promote-egress guarded write
// lands.
type staleReadStore struct {
	*recordStore
	staleWS types.Workspace
}

func (s *staleReadStore) GetWorkspace(context.Context, uuid.UUID) (types.Workspace, error) {
	return s.staleWS, nil
}

func TestPromoteRecordEgress_GuardMissConflicts(t *testing.T) {
	runID, wsID := uuid.New(), uuid.New()
	obs := recordmode.Observations{Domains: []recordmode.DomainObservation{{Host: "api.stripe.com", AllowCount: 1}}}
	// The row the handler reads says `recorded`, but the guarded write sees the
	// fake's saved blob where a re-record flipped it back to `recording`.
	fake := &recordStore{importStateFake: importStateFake{ws: types.Workspace{
		ID:      wsID,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/w", Target: "/home/agent/work"}},
		Status:  types.WorkspaceScanned,
		RecordResults: mustJSON(map[string]RecordTaskResult{
			"build": {RunID: runID, Status: recordStatusRecorded, Observations: &obs},
		})}}}
	// Prime saved with the superseding re-record, while GetWorkspace keeps
	// serving the stale `recorded` row the operator saw.
	fake.saved = mustJSON(map[string]RecordTaskResult{
		"build": {RunID: uuid.New(), Status: recordStatusRecording},
	})
	srv := newTestSrv(t, &staleReadStore{recordStore: fake, staleWS: fake.ws})
	w := do(t, srv, http.MethodPost, "/api/v1/workspaces/"+wsID.String()+"/record/build/promote-egress", adminToken, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409 when the recording changed concurrently; body=%s", w.Code, w.Body.String())
	}
	// M4: the record-entry CAS runs BEFORE the contract widening, so a CAS
	// miss must leave the requirements overlay untouched — not widen-then-409.
	if len(fake.ws.Requirements) != 0 {
		t.Errorf("requirements = %v, want untouched (empty) on a CAS miss", fake.ws.Requirements)
	}
}

// TestPromoteRecordEgress_SkipsModelProviderAndBaselineHosts is the M2
// self-check: the model-provider host modelProviderEgress unions into EVERY
// session (harness plumbing, not a task need) and the baseline clone hosts
// every scan/verify gets for free must never become a PERMANENT per-workspace
// ApprovedEgress entry, even though both show up as genuinely ALLOWED in the
// capture (the sandbox really did reach them).
func TestPromoteRecordEgress_SkipsModelProviderAndBaselineHosts(t *testing.T) {
	runID, wsID := uuid.New(), uuid.New()
	obs := recordmode.Observations{Domains: []recordmode.DomainObservation{
		{Host: "api.stripe.com", AllowCount: 1},    // genuine app need → promote
		{Host: "api.anthropic.com", AllowCount: 5}, // harness/model-provider plumbing → never
		{Host: "github.com", AllowCount: 2},        // baseline clone host → never
		// Brokered forge's SSH endpoint: confineGitBrokerEgress denies it on every
		// brokered run, so promoting it grants an entry that is dead where it
		// matters. Honesty, not confinement — the deny outranks ApprovedEgress
		// either way (it is a per-run phase after every union).
		{Host: "ssh.github.com", AllowCount: 3},
	}}
	fake := &recordStore{importStateFake: importStateFake{ws: types.Workspace{
		ID:      wsID,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/w", Target: "/home/agent/work"}},
		Status:  types.WorkspaceScanned,
		RecordResults: mustJSON(map[string]RecordTaskResult{
			"build": {RunID: runID, Mode: "auto", Status: recordStatusRecorded, Observations: &obs},
		})}}}
	h := newHarness(t)
	cfg := baseTestConfig(h, fake)
	cfg.DefaultPolicy = types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}}
	srv := New(cfg)

	w := do(t, srv, http.MethodPost, "/api/v1/workspaces/"+wsID.String()+"/record/build/promote-egress", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(fake.ws.Requirements) != 1 {
		t.Fatalf("requirements = %v, want only the stripe row (model-provider + baseline clone hosts must never be promoted)",
			fake.ws.Requirements)
	}
	if row := fake.ws.Requirements["egress:api.stripe.com"]; row.Level != "required" || row.Provenance != "operator_set" {
		t.Errorf("egress:api.stripe.com = %+v, want required/operator_set", row)
	}
}

// TestPromoteRecordEgress_ZeroPromotedDoesNotSetEgressPromoted is W20-S1-1:
// EgressPromoted used to be an unconditional `true` on every promote-egress
// call, even one whose entire observed-allowed set is plumbing (never a
// promotable candidate — see promotableHosts) or already covered, so
// `promoted` stays empty. The card renders "Promoted" purely off this flag
// (record-pane.tsx), so that used to claim success for a click that changed
// nothing.
func TestPromoteRecordEgress_ZeroPromotedDoesNotSetEgressPromoted(t *testing.T) {
	runID, wsID := uuid.New(), uuid.New()
	// Every observed+allowed host is either model-provider plumbing or
	// already approved — promotableHosts excludes the former, the dedup
	// excludes the latter, so `promoted` is empty either way.
	obs := recordmode.Observations{Domains: []recordmode.DomainObservation{
		{Host: "api.anthropic.com", AllowCount: 5},
		{Host: "already.example.com", AllowCount: 1},
	}}
	fake := &recordStore{importStateFake: importStateFake{ws: types.Workspace{
		ID:      wsID,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/w", Target: "/home/agent/work"}},
		Status:  types.WorkspaceScanned, ApprovedEgress: []string{"already.example.com"},
		RecordResults: mustJSON(map[string]RecordTaskResult{
			"build": {RunID: runID, Mode: "auto", Status: recordStatusRecorded, Observations: &obs},
		})}}}
	h := newHarness(t)
	cfg := baseTestConfig(h, fake)
	cfg.DefaultPolicy = types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}}
	srv := New(cfg)

	w := do(t, srv, http.MethodPost, "/api/v1/workspaces/"+wsID.String()+"/record/build/promote-egress", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(fake.ws.Requirements) != 0 {
		t.Fatalf("requirements = %v, want none (nothing was actually promotable)", fake.ws.Requirements)
	}
	if res := fake.savedResult(t, "build"); res.EgressPromoted {
		t.Error("egress_promoted marker set on a click that promoted zero rows — the exact false-success this finding closes")
	}
}

// TestPromoteRecordEgress_EgressPromotedStaysStickyAcrossANoOpClick pins the
// OTHER direction of the same fix: EgressPromoted must not FLIP BACK to
// false on a later no-op click against the same (immutable-once-recorded)
// observations, once a genuine promotion already landed — it means "this
// session HAS ever promoted something real", not "this specific click did".
func TestPromoteRecordEgress_EgressPromotedStaysStickyAcrossANoOpClick(t *testing.T) {
	runID, wsID := uuid.New(), uuid.New()
	obs := recordmode.Observations{Domains: []recordmode.DomainObservation{{Host: "api.stripe.com", AllowCount: 1}}}
	fake := &recordStore{importStateFake: importStateFake{ws: types.Workspace{
		ID:      wsID,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/w", Target: "/home/agent/work"}},
		Status:  types.WorkspaceScanned,
		RecordResults: mustJSON(map[string]RecordTaskResult{
			"build": {RunID: runID, Mode: "auto", Status: recordStatusRecorded, Observations: &obs},
		})}}}
	srv := newTestSrv(t, fake)
	url := "/api/v1/workspaces/" + wsID.String() + "/record/build/promote-egress"

	// First click: a genuine promotion.
	if w := do(t, srv, http.MethodPost, url, adminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("first click: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if res := fake.savedResult(t, "build"); !res.EgressPromoted {
		t.Fatal("first click should have set egress_promoted")
	}

	// Second click against the SAME (settled) observations: api.stripe.com is
	// now already in the requirements overlay, so this click's own `promoted`
	// is empty — but the marker must stay true.
	if w := do(t, srv, http.MethodPost, url, adminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("second click: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if res := fake.savedResult(t, "build"); !res.EgressPromoted {
		t.Error("egress_promoted must stay true — a later no-op click must not un-promote an earlier real one")
	}
}

// TestPromoteRecordEgress_HostSubset is the M3 self-check: an optional
// {"hosts": [...]} narrows promotion to a validated subset instead of the
// recording's entire observed-allowed set going in wholesale.
func TestPromoteRecordEgress_HostSubset(t *testing.T) {
	runID, wsID := uuid.New(), uuid.New()
	obs := recordmode.Observations{Domains: []recordmode.DomainObservation{
		{Host: "api.stripe.com", AllowCount: 1},
		{Host: "evil.example.com", AllowCount: 1},
	}}
	fake := &recordStore{importStateFake: importStateFake{ws: types.Workspace{
		ID:      wsID,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/w", Target: "/home/agent/work"}},
		Status:  types.WorkspaceScanned,
		RecordResults: mustJSON(map[string]RecordTaskResult{
			"build": {RunID: runID, Mode: "auto", Status: recordStatusRecorded, Observations: &obs},
		})}}}
	srv := newTestSrv(t, fake)
	url := "/api/v1/workspaces/" + wsID.String() + "/record/build/promote-egress"

	// A host that wasn't observed+allowed in this recording → reject, nothing promoted.
	if w := do(t, srv, http.MethodPost, url, adminToken, `{"hosts":["not-observed.example.com"]}`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unrecognized host: code = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	if len(fake.ws.Requirements) != 0 {
		t.Fatalf("a rejected subset must not widen anything, got %v", fake.ws.Requirements)
	}

	// A valid subset promotes ONLY that subset, leaving the rest un-approved.
	if w := do(t, srv, http.MethodPost, url, adminToken, `{"hosts":["api.stripe.com"]}`); w.Code != http.StatusOK {
		t.Fatalf("valid subset: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(fake.ws.Requirements) != 1 || fake.ws.Requirements["egress:api.stripe.com"].Level != "required" {
		t.Errorf("requirements = %v, want only egress:api.stripe.com (evil.example.com excluded from the requested subset)",
			fake.ws.Requirements)
	}
}

func TestGetWorkspace_RepairsStaleRecordingOnRead(t *testing.T) {
	h := newHarness(t)
	runID, wsID := uuid.New(), uuid.New()
	// Entry says `recording` but the run is TERMINAL (e.g. idle-reaped with no
	// reconcile hook): the GET must settle it via repair-on-read.
	fake := &recordStore{
		run:             types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace record", State: types.RunStopped},
		importStateFake: importStateFake{ws: recordingWorkspace(wsID, runID, "build")},
	}
	srv := New(baseTestConfig(h, fake))
	w := do(t, srv, http.MethodGet, "/api/v1/workspaces/"+wsID.String(), adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("get: code = %d", w.Code)
	}
	if res := fake.savedResult(t, "build"); res.Status != recordStatusFailed {
		t.Fatalf("stale recording not repaired on read: status=%q", res.Status)
	}
}
