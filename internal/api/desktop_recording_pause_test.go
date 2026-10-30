// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The desktop's screen recording keeps no frames while a person drives (#2968): from a "Needs you"
// take over to its hand back, and from a desktop sign-in's begin to its close.

// desktopUpdate is a FramebufferUpdate of one 3x1 raw rectangle whose 12 pixel bytes are px.
func desktopUpdate(px string) string {
	return "\x00\x00\x00\x01" + "\x00\x00\x00\x00\x00\x03\x00\x01\x00\x00\x00\x00" + px
}

// liveDesktopRecorder is a recorder past its handshake, as a relayed desktop connection on run holds.
func liveDesktopRecorder(srv *Server, run uuid.UUID) *recording.RFBRecorder {
	rec := recording.NewRFBRecorder(time.Now)
	srv.desktopRecs.add(run, rec)
	_, _ = rec.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n\r\n"))
	_, _ = rec.Write(desktopFrame(desktopHello))
	return rec
}

// recordedScreen is the bytes rec's playback frames carry, and its gaps.
func recordedScreen(t *testing.T, rec *recording.RFBRecorder) ([]byte, []recording.RFBGap) {
	t.Helper()
	doc, _ := rec.Playback()
	var pb recording.RFBPlayback
	if err := json.Unmarshal(doc, &pb); err != nil {
		t.Fatal(err)
	}
	var out []byte
	for _, f := range pb.Frames {
		_, data, _ := strings.Cut(f[1:], "{")
		b, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b...)
	}
	return out, pb.Gaps
}

// checkNoTypedBytes: what the desktop showed while the person drove is not in the recording, what it
// showed after is, and the gap and both audit rows name reason.
func checkNoTypedBytes(t *testing.T, rec *recording.RFBRecorder, audit *recRecorder, reason string) {
	t.Helper()
	screen, gaps := recordedScreen(t, rec)
	if bytes.Contains(screen, []byte("typedsecret!")) {
		t.Error("the screen while the person drove is in the recording")
	}
	if !bytes.Contains(screen, []byte("afterresume!")) {
		t.Error("the screen after the person handed back is not in the recording")
	}
	if len(gaps) != 1 || gaps[0].Reason != reason || gaps[0].EndMS == 0 {
		t.Errorf("gaps = %+v, want one closed gap for %q", gaps, reason)
	}
	rows := map[string]string{}
	for _, ev := range audit.snapshot() {
		if strings.HasPrefix(ev.Action, "desktop.recording.") {
			rows[ev.Action], _ = auditData(t, ev)["reason"].(string)
		}
	}
	if rows["desktop.recording.pause"] != reason || rows["desktop.recording.resume"] != reason {
		t.Errorf("desktop.recording rows = %v, want pause and resume for %q", rows, reason)
	}
}

// TestTakeover_PausesTheDesktopRecordingUntilHandBack: on a run that records its screen, take over
// pauses the desktop's recording and hand back resumes it.
func TestTakeover_PausesTheDesktopRecordingUntilHandBack(t *testing.T) {
	f := newTakeoverFixture(t, true)
	ast := f.srv.cfg.Store.(*authzStore)
	ast.mu.Lock()
	run := ast.runs[f.run]
	run.RecordingLevel = types.RecordingScreenActions
	ast.runs[f.run] = run
	ast.mu.Unlock()
	rec := liveDesktopRecorder(f.srv, f.run)
	id := f.seed(types.ApprovalHumanTakeover, types.ApprovalPending, "")

	if w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/take-over", f.owner, ""); w.Code != http.StatusOK {
		t.Fatalf("take over: %d %s", w.Code, w.Body.String())
	}
	_, _ = rec.Write(desktopFrame(desktopUpdate("typedsecret!")))
	if w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/hand-back", f.owner, ""); w.Code != http.StatusOK {
		t.Fatalf("hand back: %d %s", w.Code, w.Body.String())
	}
	_, _ = rec.Write(desktopFrame(desktopUpdate("afterresume!")))
	checkNoTypedBytes(t, rec, f.audit, desktopPauseTakeover)
}

// TestDesktopSignin_PauseLeavesNoTypedBytesInTheRecording: on a run that records its screen, a sign-in
// pauses the desktop's recording before the person's display starts and its end resumes it.
func TestDesktopSignin_PauseLeavesNoTypedBytesInTheRecording(t *testing.T) {
	f := newSigninFixture(t)
	f.run.RecordingLevel = types.RecordingScreenActions
	f.st.putRun(f.run)
	f.srv.cfg.RecordingStore = fsRecordingStore(t)
	rec := liveDesktopRecorder(f.srv, f.run.ID)

	f.begin(t)
	_, _ = rec.Write(desktopFrame(desktopUpdate("typedsecret!")))
	if code, body := f.post(t, "/desktop-signin/end", ""); code != http.StatusNoContent {
		t.Fatalf("end = %d %s, want 204", code, body)
	}
	_, _ = rec.Write(desktopFrame(desktopUpdate("afterresume!")))
	checkNoTypedBytes(t, rec, f.h.audit, desktopPauseSignin)
}

// TestDesktopRecording_ResumeEndsOnlyItsOwnPause: a sign-in's end never resumes a recording a take
// over paused.
func TestDesktopRecording_ResumeEndsOnlyItsOwnPause(t *testing.T) {
	srv := New(Config{Audit: &recRecorder{}, AdminToken: adminToken, RecordingStore: fsRecordingStore(t)})
	run := types.AgentRun{ID: uuid.New(), RecordingLevel: types.RecordingScreenActions}
	if !srv.PauseDesktopRecording(context.Background(), run, desktopPauseTakeover, desktopPauseTakeover) {
		t.Fatal("pause refused")
	}
	if srv.ResumeDesktopRecording(context.Background(), run, desktopPauseSignin) {
		t.Fatal("a sign-in's end resumed a take over's pause")
	}
	if !srv.ResumeDesktopRecording(context.Background(), run, desktopPauseTakeover) {
		t.Fatal("the hand back did not resume its own pause")
	}
}

// TestDesktopRecording_OverlappingPausesHoldOneGap: a take over and a sign-in that overlap, in either
// order, hold one gap until the last of them ends. Nothing shown while either is open is recorded,
// and one pause row and one resume row are written, both naming the reason that opened the gap.
func TestDesktopRecording_OverlappingPausesHoldOneGap(t *testing.T) {
	for name, order := range map[string][2]string{
		"take over then sign-in": {desktopPauseTakeover, desktopPauseSignin},
		"sign-in then take over": {desktopPauseSignin, desktopPauseTakeover},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			audit := &recRecorder{}
			srv := New(Config{Audit: audit, AdminToken: adminToken, RecordingStore: fsRecordingStore(t)})
			run := types.AgentRun{ID: uuid.New(), RecordingLevel: types.RecordingScreenActions}
			rec := liveDesktopRecorder(srv, run.ID)
			first, second := order[0], order[1]
			if !srv.PauseDesktopRecording(ctx, run, first, first) {
				t.Fatal("the first pause did not open the gap")
			}
			_, _ = rec.Write(desktopFrame(desktopUpdate("typedsecret!")))
			if srv.PauseDesktopRecording(ctx, run, second, second) {
				t.Error("a second pause opened another gap")
			}
			if srv.ResumeDesktopRecording(ctx, run, first) {
				t.Error("the recording resumed while " + second + " still holds it")
			}
			_, _ = rec.Write(desktopFrame(desktopUpdate("typedagain!!")))
			if !srv.ResumeDesktopRecording(ctx, run, second) {
				t.Fatal("the last reason's end did not resume the recording")
			}
			_, _ = rec.Write(desktopFrame(desktopUpdate("afterresume!")))

			screen, gaps := recordedScreen(t, rec)
			if bytes.Contains(screen, []byte("typedsecret!")) || bytes.Contains(screen, []byte("typedagain!!")) {
				t.Error("the screen while a person drove is in the recording")
			}
			if !bytes.Contains(screen, []byte("afterresume!")) {
				t.Error("the screen after the last hand back is not in the recording")
			}
			if len(gaps) != 1 || gaps[0].Reason != first || gaps[0].EndMS == 0 {
				t.Errorf("gaps = %+v, want one closed gap named %q", gaps, first)
			}
			rows := map[string][]string{}
			for _, ev := range audit.snapshot() {
				if strings.HasPrefix(ev.Action, "desktop.recording.") {
					reason, _ := auditData(t, ev)["reason"].(string)
					rows[ev.Action] = append(rows[ev.Action], reason)
				}
			}
			if len(rows["desktop.recording.pause"]) != 1 || len(rows["desktop.recording.resume"]) != 1 ||
				rows["desktop.recording.pause"][0] != first || rows["desktop.recording.resume"][0] != first {
				t.Errorf("desktop.recording rows = %v, want one pause and one resume naming %q", rows, first)
			}
		})
	}
}

// TestDesktopRecording_AnEndedRunsPauseIsForgotten: when a paused run's last desktop connection closes,
// its pause is kept while the run lives (a new connection starts paused) and dropped once it ended.
func TestDesktopRecording_AnEndedRunsPauseIsForgotten(t *testing.T) {
	ast := newAuthzStore()
	cfg := baseTestConfig(newHarness(t), ast)
	cfg.RecordingStore = fsRecordingStore(t)
	srv := New(cfg)
	run := types.AgentRun{ID: uuid.New(), State: types.RunRunning, RecordingLevel: types.RecordingScreenActions}
	ast.mu.Lock()
	ast.runs[run.ID] = run
	ast.mu.Unlock()
	held := func() bool {
		srv.desktopRecs.mu.Lock()
		defer srv.desktopRecs.mu.Unlock()
		_, ok := srv.desktopRecs.byRun[run.ID]
		return ok
	}
	closeOne := func() {
		sandbox, relay := net.Pipe()
		conn := srv.recordDesktopConn(run, uiSession{Run: run.ID, App: desktopApp, Principal: "alice"}, relay, nil)
		_ = sandbox.Close()
		_ = conn.Close()
	}
	srv.PauseDesktopRecording(context.Background(), run, desktopPauseTakeover, desktopPauseTakeover)
	closeOne()
	if !held() {
		t.Fatal("a live run's pause was dropped when its connection closed")
	}
	ast.mu.Lock()
	ended := ast.runs[run.ID]
	ended.State = types.RunCompleted
	ast.runs[run.ID] = ended
	ast.mu.Unlock()
	closeOne()
	if held() {
		t.Fatal("an ended run's pause is still held")
	}
}

// TestDesktopRecording_AnEndedRunsIdlePauseIsSwept (#3583): a take over's pause still open when its
// run ends, with no desktop connection left, is closed by the reconciler's pass: the hold is gone and
// one desktop.recording.resume row is written, naming the gap's reason and end_reason run_ended. A
// live run's pause stands. Whether the pass meets it alone, after the last disconnect closed it, or
// racing that disconnect, and however many passes run, the gap gets one resume row.
func TestDesktopRecording_AnEndedRunsIdlePauseIsSwept(t *testing.T) {
	for _, tc := range []struct {
		name string
		conn bool // a desktop connection is open when the run ends
		end  func(closeConn, sweep func())
	}{
		{"no connection left", false, func(_, sweep func()) { sweep(); sweep() }},
		{"the last disconnect first", true, func(closeConn, sweep func()) { closeConn(); sweep(); sweep() }},
		{"the last disconnect racing a pass", true, func(closeConn, sweep func()) {
			var wg sync.WaitGroup
			wg.Go(closeConn)
			wg.Go(sweep)
			wg.Wait()
			sweep()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newHarness(t)
			ast := newAuthzStore()
			cfg := baseTestConfig(h, ast)
			cfg.RecordingStore = fsRecordingStore(t)
			srv := New(cfg)
			run := types.AgentRun{ID: uuid.New(), State: types.RunRunning, RecordingLevel: types.RecordingScreenActions}
			ast.mu.Lock()
			ast.runs[run.ID] = run
			ast.mu.Unlock()
			held := func() bool {
				srv.desktopRecs.mu.Lock()
				defer srv.desktopRecs.mu.Unlock()
				_, ok := srv.desktopRecs.byRun[run.ID]
				return ok
			}
			sweep := func() {
				if err := srv.sweepEndedDesktopRecordingPauses(ctx); err != nil {
					t.Error(err)
				}
			}
			srv.PauseDesktopRecording(ctx, run, desktopPauseTakeover, takeoverHold(uuid.New()))
			closeConn := func() {}
			if tc.conn {
				sandbox, relay := net.Pipe()
				conn := srv.recordDesktopConn(run, uiSession{Run: run.ID, App: desktopApp, Principal: "alice"}, relay, nil)
				closeConn = func() {
					_ = sandbox.Close()
					_ = conn.Close()
				}
			}
			sweep()
			if !held() {
				t.Fatal("a live run's pause was swept")
			}
			ast.mu.Lock()
			ended := ast.runs[run.ID]
			ended.State = types.RunCompleted
			ast.runs[run.ID] = ended
			ast.mu.Unlock()
			tc.end(closeConn, sweep)
			if held() {
				t.Fatal("an ended run's pause is still held")
			}
			var resumes []map[string]any
			for _, ev := range h.audit.snapshot() {
				if ev.Action == "desktop.recording.resume" {
					resumes = append(resumes, auditData(t, ev))
				}
			}
			if len(resumes) != 1 || resumes[0]["reason"] != desktopPauseTakeover || resumes[0]["end_reason"] != signinEndRunEnded {
				t.Fatalf("desktop.recording.resume rows = %v, want one naming %q with end_reason %q", resumes, desktopPauseTakeover, signinEndRunEnded)
			}
		})
	}
}

// TestDesktopRecording_AKeptRunThatEndedIsOver (#3583): a kept run whose lease ended or whose
// container exited stays RUNNING but is over (desktopPauseRunOver): an idle take over's pause is
// swept with one resume row marked run_ended. A run lost to an outage may be revived, and a live run
// lives: their pause stands and no resume row is written.
func TestDesktopRecording_AKeptRunThatEndedIsOver(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason types.LostReason // "" leaves the run live
		over   bool
	}{
		{"kept, lease ended", types.LostEnded, true},
		{"kept, rebooted", types.LostReboot, true},
		{"lost to an outage", types.LostOutage, false},
		{"live", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newHarness(t)
			ast := newAuthzStore()
			cfg := baseTestConfig(h, ast)
			cfg.RecordingStore = fsRecordingStore(t)
			srv := New(cfg)
			run := types.AgentRun{ID: uuid.New(), State: types.RunRunning, RecordingLevel: types.RecordingScreenActions}
			if tc.reason != "" {
				lostAt := time.Now().UTC()
				run.LostAt, run.LostReason = &lostAt, tc.reason
			}
			ast.mu.Lock()
			ast.runs[run.ID] = run
			ast.mu.Unlock()
			srv.PauseDesktopRecording(ctx, run, desktopPauseTakeover, takeoverHold(uuid.New()))
			for range 2 {
				if err := srv.sweepEndedDesktopRecordingPauses(ctx); err != nil {
					t.Fatal(err)
				}
			}
			srv.desktopRecs.mu.Lock()
			_, held := srv.desktopRecs.byRun[run.ID]
			srv.desktopRecs.mu.Unlock()
			var resumes []map[string]any
			for _, ev := range h.audit.snapshot() {
				if ev.Action == "desktop.recording.resume" {
					resumes = append(resumes, auditData(t, ev))
				}
			}
			switch {
			case tc.over && (held || len(resumes) != 1 || resumes[0]["end_reason"] != signinEndRunEnded):
				t.Fatalf("held %v, desktop.recording.resume rows = %v, want the pause swept with one row marked %q", held, resumes, signinEndRunEnded)
			case !tc.over && (!held || len(resumes) != 0):
				t.Fatalf("held %v, desktop.recording.resume rows = %v, want the pause standing and no row", held, resumes)
			}
		})
	}
}
