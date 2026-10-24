// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/livebus"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A desktop recording's pause is held by each take over or sign-in by its id (#2968): two take overs
// on one run each hold it, a hold another replica ends is ended where it is held, and a pause the run
// outlives still gets its resume row.

// desktopRows counts the desktop.recording.pause and .resume rows audit holds.
func desktopRows(audit *recRecorder) (pauses, resumes int) {
	for _, ev := range audit.snapshot() {
		switch ev.Action {
		case "desktop.recording.pause":
			pauses++
		case "desktop.recording.resume":
			resumes++
		}
	}
	return pauses, resumes
}

// TestTakeover_TwoTakeOversHoldTheDesktopGapUntilBothHandBack: a run whose agent asked twice is paused
// by the first take over, and the first hand back does not resume it while the second is held.
func TestTakeover_TwoTakeOversHoldTheDesktopGapUntilBothHandBack(t *testing.T) {
	f := newTakeoverFixture(t, true)
	ast := f.srv.cfg.Store.(*authzStore)
	ast.mu.Lock()
	run := ast.runs[f.run]
	run.RecordingLevel = types.RecordingScreenActions
	ast.runs[f.run] = run
	ast.mu.Unlock()
	rec := liveDesktopRecorder(f.srv, f.run)
	a := f.seed(types.ApprovalHumanTakeover, types.ApprovalPending, "")
	b := f.seed(types.ApprovalHumanTakeover, types.ApprovalPending, "")
	post := func(id uuid.UUID, verb string) {
		t.Helper()
		if w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/"+verb, f.owner, ""); w.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", verb, id, w.Code, w.Body.String())
		}
	}
	post(a, "take-over")
	post(b, "take-over")
	_, _ = rec.Write(desktopFrame(desktopUpdate("typedsecret!")))
	post(a, "hand-back")
	_, _ = rec.Write(desktopFrame(desktopUpdate("typedagain!!")))
	post(b, "hand-back")
	_, _ = rec.Write(desktopFrame(desktopUpdate("afterresume!")))

	screen, gaps := recordedScreen(t, rec)
	if bytes.Contains(screen, []byte("typedsecret!")) || bytes.Contains(screen, []byte("typedagain!!")) {
		t.Error("the screen while a take over was still held is in the recording")
	}
	if !bytes.Contains(screen, []byte("afterresume!")) {
		t.Error("the screen after the last hand back is not in the recording")
	}
	if len(gaps) != 1 || gaps[0].EndMS == 0 {
		t.Errorf("gaps = %+v, want one closed gap", gaps)
	}
	if p, r := desktopRows(f.audit); p != 1 || r != 1 {
		t.Errorf("desktop.recording rows: %d pause, %d resume, want one of each", p, r)
	}
}

// TestDesktopRecording_AnotherReplicasEndResumesItHere: the recording_gap notice of a hold ended on
// another replica (the leader's sweep closing a sign-in begun here) resumes the recording here, with
// its resume row. A notice for another hold, a pause, or this replica's own notice does not.
func TestDesktopRecording_AnotherReplicasEndResumesItHere(t *testing.T) {
	audit := &recRecorder{}
	srv := New(Config{Audit: audit, AdminToken: adminToken, RecordingStore: fsRecordingStore(t)})
	run := types.AgentRun{ID: uuid.New(), RecordingLevel: types.RecordingScreenActions}
	rec := liveDesktopRecorder(srv, run.ID)
	hold := uuid.NewString()
	srv.PauseDesktopRecording(context.Background(), run, desktopPauseSignin, hold)
	notice := func(origin string, n recordingGapNotice) {
		raw, err := json.Marshal(n)
		if err != nil {
			t.Fatal(err)
		}
		srv.onRecordingGapNotice(livebus.Message{Kind: livebus.KindRecordingGap, Run: run.ID, Origin: origin, Data: raw})
	}
	notice("replica-b", recordingGapNotice{Reason: desktopSigninGapReason, DesktopHold: uuid.NewString()})
	notice("replica-b", recordingGapNotice{Paused: true, Reason: desktopSigninGapReason, DesktopHold: hold})
	notice(srv.replicaName(), recordingGapNotice{Reason: desktopSigninGapReason, DesktopHold: hold})
	_, _ = rec.Write(desktopFrame(desktopUpdate("typedsecret!")))
	if !srv.desktopRecs.holds(run.ID, hold) {
		t.Fatal("a notice for another hold, a pause or this replica's own resumed the recording")
	}
	notice("replica-b", recordingGapNotice{Reason: desktopSigninGapReason, DesktopHold: hold})
	_, _ = rec.Write(desktopFrame(desktopUpdate("afterresume!")))

	screen, _ := recordedScreen(t, rec)
	if bytes.Contains(screen, []byte("typedsecret!")) || !bytes.Contains(screen, []byte("afterresume!")) {
		t.Errorf("recorded screen = %q, want only what came after the other replica's end", screen)
	}
	if p, r := desktopRows(audit); p != 1 || r != 1 {
		t.Errorf("desktop.recording rows: %d pause, %d resume, want one of each", p, r)
	}
}

// TestDesktopSignin_EndOnAReplicaHoldingNoPauseTellsTheOthers: a sign-in on a screen-recorded run,
// ended on a replica that does not hold its pause, sends a recording_gap notice naming the hold.
func TestDesktopSignin_EndOnAReplicaHoldingNoPauseTellsTheOthers(t *testing.T) {
	srv, _ := miscCovBusServer(t, "replica-b")
	srv.cfg.RecordingStore = fsRecordingStore(t)
	logs := miscCovCaptureLogs(t)
	srv.endDesktopHold(context.Background(), types.AgentRun{ID: uuid.New(), RecordingLevel: types.RecordingScreenActions}, uuid.NewString())
	srv.WaitBackground()
	rec, ok := logs.find("a notice to the other replicas was not sent")
	if !ok {
		t.Fatal("the sign-in's end was not announced (no publish attempted)")
	}
	if v, _ := logs.attr(rec, "kind"); v.String() != livebus.KindRecordingGap {
		t.Errorf("kind = %v, want %s", v, livebus.KindRecordingGap)
	}
}

// TestDesktopRecording_AnEndedRunsForgottenPauseWritesItsResume: a pause the run outlives (its relay
// closed after the run ended, before any hand back) is forgotten with a resume row, so every pause
// row has one.
func TestDesktopRecording_AnEndedRunsForgottenPauseWritesItsResume(t *testing.T) {
	ast := newAuthzStore()
	h := newHarness(t)
	cfg := baseTestConfig(h, ast)
	cfg.RecordingStore = fsRecordingStore(t)
	srv := New(cfg)
	run := types.AgentRun{ID: uuid.New(), State: types.RunCompleted, RecordingLevel: types.RecordingScreenActions}
	ast.mu.Lock()
	ast.runs[run.ID] = run
	ast.mu.Unlock()
	srv.PauseDesktopRecording(context.Background(), run, desktopPauseTakeover, uuid.NewString())
	sandbox, relay := net.Pipe()
	conn := srv.recordDesktopConn(run, uiSession{Run: run.ID, App: desktopApp, Principal: "alice"}, relay, nil)
	_ = sandbox.Close()
	_ = conn.Close()
	if p, r := desktopRows(h.audit); p != 1 || r != 1 {
		t.Errorf("desktop.recording rows: %d pause, %d resume, want one of each", p, r)
	}
	for _, ev := range h.audit.snapshot() {
		if ev.Action == "desktop.recording.resume" {
			if d := auditData(t, ev); d["reason"] != desktopPauseTakeover || d["gap_end"] == nil {
				t.Errorf("resume row = %v, want the gap's reason and end", d)
			}
		}
	}
}

// A take over's hold on a desktop recording lives in the memory of the replica that answered it, and
// the approvals row says it is open (APPROVED, not handed back): a recorded desktop on any replica, or
// after a restart, reads the row as it connects and at each re-read (vmSignInWatch.sync).

// newTakeoverDesktop is a recorded Linux VM desktop with no sign-in open (newVMSignInDesktop) whose
// run's take overs live in the takeoverApprovals CAS, which its store reads as Postgres reads the
// approvals table.
func newTakeoverDesktop(t *testing.T) (*vmSignInDesktop, takeoverApprovals) {
	t.Helper()
	d := newVMSignInDesktop(t, store.DesktopVMSignIns{Store: "memory"})
	aap := takeoverApprovals{newAuthzApprovals(newAuthzStore())}
	d.st.setTakeovers(aap.authzApprovals, nil)
	d.h.srv.cfg.Approvals = aap
	return d, aap
}

// seedTakeover adds a human_takeover request on run in state; an APPROVED one is held by holder.
func seedTakeover(aap takeoverApprovals, run uuid.UUID, state types.ApprovalState, holder string) uuid.UUID {
	id := uuid.New()
	now := time.Now().UTC()
	ap := types.ApprovalRequest{ID: id, RunID: run, Kind: types.ApprovalHumanTakeover, State: state, TakeoverHolder: holder,
		RequestedScope: json.RawMessage(`{"reason":"the sign-in sends a code to a phone"}`), RequestedAt: now.Add(-2 * time.Minute)}
	if state == types.ApprovalApproved {
		ap.DecidedAt, ap.DecidedBy = &now, holder
	}
	aap.mu.Lock()
	aap.byID[id] = ap
	aap.mu.Unlock()
	return id
}

// takeoverAnswer answers POST /approvals/{id}/<verb> ("take-over" or "hand-back") on srv as owner,
// called past the router (vmSignInDesktop.signIn): the UI harness serves no SSO routes.
func takeoverAnswer(srv *Server, owner string, id uuid.UUID, verb string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/"+id.String()+"/"+verb, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id.String())
	w := httptest.NewRecorder()
	answer := srv.handleHandBackApproval
	if verb == "take-over" {
		answer = srv.handleTakeOverApproval
	}
	answer(w, r.WithContext(context.WithValue(withOIDCHuman(r.Context(), owner), chi.RouteCtxKey, rctx)))
	return w
}

// uiDesktopRows counts the UI harness's desktop.recording.pause and .resume rows.
func uiDesktopRows(h *uiHarness) (pauses, resumes int) {
	return len(h.audit.auditRows("desktop.recording.pause", "success")), len(h.audit.auditRows("desktop.recording.resume", "success"))
}

// TestTakeover_ARestartedServerHoldsANewDesktopUntilTheHandBack: a server that never saw the take over,
// as after a restart, holds a desktop that connects while it is open: what the VM sends is not
// recorded, and the hand back there resumes it, with one resume row for the one pause row it wrote.
func TestTakeover_ARestartedServerHoldsANewDesktopUntilTheHandBack(t *testing.T) {
	d, aap := newTakeoverDesktop(t)
	h := d.h
	id := seedTakeover(aap, h.run.ID, types.ApprovalApproved, h.owner)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v := openVMDesktop(t, ctx, h, d.dr)
	if !h.srv.desktopRecs.holds(h.run.ID, takeoverHold(id)) {
		t.Fatal("a desktop that connected while a take over is open is not held")
	}
	d.send <- vmFBU // sent while the take over is open: the viewer sees it, the recording does not
	v.read(len(vmFBU))
	if w := takeoverAnswer(h.srv, h.owner, id, "hand-back"); w.Code != http.StatusOK {
		t.Fatalf("hand back = %d %s", w.Code, w.Body.String())
	}
	if h.srv.desktopRecs.holds(h.run.ID, takeoverHold(id)) {
		t.Fatal("the hand back left the desktop recording held")
	}
	// The row is handed back: the re-read takes no hold and writes no second resume row.
	d.reread(t)
	d.send <- []byte("\x02\x02") // two Bells, after the hand back
	v.read(2)
	endVMDesktop(t, h, v, d.send)

	_, pb, kept := storedVMRecording(t, h)
	if want := vmHandshake() + "\x02\x02"; kept != want {
		t.Errorf("recorded frames = %q, want the handshake and the Bells after the hand back %q", kept, want)
	}
	if len(pb.Gaps) != 1 || pb.Gaps[0].Reason != desktopPauseTakeover || pb.Gaps[0].EndMS == 0 {
		t.Errorf("playback gaps = %+v, want one closed %q gap", pb.Gaps, desktopPauseTakeover)
	}
	if p, r := uiDesktopRows(h); p != 1 || r != 1 {
		t.Errorf("desktop.recording rows: %d pause, %d resume, want one of each", p, r)
	}
}

// TestTakeover_ATakeOverOnAnotherReplicaHoldsAnOpenDesktop: a take over answered on another replica
// reaches a desktop already open on this one at its next re-read, and so does the hand back there:
// nothing the VM sends in between is recorded. Each replica writes its own pause row and its resume.
func TestTakeover_ATakeOverOnAnotherReplicaHoldsAnOpenDesktop(t *testing.T) {
	d, aap := newTakeoverDesktop(t)
	h := d.h
	other := New(h.srv.cfg) // another replica over the same database and runner
	id := seedTakeover(aap, h.run.ID, types.ApprovalPending, "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v := openVMDesktop(t, ctx, h, d.dr)
	d.send <- []byte("\x02") // a Bell before the take over
	v.read(1)
	if w := takeoverAnswer(other, h.owner, id, "take-over"); w.Code != http.StatusOK {
		t.Fatalf("take over on the other replica = %d %s", w.Code, w.Body.String())
	}
	d.reread(t)
	if !h.srv.desktopRecs.holds(h.run.ID, takeoverHold(id)) {
		t.Fatal("a take over answered on another replica did not hold this replica's open desktop at its re-read")
	}
	d.send <- vmFBU // sent while the take over is open
	v.read(len(vmFBU))
	if w := takeoverAnswer(other, h.owner, id, "hand-back"); w.Code != http.StatusOK {
		t.Fatalf("hand back on the other replica = %d %s", w.Code, w.Body.String())
	}
	d.reread(t)
	if h.srv.desktopRecs.holds(h.run.ID, takeoverHold(id)) {
		t.Fatal("the hand back on another replica left this replica's desktop recording held after its re-read")
	}
	d.send <- []byte("\x02\x02") // two Bells after the hand back
	v.read(2)
	endVMDesktop(t, h, v, d.send)

	_, pb, kept := storedVMRecording(t, h)
	if want := vmHandshake() + "\x02" + "\x02\x02"; kept != want {
		t.Errorf("recorded frames = %q, want the handshake and the Bells either side of the take over %q", kept, want)
	}
	if len(pb.Gaps) != 1 || pb.Gaps[0].Reason != desktopPauseTakeover || pb.Gaps[0].EndMS == 0 {
		t.Errorf("playback gaps = %+v, want one closed %q gap", pb.Gaps, desktopPauseTakeover)
	}
	if p, r := uiDesktopRows(h); p != 2 || r != 2 {
		t.Errorf("desktop.recording rows: %d pause, %d resume, want two of each (one per replica)", p, r)
	}
}

// TestTakeover_AnUnreadableTakeOverRefusesTheDesktop: a recorded desktop whose run's take overs cannot
// be read is refused with the sign-in mark's 503 before the runner is dialled.
func TestTakeover_AnUnreadableTakeOverRefusesTheDesktop(t *testing.T) {
	d, _ := newTakeoverDesktop(t)
	h := d.h
	srv := httptest.NewServer(h.gateway)
	t.Cleanup(srv.Close)
	c := desktopCookie(t, h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser)))
	d.st.setTakeovers(nil, errors.New("store: pool closed"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+desktopPath(h, uiDesktopRFB),
		&websocket.DialOptions{HTTPHeader: http.Header{"Cookie": {c.Name + "=" + c.Value}}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("dial = %v %v, want a 503 refusal", resp, err)
	}
	if body, _ := io.ReadAll(resp.Body); !strings.Contains(string(body), reasonDesktopSignInsUnavailable) {
		t.Fatalf("refusal = %s, want %s", body, reasonDesktopSignInsUnavailable)
	}
	select {
	case ref := <-d.dr.dialled:
		t.Fatalf("dialled %q with the take overs unreadable", ref)
	default:
	}
}

// TestTakeover_NoOpenTakeOverKeepsTheDesktopsFrames: a run whose take overs are all still waiting or
// handed back keeps its desktop's frames, with no gap and no pause row.
func TestTakeover_NoOpenTakeOverKeepsTheDesktopsFrames(t *testing.T) {
	d, aap := newTakeoverDesktop(t)
	h := d.h
	seedTakeover(aap, h.run.ID, types.ApprovalPending, "")
	back := seedTakeover(aap, h.run.ID, types.ApprovalApproved, h.owner)
	aap.mu.Lock()
	ap := aap.byID[back]
	at := time.Now().UTC()
	ap.HandedBackAt = &at
	aap.byID[back] = ap
	aap.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v := openVMDesktop(t, ctx, h, d.dr)
	d.send <- []byte("\x02")
	v.read(1)
	endVMDesktop(t, h, v, d.send)

	_, pb, kept := storedVMRecording(t, h)
	if want := vmHandshake() + "\x02"; kept != want {
		t.Errorf("recorded frames = %q, want the handshake and the Bell %q", kept, want)
	}
	if len(pb.Gaps) != 0 {
		t.Errorf("playback gaps = %+v, want none", pb.Gaps)
	}
	if p, r := uiDesktopRows(h); p != 0 || r != 0 {
		t.Errorf("desktop.recording rows: %d pause, %d resume, want none", p, r)
	}
}

// TestTakeover_AContainersNoVNCAppHoldsAnOpenTakeOver: a container run's noVNC app, which has no
// sign-in row, is held from the first frame by a take over a server never saw, as after a restart.
func TestTakeover_AContainersNoVNCAppHoldsAnOpenTakeOver(t *testing.T) {
	offered, pasted := make(chan string, 1), make(chan []byte, 1)
	h := desktopHarness(t, rfbDesktopBackend(t, offered, pasted, false), nil)
	h.run.RecordingLevel = types.RecordingScreenActions
	h.store.putRun(h.run)
	h.srv.cfg.RecordingStore = fsRecordingStore(t)
	aap := takeoverApprovals{newAuthzApprovals(newAuthzStore())}
	id := seedTakeover(aap, h.run.ID, types.ApprovalApproved, h.owner)
	st := &vmSignInTestStore{uiMemStore: h.store}
	st.setTakeovers(aap.authzApprovals, nil)
	h.srv.cfg.Store = st
	ds := openDesktop(t, h, desktopApp)
	if ds.resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(ds.resp.Body)
		t.Fatalf("upgrade = %d %s, want 101", ds.resp.StatusCode, body)
	}
	if !h.srv.desktopRecs.holds(h.run.ID, takeoverHold(id)) {
		t.Fatal("a container's desktop that connected while a take over is open is not held")
	}
	<-offered
	if next, err := readWSFrame(ds.br); err != nil || string(next) != "\x02" {
		t.Fatalf("after ServerInit the browser got %q (err %v), want the Bell", next, err)
	}
	_ = ds.conn.Close()
	_, pb, kept := storedVMRecording(t, h)
	if kept != vmHandshake() {
		t.Errorf("recorded frames = %q, want only the handshake: the Bell came while the take over was open", kept)
	}
	if len(pb.Gaps) != 1 || pb.Gaps[0].Reason != desktopPauseTakeover {
		t.Errorf("playback gaps = %+v, want one %q gap", pb.Gaps, desktopPauseTakeover)
	}
}
