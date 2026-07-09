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

	"github.com/cjohnstoniv/wardyn/internal/recordmode"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// recordStore fakes exactly the store surface the record lane touches.
type recordStore struct {
	store.Store
	ws            types.Workspace
	run           types.AgentRun
	events        []types.AuditEvent
	grants        []types.CredentialGrant
	saved         json.RawMessage  // current record_results blob (per-key upserts land here)
	state         *types.Workspace // last SetWorkspaceImportState write (nil = untouched)
	claimedRun    *uuid.UUID       // last ClaimWorkspaceActiveRun run id
	clearedActive bool             // ClearWorkspaceActiveRun called
}

func (s *recordStore) GetWorkspace(context.Context, uuid.UUID) (types.Workspace, error) {
	ws := s.ws
	if s.saved != nil {
		ws.RecordResults = s.saved
	}
	return ws, nil
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
func (s *recordStore) SetWorkspaceImportState(_ context.Context, _ uuid.UUID, status types.WorkspaceStatus, active *uuid.UUID, vr json.RawMessage, vh string, va *time.Time) (types.Workspace, error) {
	ws := s.ws
	ws.Status = status
	ws.ActiveRunID = active
	ws.VerifyResult = vr
	ws.VerifiedProfileHash = vh
	ws.VerifiedAt = va
	s.state = &ws
	return ws, nil
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
		ID: wsID, Kind: types.WorkspaceKindLocalDir, Source: "/w", Status: types.WorkspaceScanned,
		ActiveRunID: &runID,
		RecordResults: mustJSON(map[string]RecordTaskResult{
			task: {RunID: runID, Mode: recordModeAuto, Status: recordStatusRecording},
		}),
	}
}

func egressAllowEvent(runID uuid.UUID, host string) types.AuditEvent {
	return types.AuditEvent{RunID: &runID, Action: "egress.allow", Outcome: "success",
		Target: host, Data: mustJSON(map[string]any{"host": host, "method": "GET"})}
}

func TestReconcileRecordRun_EmptyCaptureIsFailureNeverNoEgress(t *testing.T) {
	h := newHarness(t)
	runID, wsID := uuid.New(), uuid.New()
	fake := &recordStore{
		run: types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace record", State: types.RunCompleted},
		ws:  recordingWorkspace(wsID, runID, "build"),
	}
	srv := New(Config{Identity: h.idp, Audit: h.audit, AdminToken: adminToken,
		TrustDomain: "wardyn.local", ControlPlaneURL: "http://wardynd:8080", Store: fake})

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

func TestReconcileRecordRun_CapturesObservationsAndSecretNames(t *testing.T) {
	h := newHarness(t)
	runID, wsID, grantID := uuid.New(), uuid.New(), uuid.New()
	fake := &recordStore{
		run: types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace record",
			State: types.RunCompleted, ConfinementClass: types.CC3},
		ws: recordingWorkspace(wsID, runID, "build"),
		events: []types.AuditEvent{
			egressAllowEvent(runID, "registry.npmjs.org"),
			egressAllowEvent(runID, "api.stripe.com"),
			{RunID: &runID, Action: "credential.mint", Outcome: "success",
				Data: mustJSON(map[string]any{"grant_id": grantID.String()})},
		},
		grants: []types.CredentialGrant{{ID: grantID, RunID: runID,
			Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: mustJSON(map[string]any{"secret_name": "STRIPE_SECRET_KEY"})}}},
	}
	srv := New(Config{Identity: h.idp, Audit: h.audit, AdminToken: adminToken,
		TrustDomain: "wardyn.local", ControlPlaneURL: "http://wardynd:8080", Store: fake})

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

func TestReconcileRecordRun_IgnoresNonRecordAndSupersededRuns(t *testing.T) {
	h := newHarness(t)
	runID, wsID := uuid.New(), uuid.New()
	// A verify run must never be captured into record_results.
	fake := &recordStore{
		run: types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace verify"},
		ws:  recordingWorkspace(wsID, runID, "build"),
	}
	srv := New(Config{Identity: h.idp, Audit: h.audit, AdminToken: adminToken,
		TrustDomain: "wardyn.local", ControlPlaneURL: "http://wardynd:8080", Store: fake})
	srv.reconcileRecordRun(context.Background(), runID)
	if fake.saved != nil {
		t.Error("verify run wrote record_results")
	}
	// A record run whose task entry now points at a NEWER run is superseded.
	fake2 := &recordStore{
		run: types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace record"},
		ws:  recordingWorkspace(wsID, uuid.New() /* newer run owns the entry */, "build"),
	}
	srv2 := New(Config{Identity: h.idp, Audit: h.audit, AdminToken: adminToken,
		TrustDomain: "wardyn.local", ControlPlaneURL: "http://wardynd:8080", Store: fake2})
	srv2.reconcileRecordRun(context.Background(), runID)
	if fake2.saved != nil {
		t.Error("superseded run wrote record_results")
	}
}

func TestRecordWorkspace_Guards(t *testing.T) {
	wsID := uuid.New()
	ws := types.Workspace{ID: wsID, Kind: types.WorkspaceKindLocalDir, Source: "/w", Status: types.WorkspaceScanned,
		SetupCommands: mustJSON([]workspacescan.SetupCommand{{Stage: "build", Command: "go build ./...", Source: "convention:go"}})}
	fake := &recordStore{ws: ws}
	srv := newVerifySrv(t, fake)
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
	fake := &recordStore{ws: types.Workspace{ID: wsID, Kind: types.WorkspaceKindLocalDir, Source: "/w",
		Status: types.WorkspaceScanned,
		RecordResults: mustJSON(map[string]RecordTaskResult{
			"build-test": {RunID: uuid.New(), Label: "build & test", Mode: recordModeInteractive, Status: recordStatusRecorded},
		})}}
	srv := newVerifySrv(t, fake)
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
	fake := &recordStore{ws: types.Workspace{ID: wsID, Kind: types.WorkspaceKindLocalDir, Source: "/w",
		Status: types.WorkspaceScanned, ApprovedEgress: []string{"already.example.com"},
		RecordResults: mustJSON(map[string]RecordTaskResult{
			"build": {RunID: runID, Mode: recordModeAuto, Status: recordStatusRecorded, Observations: &obs},
		})}}
	srv := newVerifySrv(t, fake)
	w := do(t, srv, http.MethodPost, "/api/v1/workspaces/"+wsID.String()+"/record/build/promote-egress", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	wantApproved := []string{"already.example.com", "api.stripe.com"}
	if len(fake.ws.ApprovedEgress) != 2 || fake.ws.ApprovedEgress[0] != wantApproved[0] || fake.ws.ApprovedEgress[1] != wantApproved[1] {
		t.Errorf("approved = %v, want %v (allow-only, valid-host-only, deduped, sorted)", fake.ws.ApprovedEgress, wantApproved)
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

func TestUploadVerifyResult_LateRecordUploadCannotRevertCapture(t *testing.T) {
	h := newHarness(t)
	runID, wsID := uuid.New(), uuid.New()
	tok := h.mintRunToken(t, runID)
	// The workspace's CURRENT entry says `recording` (the handler's read), but
	// the fake's saved blob — what the guarded write checks — says `recorded`:
	// the capture landed between the handler's read and its write.
	fake := &recordStore{
		run: types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace record"},
		ws:  recordingWorkspace(wsID, runID, "build"),
	}
	fake.saved = mustJSON(map[string]RecordTaskResult{
		"build": {RunID: runID, Mode: recordModeAuto, Status: recordStatusRecorded},
	})
	srv := New(Config{Identity: h.idp, Audit: h.audit, AdminToken: adminToken,
		TrustDomain: "wardyn.local", ControlPlaneURL: "http://wardynd:8080", Store: fake})

	// The handler reads the workspace through GetWorkspace (which reflects
	// saved = recorded) → 409 no in-flight recording. Either way, the entry
	// must remain `recorded`.
	body := `{"ran":true,"ok":false,"done":false,"steps":[{"stage":"build","command":"x","exit_code":0,"running":true}]}`
	w := do(t, srv, http.MethodPut, "/api/v1/internal/verify-results/"+runID.String(), tok, body)
	if w.Code != http.StatusConflict && w.Code != http.StatusNoContent {
		t.Fatalf("code = %d, want 409 (stale read) or 204 (guarded no-op)", w.Code)
	}
	if res := fake.savedResult(t, "build"); res.Status != recordStatusRecorded {
		t.Fatalf("completed capture reverted to %q by a late upload", res.Status)
	}
}

func TestPromoteRecordEgress_GuardMissConflicts(t *testing.T) {
	runID, wsID := uuid.New(), uuid.New()
	obs := recordmode.Observations{Domains: []recordmode.DomainObservation{{Host: "api.stripe.com", AllowCount: 1}}}
	// The row the handler reads says `recorded`, but the guarded write sees the
	// fake's saved blob where a re-record flipped it back to `recording`.
	fake := &recordStore{ws: types.Workspace{ID: wsID, Kind: types.WorkspaceKindLocalDir, Source: "/w",
		Status: types.WorkspaceScanned,
		RecordResults: mustJSON(map[string]RecordTaskResult{
			"build": {RunID: runID, Status: recordStatusRecorded, Observations: &obs},
		})}}
	// Prime saved with the superseding re-record, while GetWorkspace keeps
	// serving the stale `recorded` row the operator saw.
	fake.saved = mustJSON(map[string]RecordTaskResult{
		"build": {RunID: uuid.New(), Status: recordStatusRecording},
	})
	srv := newVerifySrv(t, &staleReadStore{recordStore: fake, staleWS: fake.ws})
	w := do(t, srv, http.MethodPost, "/api/v1/workspaces/"+wsID.String()+"/record/build/promote-egress", adminToken, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409 when the recording changed concurrently; body=%s", w.Code, w.Body.String())
	}
	// M4: the record-entry CAS runs BEFORE the egress widening, so a CAS miss
	// must leave ApprovedEgress untouched — not widen-then-409.
	if len(fake.ws.ApprovedEgress) != 0 {
		t.Errorf("approved egress = %v, want untouched (empty) on a CAS miss", fake.ws.ApprovedEgress)
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
	}}
	fake := &recordStore{ws: types.Workspace{ID: wsID, Kind: types.WorkspaceKindLocalDir, Source: "/w",
		Status: types.WorkspaceScanned,
		RecordResults: mustJSON(map[string]RecordTaskResult{
			"build": {RunID: runID, Mode: recordModeAuto, Status: recordStatusRecorded, Observations: &obs},
		})}}
	h := newHarness(t)
	srv := New(Config{Identity: h.idp, Audit: h.audit, AdminToken: adminToken,
		TrustDomain: "wardyn.local", ControlPlaneURL: "http://wardynd:8080", Store: fake,
		DefaultPolicy: types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}}})

	w := do(t, srv, http.MethodPost, "/api/v1/workspaces/"+wsID.String()+"/record/build/promote-egress", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(fake.ws.ApprovedEgress) != 1 || fake.ws.ApprovedEgress[0] != "api.stripe.com" {
		t.Errorf("approved = %v, want only [api.stripe.com] (model-provider + baseline clone hosts must never be promoted)",
			fake.ws.ApprovedEgress)
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
	fake := &recordStore{ws: types.Workspace{ID: wsID, Kind: types.WorkspaceKindLocalDir, Source: "/w",
		Status: types.WorkspaceScanned,
		RecordResults: mustJSON(map[string]RecordTaskResult{
			"build": {RunID: runID, Mode: recordModeAuto, Status: recordStatusRecorded, Observations: &obs},
		})}}
	srv := newVerifySrv(t, fake)
	url := "/api/v1/workspaces/" + wsID.String() + "/record/build/promote-egress"

	// A host that wasn't observed+allowed in this recording → reject, nothing promoted.
	if w := do(t, srv, http.MethodPost, url, adminToken, `{"hosts":["not-observed.example.com"]}`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unrecognized host: code = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	if len(fake.ws.ApprovedEgress) != 0 {
		t.Fatalf("a rejected subset must not widen anything, got %v", fake.ws.ApprovedEgress)
	}

	// A valid subset promotes ONLY that subset, leaving the rest un-approved.
	if w := do(t, srv, http.MethodPost, url, adminToken, `{"hosts":["api.stripe.com"]}`); w.Code != http.StatusOK {
		t.Fatalf("valid subset: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(fake.ws.ApprovedEgress) != 1 || fake.ws.ApprovedEgress[0] != "api.stripe.com" {
		t.Errorf("approved = %v, want only [api.stripe.com] (evil.example.com excluded from the requested subset)",
			fake.ws.ApprovedEgress)
	}
}

// staleReadStore serves a fixed stale workspace row from GetWorkspace while
// delegating writes (and their guards) to the embedded recordStore.
type staleReadStore struct {
	*recordStore
	staleWS types.Workspace
}

func (s *staleReadStore) GetWorkspace(context.Context, uuid.UUID) (types.Workspace, error) {
	return s.staleWS, nil
}

func TestGetWorkspace_RepairsStaleRecordingOnRead(t *testing.T) {
	h := newHarness(t)
	runID, wsID := uuid.New(), uuid.New()
	// Entry says `recording` but the run is TERMINAL (e.g. idle-reaped with no
	// reconcile hook): the GET must settle it via repair-on-read.
	fake := &recordStore{
		run: types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace record", State: types.RunStopped},
		ws:  recordingWorkspace(wsID, runID, "build"),
	}
	srv := New(Config{Identity: h.idp, Audit: h.audit, AdminToken: adminToken,
		TrustDomain: "wardyn.local", ControlPlaneURL: "http://wardynd:8080", Store: fake})
	w := do(t, srv, http.MethodGet, "/api/v1/workspaces/"+wsID.String(), adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("get: code = %d", w.Code)
	}
	if res := fake.savedResult(t, "build"); res.Status != recordStatusFailed {
		t.Fatalf("stale recording not repaired on read: status=%q", res.Status)
	}
}

func TestUploadVerifyResult_RecordRunLandsInRecordResultsOnly(t *testing.T) {
	h := newHarness(t)
	runID, wsID := uuid.New(), uuid.New()
	tok := h.mintRunToken(t, runID)
	fake := &recordStore{
		run: types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace record"},
		ws:  recordingWorkspace(wsID, runID, "build"),
	}
	srv := New(Config{Identity: h.idp, Audit: h.audit, AdminToken: adminToken,
		TrustDomain: "wardyn.local", ControlPlaneURL: "http://wardynd:8080", Store: fake})

	body := `{"ran":true,"ok":true,"done":false,"total":2,"steps":[{"stage":"install","command":"npm ci","exit_code":0,"running":true}]}`
	w := do(t, srv, http.MethodPut, "/api/v1/internal/verify-results/"+runID.String(), tok, body)
	if w.Code != http.StatusNoContent {
		t.Fatalf("code = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	res := fake.savedResult(t, "build")
	if len(res.Steps) != 1 || res.Steps[0].Command != "npm ci" {
		t.Errorf("steps = %+v, want the streamed install step", res.Steps)
	}
	if res.Status != recordStatusRecording {
		t.Errorf("status = %q, want still recording (only termination capture finalizes)", res.Status)
	}
	if fake.state != nil {
		t.Error("record upload touched SetWorkspaceImportState — verify state must be unreachable from the record lane")
	}
}
