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
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The admin bulk restart refuses a Windows VM run in its result, and replaces no proxy.
func TestWindowsVMAdminRestartRefused(t *testing.T) {
	f := newReviveFixture(t)
	f.st.run.MachineKind = types.MachineWindowsVM
	f.run = f.st.run
	res := f.restart(t)
	if res.OK || res.Reason != reasonWindowsVMFeatureRefused || res.Error != windowsDeskOnlyMsg("revive") {
		t.Errorf("restart result = %+v, want refused %s", res, reasonWindowsVMFeatureRefused)
	}
	if len(f.rr.replaced) != 0 {
		t.Errorf("ReplaceProxy calls = %d, want none", len(f.rr.replaced))
	}
}

// Every live egress edit of a Windows VM run is refused before it is tried: one a revive would apply, and one a
// runner that narrows in place would apply. No proxy is replaced or narrowed.
func TestWindowsVMEgressEditRefused(t *testing.T) {
	want := func(t *testing.T, name string, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != http.StatusConflict || errorReasonOf(w.Body.String()) != reasonWindowsVMFeatureRefused ||
			!strings.Contains(w.Body.String(), windowsDeskOnlyMsg("egress edit")) {
			t.Errorf("%s: egress edit = %d %s, want 409 %s", name, w.Code, w.Body, reasonWindowsVMFeatureRefused)
		}
	}
	f := newEgressEditFixture(t)
	f.st.run.MachineKind = types.MachineWindowsVM
	f.run = f.st.run
	want(t, "by revive", f.editEgress(t, `{"deny_hosts":["evil.example"]}`))
	if len(f.rr.replaced) != 0 {
		t.Errorf("by revive: ReplaceProxy calls = %d, want none", len(f.rr.replaced))
	}
	n := newNarrowFixture(t)
	n.st.run.MachineKind = types.MachineWindowsVM
	n.run = n.st.run
	want(t, "in place", n.editEgress(t, `{"deny_hosts":["evil.example"]}`))
	if probes, narrows := n.nr.counts(); probes != 0 || narrows != 0 || len(n.rr.replaced) != 0 {
		t.Errorf("in place: probes %d, narrows %d, replaces %d; want none", probes, narrows, len(n.rr.replaced))
	}
	n.nothingChanged(t)
}

// A token launch is refused, even one that carries the person's own identity: a person's API token, the CLI's
// sign-in token, a token delegated to a portal, and an issuer-portal grant. Only an interactive sign-in starts a Windows VM run.
func TestWindowsVMTokenLaunchRefused(t *testing.T) {
	srv, ws, _ := winvmServer(t, true)
	for name, ctx := range map[string]context.Context{
		"a person's API token":    withAPITokenID(memberContext(), uuid.New()),
		"the CLI's sign-in token": context.WithValue(withAPITokenID(memberContext(), uuid.New()), apiTokenKindCtxKey{}, types.APITokenKindCLIAccess),
		"a delegated token":       audit.WithDelegation(memberContext(), types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New(), Actor: &types.DelegationActor{Sub: "agent", Iss: "https://portal.example"}}),
		"an issuer-portal grant":  audit.WithDelegation(memberContext(), types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New()}),
	} {
		_, rf := srv.windowsImageDecision(winvmHTTP(ctx), winvmReq(), types.RunPolicySpec{})
		wantWinRefusal(t, name, rf, http.StatusForbidden, reasonWindowsVMFeatureRefused, "")
		if rf != nil && rf.body.Error != windowsTokenMsg {
			t.Errorf("%s: %q, want the token sentence", name, rf.body.Error)
		}
	}
	if len(ws.checks) != 0 {
		t.Fatalf("a token launch reached the seat check: %v", ws.checks)
	}
	if _, rf := srv.windowsImageDecision(winvmHTTP(memberContext()), winvmReq(), types.RunPolicySpec{}); rf != nil {
		t.Errorf("an interactive sign-in refused: %s", rf.body.Error)
	}
}

// A request that asks to run as a service is refused as not a person's launch, before the seat check.
func TestWindowsVMServiceRequestRefused(t *testing.T) {
	srv, ws, _ := winvmServer(t, true)
	req := winvmReq(func(q *createRunRequest) { q.Service = &types.RunService{Restart: types.ServiceRestartOnFailure} })
	_, rf := srv.windowsImageDecision(winvmHTTP(memberContext()), req, types.RunPolicySpec{})
	wantWinRefusal(t, "service", rf, http.StatusForbidden, reasonWindowsVMFeatureRefused, "")
	if rf != nil && rf.body.Error != windowsNotAPersonMsg {
		t.Errorf("service: %q, want the not-a-person sentence", rf.body.Error)
	}
	if len(ws.checks) != 0 {
		t.Fatalf("a service request reached the seat check: %v", ws.checks)
	}
}

// With a recording store and no level set, a Windows VM launch is refused 403 with field recording, and so is
// one into a pool that requires recording, whatever the policy says. A policy that says off passes on to the
// runner checks.
func TestWindowsVMRecordingWiring(t *testing.T) {
	srv, _, _ := winvmServer(t, true)
	rs, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv.cfg.RecordingStore = rs
	off := types.RunPolicySpec{Recording: types.RecordingOff}
	required := runPlacement{pooled: true, placement: types.PlacementLocal, row: types.RunnerPool{Recording: types.RecordingRequired}}
	for name, tc := range map[string]struct {
		spec  types.RunPolicySpec
		place runPlacement
		msg   string
	}{
		"no level set":           {spec: types.RunPolicySpec{}, msg: "would be recorded at terminal"},
		"a pool that records":    {spec: off, place: required, msg: windowsRecordingMsg},
		"a terminal level asked": {spec: types.RunPolicySpec{Recording: types.RecordingTerminal}, msg: "would be recorded at terminal"},
	} {
		rf := srv.windowsPlacementRefusal(winvmHTTP(memberContext()), winvmReq(), tc.spec, tc.place, governanceCeiling{}, true)
		wantWinRefusal(t, name, rf, http.StatusForbidden, reasonWindowsVMFeatureRefused, "recording")
		if rf != nil && !strings.Contains(rf.body.Error, tc.msg) {
			t.Errorf("%s: %q, want it to say %q", name, rf.body.Error, tc.msg)
		}
	}
	rf := srv.windowsPlacementRefusal(winvmHTTP(memberContext()), winvmReq(), off, runPlacement{}, governanceCeiling{}, true)
	wantWinRefusal(t, "recording off", rf, http.StatusUnprocessableEntity, reasonWindowsVMRunnerUnsupported, "")
}

// The interactive exec opened under the terminal says a Windows VM run's own sentence; a background run still
// gets errAttachBackgroundOnly.
func TestWindowsVMEstablishExecRefused(t *testing.T) {
	srv := capsServer(&fakeRunner{})
	err := srv.establishExec(context.Background(), types.AgentRun{ID: uuid.New(), MachineKind: types.MachineWindowsVM, Interactive: true}, nil)
	if err == nil || err.Error() != windowsDeskOnlyMsg("interactive exec") {
		t.Errorf("Windows VM run: %v, want %q", err, windowsDeskOnlyMsg("interactive exec"))
	}
	err = srv.establishExec(context.Background(), types.AgentRun{ID: uuid.New(), Experience: types.ExperienceBackground}, nil)
	if !errors.Is(err, errAttachBackgroundOnly) {
		t.Errorf("background run: %v, want %v", err, errAttachBackgroundOnly)
	}
}

// SSH into a live Windows VM run, with its owner's registered key, is refused at auth with the run's own
// sentence, and the failure row names the reason.
func TestWindowsVMSSHAuthRefused(t *testing.T) {
	st := newSSHMemStore()
	runID := uuid.New()
	st.putRun(types.AgentRun{ID: runID, CreatedBy: "alice@example.com", State: types.RunRunning, SandboxRef: "sbx-1",
		MachineKind: types.MachineWindowsVM, Interactive: true})
	_, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: "alice@example.com", PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	h := newSSHTestHarness(t, st, &sshFakeRunner{})
	_, err := h.srv.sshAuth(fakeSSHConn{user: runID.String()}, pub)
	if want := "ssh: " + windowsDeskOnlyMsg("SSH access"); err == nil || err.Error() != want {
		t.Fatalf("auth = %v, want %q", err, want)
	}
	ev := waitForAudit(t, h.audit, runID, "ssh.authenticate", "failure")
	if ev == nil {
		t.Fatalf("no ssh.authenticate failure row; events=%s", auditDump(h.audit.snapshot(), runID))
	}
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil || data["reason"] != reasonWindowsVMFeatureRefused {
		t.Errorf("failure row data = %s (%v), want reason %s", ev.Data, err, reasonWindowsVMFeatureRefused)
	}
}

// The terminal's ticket, a desktop file listing and a desktop sign-in pause each refuse a live Windows VM run
// 409 with the door's own sentence; the background watch answers that the run is not a background task.
func TestWindowsVMHTTPDoorsRefused(t *testing.T) {
	e := winvmExecHarness(t)
	for _, tc := range []struct{ method, suffix, door string }{
		{http.MethodPost, "/attach-ticket", "terminal"},
		{http.MethodGet, "/desktop/files", "desktop files, sign-ins or pause"},
		{http.MethodPost, "/desktop/sign-ins/pause", "desktop files, sign-ins or pause"},
	} {
		w := e.ownerDo(t, tc.method, tc.suffix, "")
		if w.Code != http.StatusConflict || errorReasonOf(w.Body.String()) != reasonWindowsVMFeatureRefused ||
			!strings.Contains(w.Body.String(), windowsDeskOnlyMsg(tc.door)) {
			t.Errorf("%s %s = %d %s, want 409 %s", tc.method, tc.suffix, w.Code, w.Body, reasonWindowsVMFeatureRefused)
		}
	}
	run := types.AgentRun{ID: e.id, MachineKind: types.MachineWindowsVM, Interactive: true, State: types.RunRunning}
	rf := e.srv.backgroundWatchRefusal(context.Background(), run)
	wantWinRefusal(t, "watch", rf, http.StatusConflict, reasonRunBackgroundWatchNotBackground, "")
}

// A Windows VM run's desktop ticket mints: ?purpose=ui asks the web application gateway's door, the run's one
// door. A ticket with any other purpose is still refused at the terminal's door, and the ui ticket, spent on the
// terminal's socket, is refused there.
func TestWindowsVMUITicketMintsForTheDesktopOnly(t *testing.T) {
	rig := newSupportTermRig(t)
	rig.run.MachineKind, rig.run.Interactive = types.MachineWindowsVM, true
	rig.st.putRun(rig.run)
	w := rig.mint(rig.owner, "?purpose=ui")
	if tk := rig.stored(w); tk.RunID != rig.run.ID || tk.Principal != supportOwnerSub {
		t.Fatalf("ui ticket = %+v, want the owner's for the run", tk)
	}
	var out struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"", "?purpose=terminal"} {
		w := rig.mint(rig.owner, query)
		if w.Code != http.StatusConflict || errorReasonOf(w.Body.String()) != reasonWindowsVMFeatureRefused ||
			!strings.Contains(w.Body.String(), windowsDeskOnlyMsg("terminal")) {
			t.Errorf("mint %q = %d %s, want 409 %s naming the terminal", query, w.Code, w.Body, reasonWindowsVMFeatureRefused)
		}
	}
	if c, status := rig.dial(out.Ticket); c != nil || status != http.StatusConflict {
		t.Errorf("the ui ticket on the terminal's socket: upgraded %v, status %d; want 409", c != nil, status)
	}
}

// The gateway takes a Windows VM run's ui ticket past its run-entry and background doors to the desktop arm,
// which opens the desktop's session.
func TestWindowsVMUITicketEntersTheDesktop(t *testing.T) {
	h, _ := newDesktopHarness(t)
	h.run.MachineKind, h.run.Interactive = types.MachineWindowsVM, true
	h.store.putRun(h.run)
	rec := h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != desktopPath(h, "/") {
		t.Fatalf("enter = %d to %q: %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
}
