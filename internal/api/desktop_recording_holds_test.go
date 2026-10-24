// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/livebus"
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
