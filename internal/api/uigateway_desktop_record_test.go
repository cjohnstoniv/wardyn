// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// vmKeyEvent is the viewer pressing "q"; vmFBU is a FramebufferUpdate with no rectangles.
var (
	vmKeyEvent = []byte{4, 1, 0, 0, 0, 0, 0, 'q'}
	vmFBU      = []byte{0, 0, 0, 0}
)

// vmHandshake is everything qemuDisplay sends before its first server message.
func vmHandshake() string {
	return "RFB 003.008\n" + "\x01\x01" + "\x00\x00\x00\x00" + string(rfbServerInit())
}

// vmDesktopRun is a desktop harness on a run recorded at screen_actions, in an organisation that keeps
// recordings; the VM's display sends what send carries and hands what it reads to got.
func vmDesktopRun(t *testing.T) (h *uiHarness, dr *desktopFakeRunner, send chan []byte, got chan []byte) {
	t.Helper()
	h, dr = newDesktopHarness(t)
	h.run.RecordingLevel = types.RecordingScreenActions
	h.store.putRun(h.run)
	h.srv.cfg.RecordingStore = fsRecordingStore(t)
	send, got = make(chan []byte, 8), make(chan []byte, 8)
	dr.after = func(b net.Conn) {
		go func() {
			for {
				buf := make([]byte, 256)
				n, err := b.Read(buf)
				if n > 0 {
					got <- buf[:n]
				}
				if err != nil {
					return
				}
			}
		}()
		for m := range send {
			if _, err := b.Write(m); err != nil {
				return
			}
		}
	}
	return h, dr, send, got
}

// openVMDesktop opens the run's desktop and answers the VM's handshake as the viewer does.
func openVMDesktop(t *testing.T, ctx context.Context, h *uiHarness, dr *desktopFakeRunner) *desktopViewer {
	t.Helper()
	srv := httptest.NewServer(h.gateway)
	t.Cleanup(srv.Close)
	v := dialDesktop(t, ctx, h, srv.URL, desktopCookie(t, h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser))))
	<-dr.dialled
	v.handshake()
	return v
}

// endVMDesktop closes the VM's display and reads the viewer's socket to its close frame, then waits for
// the relay's ui.close row: by then the relay has closed the display and stored any recording.
func endVMDesktop(t *testing.T, h *uiHarness, v *desktopViewer, send chan []byte) {
	t.Helper()
	close(send)
	if _, _, err := v.ws.Read(v.ctx); err == nil {
		t.Fatal("the viewer's socket stayed open after the VM closed its display")
	}
	for i := 0; i < 500 && len(h.audit.auditRows("ui.close", "success")) == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if len(h.audit.auditRows("ui.close", "success")) == 0 {
		t.Fatal("the desktop relay never closed")
	}
}

// storedVMRecording waits for the desktop.recording.write row and reads the playback it names, with
// the frames' bytes joined.
func storedVMRecording(t *testing.T, h *uiHarness) (map[string]any, recording.RFBPlayback, string) {
	t.Helper()
	var rows []types.AuditEvent
	for i := 0; i < 500 && len(rows) == 0; i++ {
		if rows = h.audit.auditRows("desktop.recording.write", "success"); len(rows) == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("desktop.recording.write rows = %d, want 1: the VM desktop session was not recorded, or kept no frame", len(rows))
	}
	write := auditData(t, rows[0])
	rc, err := h.srv.cfg.RecordingStore.OpenCast(context.Background(), write["key"].(string))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	var pb recording.RFBPlayback
	if err := json.NewDecoder(rc).Decode(&pb); err != nil {
		t.Fatal(err)
	}
	var joined strings.Builder
	for _, f := range pb.Frames {
		_, b64, _ := strings.Cut(strings.TrimPrefix(f, "{"), "{")
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatalf("frame %q: %v", f, err)
		}
		joined.Write(raw)
	}
	return write, pb, joined.String()
}

// TestVMDesktopRecording_YieldsACastWithThePauseGap: a Linux VM's desktop is recorded like the noVNC
// app's. The session yields the same cast a container's desktop does (a desktop- session under the
// run's cast key, holding noVNC playback of the VM's frames); a pause keeps none of what the VM sends
// while it is open, leaves a gap in the playback and writes desktop.recording.pause and a
// desktop.recording.resume row carrying that gap.
func TestVMDesktopRecording_YieldsACastWithThePauseGap(t *testing.T) {
	h, dr, send, _ := vmDesktopRun(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v := openVMDesktop(t, ctx, h, dr)
	if !h.srv.PauseDesktopRecording(ctx, h.run, desktopPauseSignin, desktopPauseSignin) {
		t.Fatal("pause refused")
	}
	send <- vmFBU // sent while paused: the viewer sees it, the recording does not
	v.read(len(vmFBU))
	if !h.srv.ResumeDesktopRecording(ctx, h.run, desktopPauseSignin) {
		t.Fatal("resume refused")
	}
	send <- []byte("\x02\x02") // two Bells
	v.read(2)
	endVMDesktop(t, h, v, send)

	write, pb, kept := storedVMRecording(t, h)
	session, _ := write["session"].(string)
	if write["format"] != "novnc-playback" || !strings.HasPrefix(session, desktopRecordingSuffix) ||
		write["key"] != recording.CastKey(h.run.ID.String(), session) || pb.Format != "novnc-playback" || pb.Version != 1 {
		t.Errorf("write row = %v, playback %s v%d: want a desktop cast under the run's cast key", write, pb.Format, pb.Version)
	}
	if want := vmHandshake() + "\x02\x02"; kept != want {
		t.Errorf("recorded frames = %q, want the handshake and the Bells after the resume %q", kept, want)
	}
	if len(pb.Gaps) != 1 || pb.Gaps[0].Reason != desktopPauseSignin || pb.Gaps[0].EndMS == 0 {
		t.Errorf("playback gaps = %+v, want one closed %q gap", pb.Gaps, desktopPauseSignin)
	}
	pause, resume := h.audit.auditRows("desktop.recording.pause", "success"), h.audit.auditRows("desktop.recording.resume", "success")
	if len(pause) != 1 || len(resume) != 1 {
		t.Fatalf("pause rows %d, resume rows %d, want 1 each", len(pause), len(resume))
	}
	p, r := auditData(t, pause[0]), auditData(t, resume[0])
	if r["reason"] != desktopPauseSignin || r["gap_start"] != p["gap_start"] || r["gap_end"] == nil || r["gap_ms"] == nil {
		t.Errorf("resume row = %v, want the gap the pause %v opened", r, p)
	}
}

// TestVMDesktopRecording_NeverKeepsTheViewersSide: what the viewer sends (a key press, a paste) reaches
// the VM and never the recording, which holds exactly what the VM sent and was never stopped by bytes
// it could not follow.
func TestVMDesktopRecording_NeverKeepsTheViewersSide(t *testing.T) {
	h, dr, send, got := vmDesktopRun(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v := openVMDesktop(t, ctx, h, dr)
	typed := append(append([]byte(nil), vmKeyEvent...), rfbCut(6, pasteSentinel)...)
	v.write(typed)
	var reached []byte
	for len(reached) < len(typed) {
		select {
		case b := <-got:
			reached = append(reached, b...)
		case <-ctx.Done():
			t.Fatalf("the VM got %q of the viewer's %q", reached, typed)
		}
	}
	send <- []byte("\x02\x02")
	v.read(2)
	endVMDesktop(t, h, v, send)

	_, pb, kept := storedVMRecording(t, h)
	if strings.Contains(kept, string(vmKeyEvent)) || strings.Contains(kept, pasteSentinel) {
		t.Errorf("the recording carries what the viewer typed: %q", kept)
	}
	if want := vmHandshake() + "\x02\x02"; kept != want || pb.StoppedMS != 0 {
		t.Errorf("recorded frames = %q (stopped at %d ms), want only what the VM sent %q", kept, pb.StoppedMS, want)
	}
}

// TestVMDesktopRecording_FollowsTheRecordingKnob: a VM desktop is recorded only on a screen_actions run
// in an organisation that keeps recordings (WARDYN_RECORDING_STORE), as the noVNC app's is.
func TestVMDesktopRecording_FollowsTheRecordingKnob(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(h *uiHarness)
	}{
		{"terminal level", func(h *uiHarness) { h.run.RecordingLevel = types.RecordingTerminal }},
		{"policy off", func(h *uiHarness) {
			h.run.RecordingLevel, h.run.NotRecorded = types.RecordingOff, types.NotRecordedPolicyOff
		}},
		{"store off", func(h *uiHarness) { h.srv.cfg.RecordingStore = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, dr, send, _ := vmDesktopRun(t)
			tc.set(h)
			h.store.putRun(h.run)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			v := openVMDesktop(t, ctx, h, dr)
			send <- []byte("\x02\x02")
			v.read(2)
			endVMDesktop(t, h, v, send)
			if rows := h.audit.auditRows("desktop.recording.write", "success"); len(rows) != 0 {
				t.Errorf("%s: the VM desktop was recorded: %v", tc.name, rows)
			}
		})
	}
}
