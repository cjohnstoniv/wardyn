// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
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
	if !srv.PauseDesktopRecording(context.Background(), run, desktopPauseTakeover) {
		t.Fatal("pause refused")
	}
	if srv.ResumeDesktopRecording(context.Background(), run, desktopPauseSignin) {
		t.Fatal("a sign-in's end resumed a take over's pause")
	}
	if !srv.ResumeDesktopRecording(context.Background(), run, desktopPauseTakeover) {
		t.Fatal("the hand back did not resume its own pause")
	}
}
