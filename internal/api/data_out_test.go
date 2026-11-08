// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func dataOutPtr(r types.TransferRule) *types.TransferRule { return &r }

// withDataOut sets the data_out rule the run's recorded envelope carries.
func (e *desktopFilesEnv) withDataOut(r types.TransferRule) *desktopFilesEnv {
	e.srv.cfg.Store.(*envelopeStore).spec.DataOut = r
	return e
}

func (e *desktopFilesEnv) withoutDesktop() *desktopFilesEnv {
	e.srv.cfg.Store.(*envelopeStore).spec.UIApps = nil
	return e
}

// TestDataOutBoundarySealsRunAtRevive: the seal makes a run's data_out the stricter of its own and each outer
// layer's, records the layer that decided it, and an unread boundary or restriction seals it to blocked with
// every push refused.
func TestDataOutBoundarySealsRunAtRevive(t *testing.T) {
	boundary := sealLayer{rec: boundaryProvenance, dataOut: dataOutPtr(types.TransferBlocked), denyAllPushes: true}
	spec := types.RunPolicySpec{DataOut: types.TransferReview}
	decided := sealPolicy(&spec, []sealLayer{boundary})
	if spec.DataOut != types.TransferBlocked || len(decided) != 1 || decided[0].knob != "data_out" || decided[0].layer != boundaryProvenance {
		t.Fatalf("sealed to %q, decisions %+v; want blocked, decided by the boundary", spec.DataOut, decided)
	}
	spec = types.RunPolicySpec{DataOut: types.TransferBlocked}
	if decided := sealPolicy(&spec, []sealLayer{{rec: boundaryProvenance, dataOut: dataOutPtr(types.TransferReview)}}); spec.DataOut != types.TransferBlocked || len(decided) != 0 {
		t.Errorf("a looser layer changed a blocked run to %q, decisions %+v", spec.DataOut, decided)
	}
	spec = types.RunPolicySpec{}
	sealPolicy(&spec, []sealLayer{{rec: boundaryProvenance, dataOut: dataOutPtr(types.TransferAllowed)}})
	if spec.DataOut != "" {
		t.Errorf("an allowed layer wrote %q over an unset rule", spec.DataOut)
	}

	r := types.Restriction{Spec: types.RestrictionSpec{DataOut: dataOutPtr(types.TransferBlocked)}}
	if l := restrictionSealLayer(r); l.dataOut == nil || *l.dataOut != types.TransferBlocked || !l.denyAllPushes {
		t.Errorf("a blocked restriction's layer = %+v, want blocked and denyAllPushes", l)
	}
	r.Spec.DataOut = dataOutPtr(types.TransferReview)
	if l := restrictionSealLayer(r); l.dataOut == nil || *l.dataOut != types.TransferReview || l.denyAllPushes {
		t.Errorf("a review restriction's layer = %+v, want review and no push refusal", l)
	}
}

// TestDataOutFilesPanelReviewOnRunWithoutDesktop: under review a run with no desktop lists ~/Transfers, holds
// each download for a review, and refuses an upload.
func TestDataOutFilesPanelReviewOnRunWithoutDesktop(t *testing.T) {
	e := newDesktopFilesEnv(t, nil).withoutDesktop().withDataOut(types.TransferReview)
	w := doSSO(t, e.srv, http.MethodGet, e.path(""), e.ses["owner"], "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `{"name":"agent-report.csv","bytes":17}`) {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	w = doSSO(t, e.srv, http.MethodGet, e.path("agent-report.csv"), e.ses["owner"], "")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), reasonDesktopReviewRequired) || strings.Contains(w.Body.String(), "agent,wrote") {
		t.Fatalf("download without an approval: %d %s", w.Code, w.Body.String())
	}
	w = doSSO(t, e.srv, http.MethodPut, e.path("in.txt"), e.ses["owner"], "x")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), reasonDesktopTransferBlocked) {
		t.Fatalf("upload: %d %s, want the blocked refusal", w.Code, w.Body.String())
	}
}

// TestDataOutFilesPanelAllowedRunWithoutDesktopStill409: with the rule absent or allowed, a run with no
// desktop is refused as before.
func TestDataOutFilesPanelAllowedRunWithoutDesktopStill409(t *testing.T) {
	for _, rule := range []types.TransferRule{"", types.TransferAllowed, types.TransferBlocked} {
		e := newDesktopFilesEnv(t, nil).withoutDesktop().withDataOut(rule)
		w := doSSO(t, e.srv, http.MethodGet, e.path(""), e.ses["owner"], "")
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), reasonDesktopTransferUnavailable) {
			t.Errorf("data_out %q: %d %s, want 409 %s", rule, w.Code, w.Body.String(), reasonDesktopTransferUnavailable)
		}
	}
}

// TestDataOutFilesPanelBlockedRefusesEveryDownload: blocked refuses the download even where the run's own
// desktop rules allow it.
func TestDataOutFilesPanelBlockedRefusesEveryDownload(t *testing.T) {
	e := newDesktopFilesEnv(t, &types.DesktopControls{Download: types.TransferAllowed}).withDataOut(types.TransferBlocked)
	w := doSSO(t, e.srv, http.MethodGet, e.path("agent-report.csv"), e.ses["owner"], "")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), reasonDesktopTransferBlocked) || strings.Contains(w.Body.String(), "agent,wrote") {
		t.Fatalf("download under blocked: %d %s", w.Code, w.Body.String())
	}
	if rows := e.transferRows("download"); len(rows) != 1 || rows[0].Outcome != "denied" {
		t.Fatalf("the refused download is audited once as desktop.file.transfer: %+v", rows)
	}
}

// TestDataOutClipboardCopyOutBlockedUnderReview: the rules the desktop relay builds its filter from have copy
// out off under review and blocked, and are untouched with the rule absent.
func TestDataOutClipboardCopyOutBlockedUnderReview(t *testing.T) {
	for _, tc := range []struct {
		rule    types.TransferRule
		copyOut types.TransferRule
	}{{"", types.TransferAllowed}, {types.TransferAllowed, types.TransferAllowed}, {types.TransferReview, types.TransferBlocked}, {types.TransferBlocked, types.TransferBlocked}} {
		e := newDesktopFilesEnv(t, &types.DesktopControls{CopyOut: types.TransferAllowed}).withDataOut(tc.rule)
		_, rules, rule, err := e.srv.runDesktopPolicy(context.Background(), e.run.ID)
		if err != nil || rules.CopyOut != tc.copyOut || rules.PasteIn != types.TransferAllowed || rule != tc.rule.DataOut() {
			t.Errorf("data_out %q: rules %+v, rule %q, err %v; want copy out %q, paste in allowed", tc.rule, rules, rule, err, tc.copyOut)
		}
	}
}

// TestDataOutFilesPanelReviewUnavailableOnAMachineWithoutExecStreams: a runner with no exec stream answers
// 409 data_out_review_unavailable at the list.
func TestDataOutFilesPanelReviewUnavailableOnAMachineWithoutExecStreams(t *testing.T) {
	e := newDesktopFilesEnv(t, nil).withoutDesktop().withDataOut(types.TransferReview)
	e.srv.cfg.Runner = &sshFakeRunner{}
	w := doSSO(t, e.srv, http.MethodGet, e.path(""), e.ses["owner"], "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), reasonDataOutReviewUnavailable) {
		t.Fatalf("list on a machine with no exec stream: %d %s", w.Code, w.Body.String())
	}
}

func dataOutRunOutput(t *testing.T, rule types.TransferRule, readErr error) (*Server, uuid.UUID) {
	t.Helper()
	srv, _, seed := runOutputServer(t)
	srv.cfg.Store = &envelopeStore{Store: srv.cfg.Store, spec: types.RunPolicySpec{DataOut: rule}, readErr: readErr}
	return srv, seed(types.AgentRun{State: types.RunCompleted})
}

// TestDataOutRunOutputSecurityOnly: under review or blocked the owner is refused and audited, and a security
// admin is let through to the output's own answers.
func TestDataOutRunOutputSecurityOnly(t *testing.T) {
	for _, rule := range []types.TransferRule{types.TransferReview, types.TransferBlocked} {
		srv, id := dataOutRunOutput(t, rule, nil)
		code, _, refusal := getRunOutput(t, srv, id, "", outputOwnerCookie(t))
		if code != http.StatusForbidden || refusal.Reason != reasonDataOutBlocked {
			t.Errorf("%s: owner = %d %q, want 403 %s", rule, code, refusal.Reason, reasonDataOutBlocked)
		}
		code, _, refusal = getRunOutput(t, srv, id, "", ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin))
		if code == http.StatusForbidden && refusal.Reason == reasonDataOutBlocked {
			t.Errorf("%s: a security admin was refused by the rule", rule)
		}
	}
}

// TestDataOutRecordingOwnerRefused: the owner's replay is a 404 with the audit row; a super admin's replay is
// served and audited as recording.read.
func TestDataOutRecordingOwnerRefused(t *testing.T) {
	srv, ast, _, rs := newAuthzMatrixServer(t)
	srv.cfg.Store = &envelopeStore{Store: srv.cfg.Store, spec: types.RunPolicySpec{DataOut: types.TransferReview}}
	rec := srv.cfg.Audit.(*recRecorder)
	id := uuid.New()
	ast.mu.Lock()
	ast.runs[id] = types.AgentRun{ID: id, CreatedBy: "sub-member", State: types.RunStopped, Agent: "claude-code"}
	ast.mu.Unlock()
	if err := rs.SaveCast(context.Background(), id.String(), strings.NewReader(`{"version":2}`+"\n")); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/runs/" + id.String() + "/recording/" + id.String()
	if w := doSSO(t, srv, http.MethodGet, path, outputOwnerCookie(t), ""); w.Code != http.StatusNotFound {
		t.Fatalf("owner replay under review = %d %s, want 404", w.Code, w.Body.String())
	}
	if ev := findAudit(rec.snapshot(), id, "run.data_out.denied", "denied"); ev == nil || !strings.Contains(string(ev.Data), `"path":"recording"`) {
		t.Errorf("no run.data_out.denied row for the recording: %+v", ev)
	}
	if w := doSSO(t, srv, http.MethodGet, path, ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin), ""); w.Code != http.StatusOK {
		t.Fatalf("super admin replay under review = %d %s, want 200", w.Code, w.Body.String())
	}
	if ev := findAudit(rec.snapshot(), id, auditRecordingRead, "success"); ev == nil {
		t.Error("the operator's replay was not audited as recording.read")
	}
}

// TestDataOutUnreadableFailsClosed: a rule that cannot be read refuses the output with 503.
func TestDataOutUnreadableFailsClosed(t *testing.T) {
	srv, id := dataOutRunOutput(t, "", errors.New("audit store unavailable"))
	code, _, refusal := getRunOutput(t, srv, id, "", outputOwnerCookie(t))
	if code != http.StatusServiceUnavailable || refusal.Reason != reasonDataOutUnreadable {
		t.Errorf("output with an unreadable rule = %d %q, want 503 %s", code, refusal.Reason, reasonDataOutUnreadable)
	}
}

// TestDataOutAbsentChangesNothing: with the rule absent, the owner's output and replay answer as before.
func TestDataOutAbsentChangesNothing(t *testing.T) {
	srv, id := dataOutRunOutput(t, "", nil)
	if code, _, refusal := getRunOutput(t, srv, id, "", outputOwnerCookie(t)); code == http.StatusForbidden || code == http.StatusServiceUnavailable {
		t.Errorf("output with no data_out rule = %d %q, want the output's own answer", code, refusal.Reason)
	}
	for _, rule := range []types.TransferRule{"", types.TransferAllowed} {
		e := newDesktopFilesEnv(t, &types.DesktopControls{Download: types.TransferAllowed}).withDataOut(rule)
		if w := doSSO(t, e.srv, http.MethodGet, e.path("agent-report.csv"), e.ses["owner"], ""); w.Code != http.StatusOK {
			t.Errorf("download with data_out %q = %d, want 200", rule, w.Code)
		}
	}
}

// TestDataOutWebAppRefusedDesktopOpen: under review a web app other than the desktop is refused with an audit
// row, and the desktop is not.
func TestDataOutWebAppRefusedDesktopOpen(t *testing.T) {
	h := newUIHarness(t, okBackend())
	h.store.putEffectivePolicy(h.run.ID, types.RunPolicySpec{DataOut: types.TransferReview})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+h.run.ID.String()+"/ui/code/session")
	if h.srv.refuseAppDataOut(w, r, h.run, "code") != true || w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), reasonDataOutBlocked) {
		t.Fatalf("a web app under review: refused %d %s", w.Code, w.Body.String())
	}
	if rows := h.audit.auditRows("run.data_out.denied", "denied"); len(rows) != 1 || !strings.Contains(string(rows[0].Data), `"path":"app"`) {
		t.Errorf("run.data_out.denied rows = %+v, want one for the app", rows)
	}
	for _, app := range []string{"novnc", desktopSigninApp} {
		w = httptest.NewRecorder()
		if h.srv.refuseAppDataOut(w, r, h.run, app) {
			t.Errorf("the desktop %q was refused under review", app)
		}
	}
}

// TestDataOutComputerFolderBothRefused: a computer folder that syncs back is refused at create (422) under
// review or blocked, and copy in is not.
func TestDataOutComputerFolderBothRefused(t *testing.T) {
	runner := uuid.New()
	for _, tc := range []struct {
		rule      types.TransferRule
		direction string
		refused   bool
	}{
		{types.TransferReview, types.ComputerFolderBoth, true},
		{types.TransferBlocked, types.ComputerFolderBoth, true},
		{types.TransferReview, types.ComputerFolderIn, false},
		{"", types.ComputerFolderBoth, false},
		{types.TransferAllowed, types.ComputerFolderBoth, false},
	} {
		w := httptest.NewRecorder()
		f := &runFold{w: w, r: httptest.NewRequest(http.MethodPost, "/api/v1/runs"), spec: types.RunPolicySpec{DataOut: tc.rule},
			req: &createRunRequest{LocalFolder: &types.RunLocalFolder{RunnerID: &runner, Direction: tc.direction}}}
		got := !(&Server{}).stepComputerFolderDataOut(f, nil)
		if got != tc.refused || (got && (w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), reasonComputerFolderDataOut))) {
			t.Errorf("data_out %q, direction %q: refused %v (%d %s), want %v", tc.rule, tc.direction, got, w.Code, w.Body.String(), tc.refused)
		}
	}
}

// TestDataOutBringBackRefused: the person's bring-back is refused 403 with an audit row under review, and the
// run's end skips the automatic one.
func TestDataOutBringBackRefused(t *testing.T) {
	s, cs, audit, run, _, _ := bringBackFixture(t)
	s.cfg.Store = dataOutFolderStore{refStore: s.cfg.Store.(refStore), spec: types.RunPolicySpec{DataOut: types.TransferReview}}
	res := bringBack(t, s, bringBackOwnerSession(t), run)
	if res.code != http.StatusForbidden || res.reason() != reasonDataOutBlocked {
		t.Fatalf("bring-back under review = %d %s, want 403 %s", res.code, res.body, reasonDataOutBlocked)
	}
	if ev := findAudit(audit.snapshot(), run.ID, "run.data_out.denied", "denied"); ev == nil || !strings.Contains(string(ev.Data), `"path":"computer_folder"`) {
		t.Errorf("no run.data_out.denied row for the folder: %+v", ev)
	}
	if s.bringComputerFolderBack(context.Background(), run.ID, "sbx") {
		t.Error("the run's end brought the folder back under review")
	}
	if cs.status.Back != nil {
		t.Errorf("a pass back ran: %+v", cs.status.Back)
	}
}

// dataOutFolderStore is a computer-folder store whose run records a policy envelope with spec.
type dataOutFolderStore struct {
	refStore
	spec types.RunPolicySpec
}

func (s dataOutFolderStore) QueryAuditEvents(_ context.Context, runID uuid.UUID, _ int) ([]types.AuditEvent, error) {
	data, _ := json.Marshal(s.spec)
	return []types.AuditEvent{{ID: uuid.New(), RunID: &runID, Action: "run.policy.resolve", Outcome: "success", Data: data}}, nil
}
