// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

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

// macOSDesktopRun is vmDesktopRun on a macOS VM run at level.
func macOSDesktopRun(t *testing.T, level types.RecordingLevel) (h *uiHarness, dr *desktopFakeRunner, send chan []byte) {
	t.Helper()
	h, dr, send, _ = vmDesktopRun(t)
	h.run.Agent, h.run.MachineKind, h.run.RecordingLevel = "claude-code", types.MachineMacOSVM, level
	h.store.putRun(h.run)
	return h, dr, send
}

// TestMacOSVMDesktopRecording_YieldsACastWithThePauseGap: a macOS VM's desktop is recorded at Screen and
// actions through the recorder a Linux VM's uses: one desktop- cast under the run's cast key, a pause
// keeping none of what the display sends while it is open, and the same pause and resume rows.
func TestMacOSVMDesktopRecording_YieldsACastWithThePauseGap(t *testing.T) {
	h, dr, send := macOSDesktopRun(t, types.RecordingScreenActions)
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
		write["key"] != recording.CastKey(h.run.ID.String(), session) || pb.Format != "novnc-playback" {
		t.Errorf("write row = %v, playback %s: want a desktop cast under the run's cast key", write, pb.Format)
	}
	if want := vmHandshake() + "\x02\x02"; kept != want || pb.StoppedMS != 0 {
		t.Errorf("recorded frames = %q (stopped at %d ms), want the handshake and the Bells after the resume %q", kept, pb.StoppedMS, want)
	}
	if len(pb.Gaps) != 1 || pb.Gaps[0].Reason != desktopPauseSignin || pb.Gaps[0].EndMS == 0 {
		t.Errorf("playback gaps = %+v, want one closed %q gap", pb.Gaps, desktopPauseSignin)
	}
	if pause, resume := h.audit.auditRows("desktop.recording.pause", "success"), h.audit.auditRows("desktop.recording.resume", "success"); len(pause) != 1 || len(resume) != 1 {
		t.Errorf("pause rows %d, resume rows %d, want 1 each", len(pause), len(resume))
	}
}

// TestMacOSVMDesktopRecording_FollowsTheLevel: a macOS VM's desktop is kept at Screen and actions only,
// and not on a run that records nothing, as for every machine kind.
func TestMacOSVMDesktopRecording_FollowsTheLevel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		level types.RecordingLevel
		note  types.RunNotRecorded
		want  int
	}{
		{"screen and actions", types.RecordingScreenActions, "", 1},
		{"screen and actions, runner declined", types.RecordingScreenActions, types.NotRecordedRunnerDeclined, 1},
		{"terminal", types.RecordingTerminal, "", 0},
		{"policy off", types.RecordingOff, types.NotRecordedPolicyOff, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, dr, send := macOSDesktopRun(t, tc.level)
			h.run.NotRecorded = tc.note
			h.store.putRun(h.run)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			v := openVMDesktop(t, ctx, h, dr)
			send <- []byte("\x02\x02")
			v.read(2)
			endVMDesktop(t, h, v, send)
			if tc.want == 1 {
				storedVMRecording(t, h)
			}
			if rows := h.audit.auditRows("desktop.recording.write", "success"); len(rows) != tc.want {
				t.Errorf("desktop.recording.write rows = %d, want %d", len(rows), tc.want)
			}
		})
	}
}

// rfbPixelFormat8 is a SetPixelFormat of 8 bits per pixel, a format a recorded desktop's recorder cannot frame;
// rfbPixelsAsked is a FramebufferUpdateRequest, which asks for pixels in the format in force: with no
// SetPixelFormat, ServerInit's (rfbServerInit: red, green and blue at 16, 8 and 0, not the playback order).
var (
	rfbPixelFormat8 = []byte{0, 0, 0, 0, 8, 8, 0, 1, 0, 7, 0, 7, 0, 3, 5, 2, 0, 0, 0, 0}
	rfbPixelsAsked  = []byte{3, 0, 0, 0, 0, 0, 5, 0, 3, 32}
)

// refusedCase is a viewer's message on a desktop, binary unless text, and, when the desktop is recorded and the
// relay refuses the message, the reason the refusal's stop mark gives: the refused format, or what else the relay
// refused.
type refusedCase struct {
	recorded bool
	msg      []byte
	reason   string
	text     bool
}

var refusedCases = map[string]refusedCase{
	"recorded, SetPixelFormat": {true, rfbPixelFormat8, desktopStopPixels +
		"8 bits per pixel, depth 8, little-endian true colour, maxima of 7, 7 and 3 and shifts of 5, 2 and 0 for red, green and blue", false},
	"recorded, ServerInit": {true, rfbPixelsAsked, desktopStopPixels +
		"32 bits per pixel, depth 24, little-endian true colour, maxima of 255, 255 and 255 and shifts of 16, 8 and 0 for red, green and blue", false},
	"recorded, a message it cannot follow": {true, []byte{7}, "Wardyn refused a message from the browser that it cannot follow", false},
	"recorded, a text message":             {true, []byte("text"), "Wardyn refused a WebSocket frame from the browser that it cannot read", true},
	"unrecorded":                           {false, rfbPixelFormat8, "", false},
}

// stoppedWhereRefused checks a recording its relay ended at a refusal: the write row carries stopped_ms, as
// every early stop's does, and the recording's last record is the stop mark, with reason.
func stoppedWhereRefused(t *testing.T, write map[string]any, pb recording.RFBPlayback, reason string) {
	t.Helper()
	if ms, _ := write["stopped_ms"].(float64); ms <= 0 || int64(ms) != pb.StoppedMS {
		t.Fatalf("write row stopped_ms = %v, recording stopped at %d ms: want the same stop mark in both", write["stopped_ms"], pb.StoppedMS)
	}
	if pb.StopReason != reason {
		t.Fatalf("stop reason = %q, want %q", pb.StopReason, reason)
	}
	for _, f := range pb.Frames {
		ms, _, _ := strings.Cut(strings.TrimPrefix(f, "{"), "{")
		if at, err := strconv.ParseInt(ms, 10, 64); err != nil || at > pb.StoppedMS {
			t.Fatalf("frame %q comes after the stop mark at %d ms", f, pb.StoppedMS)
		}
	}
}

// TestVMDesktopRecording_ARefusalEndsWithAStopMark: on a recorded VM desktop the relay's filter is
// told the display is recorded, so a SetPixelFormat the recorder cannot frame, or a request for pixels in
// the display's own format when that is not the replay's, closes the viewer's socket and never reaches the
// display, and the stored recording ends with a stop mark naming the format; so does a message the relay
// cannot follow at all, or a text message, which the door hands the filter rather than refusing it itself, with
// what it refused. Unrecorded, the same SetPixelFormat reaches the display.
func TestVMDesktopRecording_ARefusalEndsWithAStopMark(t *testing.T) {
	for name, tc := range refusedCases {
		t.Run(name, func(t *testing.T) {
			h, dr, send, got := vmDesktopRun(t)
			if !tc.recorded {
				h.run.RecordingLevel = ""
				h.store.putRun(h.run)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			v := openVMDesktop(t, ctx, h, dr)
			if tc.text {
				if err := v.ws.Write(ctx, websocket.MessageText, tc.msg); err != nil {
					t.Fatal(err)
				}
			} else {
				v.write(tc.msg)
			}
			if !tc.recorded {
				var reached []byte
				for len(reached) < len(tc.msg) {
					select {
					case b := <-got:
						reached = append(reached, b...)
					case <-ctx.Done():
						t.Fatalf("the VM got %x of the viewer's %x", reached, tc.msg)
					}
				}
				if !bytes.Equal(reached, tc.msg) {
					t.Fatalf("the VM got %x, want the viewer's %x", reached, tc.msg)
				}
				endVMDesktop(t, h, v, send)
				return
			}
			rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
			_, _, err := v.ws.Read(rctx)
			open := rctx.Err() != nil
			rcancel()
			if err == nil || open {
				t.Fatalf("the viewer's socket stayed open after %x on a recorded desktop: %v", tc.msg, err)
			}
			endVMDesktop(t, h, v, send)
			select {
			case b := <-got:
				t.Fatalf("the VM read %x: the refused message reached it", b)
			default:
			}
			write, pb, _ := storedVMRecording(t, h)
			stoppedWhereRefused(t, write, pb, tc.reason)
		})
	}
}

// TestDesktopAppRecording_ARefusalEndsWithAStopMark is the same for a container's noVNC app, whose
// filter goes on in uiFilterUpgrade: recorded, each message closes the browser's socket, never reaches the
// desktop and ends the recording with its stop mark; unrecorded, the SetPixelFormat reaches the desktop.
func TestDesktopAppRecording_ARefusalEndsWithAStopMark(t *testing.T) {
	for name, tc := range refusedCases {
		t.Run(name, func(t *testing.T) {
			offered, pasted := make(chan string, 1), make(chan []byte, 1)
			h := desktopHarness(t, rfbDesktopBackend(t, offered, pasted, false), nil)
			if tc.recorded {
				h.run.RecordingLevel = types.RecordingScreenActions
				h.store.putRun(h.run)
				h.srv.cfg.RecordingStore = fsRecordingStore(t)
			}
			ds := openDesktop(t, h, desktopApp)
			<-offered
			if next, err := readWSFrame(ds.br); err != nil || !bytes.Equal(next, []byte{2}) {
				t.Fatalf("after ServerInit the browser got %q (err %v), want the Bell", next, err)
			}
			f := wsFrame(tc.msg, true)
			if tc.text {
				f[0] = 0x81
			}
			if _, err := ds.conn.Write(f); err != nil {
				t.Fatalf("%x write: %v", tc.msg, err)
			}
			if !tc.recorded {
				select {
				case b := <-pasted:
					if !bytes.Equal(b, tc.msg) {
						t.Fatalf("the desktop got %x, want the browser's %x", b, tc.msg)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("an unrecorded desktop's %x never reached it", tc.msg)
				}
				return
			}
			var ne net.Error
			if _, err := readWSFrame(ds.br); err == nil || errors.As(err, &ne) && ne.Timeout() {
				t.Fatalf("the browser's socket stayed open after %x on a recorded desktop: %v", tc.msg, err)
			}
			write, pb, _ := storedVMRecording(t, h) // the relay closed and stored its recording
			select {
			case b := <-pasted:
				t.Fatalf("the desktop read %x: the refused message reached it", b)
			default:
			}
			stoppedWhereRefused(t, write, pb, tc.reason)
		})
	}
}
