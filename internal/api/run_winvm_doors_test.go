package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/audit"
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

// A live egress edit that would revive a Windows VM run's proxy is refused, and replaces no proxy.
func TestWindowsVMEgressEditRefused(t *testing.T) {
	f := newEgressEditFixture(t)
	f.st.run.MachineKind = types.MachineWindowsVM
	f.run = f.st.run
	w := f.editEgress(t, `{"deny_hosts":["evil.example"]}`)
	if w.Code != http.StatusConflict || errorReasonOf(w.Body.String()) != reasonWindowsVMFeatureRefused ||
		!strings.Contains(w.Body.String(), windowsDeskOnlyMsg("revive")) {
		t.Errorf("egress edit = %d %s, want 409 %s", w.Code, w.Body, reasonWindowsVMFeatureRefused)
	}
	if len(f.rr.replaced) != 0 {
		t.Errorf("ReplaceProxy calls = %d, want none", len(f.rr.replaced))
	}
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
		{http.MethodGet, "/desktop/files", "desktop"},
		{http.MethodPost, "/desktop/sign-ins/pause", "desktop"},
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
