// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func desktopFrame(payload string) []byte {
	return append([]byte{0x82, byte(len(payload))}, payload...)
}

// TestDesktopRecording_PauseGapRow: a screen_actions run's desktop connection is recorded from the
// sandbox side; a pause keeps no frames and writes the gap to the audit log, pause and resume rows
// with its start, end, length and reason; the close stores the playback with that gap.
func TestDesktopRecording_PauseGapRow(t *testing.T) {
	audit := &recRecorder{}
	store := fsRecordingStore(t)
	now := time.Date(2026, 10, 20, 10, 42, 18, 0, time.UTC)
	srv := New(Config{Audit: audit, AdminToken: adminToken, RecordingStore: store})
	srv.cfg.Now = func() time.Time { now = now.Add(time.Second); return now }
	run := types.AgentRun{ID: uuid.New(), RecordingLevel: types.RecordingScreenActions}
	sess := uiSession{Run: run.ID, App: desktopApp, Principal: "alice"}

	sandbox, relay := net.Pipe()
	conn := srv.recordDesktopConn(run, sess, relay)
	if conn == relay {
		t.Fatal("a screen_actions desktop connection was not recorded")
	}
	sent := make(chan []byte, 4)
	go func() {
		for b := range sent {
			_, _ = sandbox.Write(b)
		}
		_ = sandbox.Close()
	}()
	read := func(n int) {
		buf := make([]byte, n)
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Fatal(err)
		}
	}
	up := "HTTP/1.1 101 Switching Protocols\r\n\r\n"
	sent <- []byte(up)
	read(len(up))
	sent <- desktopFrame("before")
	read(8)
	if !srv.PauseDesktopRecording(context.Background(), run, "you took over to sign in") {
		t.Fatal("pause refused")
	}
	sent <- desktopFrame("during")
	read(8)
	if !srv.ResumeDesktopRecording(context.Background(), run) {
		t.Fatal("resume refused")
	}
	sent <- desktopFrame("after")
	read(7)
	close(sent)
	_ = conn.Close()

	var pause, resume, write map[string]any
	for _, ev := range audit.snapshot() {
		switch ev.Action {
		case "desktop.recording.pause":
			pause = auditData(t, ev)
		case "desktop.recording.resume":
			resume = auditData(t, ev)
		case "desktop.recording.write":
			write = auditData(t, ev)
		}
	}
	if pause == nil || pause["reason"] != "you took over to sign in" || pause["gap_start"] == nil {
		t.Fatalf("pause row = %v", pause)
	}
	if resume == nil || resume["gap_start"] != pause["gap_start"] || resume["gap_end"] == nil || resume["gap_ms"] == nil ||
		resume["reason"] != "you took over to sign in" {
		t.Fatalf("resume row = %v", resume)
	}
	if write == nil || write["format"] != "novnc-playback" {
		t.Fatalf("write row = %v", write)
	}
	rc, err := store.OpenCast(context.Background(), write["key"].(string))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	var pb recording.RFBPlayback
	if err := json.NewDecoder(rc).Decode(&pb); err != nil {
		t.Fatal(err)
	}
	if len(pb.Frames) != 2 || len(pb.Gaps) != 1 || pb.Gaps[0].Reason != "you took over to sign in" || pb.Gaps[0].EndMS == 0 {
		t.Fatalf("stored playback = %+v", pb)
	}
}

// TestDesktopRecording_OnlyScreenActionsDesktops: every other connection is relayed untouched,
// including a desktop at terminal, one whose organisation keeps no recordings, and another app.
func TestDesktopRecording_OnlyScreenActionsDesktops(t *testing.T) {
	_, relay := net.Pipe()
	screen := types.AgentRun{ID: uuid.New(), RecordingLevel: types.RecordingScreenActions}
	on := New(Config{Audit: &recRecorder{}, AdminToken: adminToken, RecordingStore: fsRecordingStore(t)})
	off := New(Config{Audit: &recRecorder{}, AdminToken: adminToken})
	for _, tc := range []struct {
		name string
		srv  *Server
		run  types.AgentRun
		app  string
	}{
		{"terminal level", on, types.AgentRun{ID: uuid.New()}, desktopApp},
		{"policy off", on, types.AgentRun{ID: uuid.New(), RecordingLevel: types.RecordingOff, NotRecorded: types.NotRecordedPolicyOff}, desktopApp},
		{"store off", off, screen, desktopApp},
		{"another app", on, screen, "code-server"},
	} {
		if got := tc.srv.recordDesktopConn(tc.run, uiSession{Run: tc.run.ID, App: tc.app}, relay); got != relay {
			t.Errorf("%s: the connection was wrapped", tc.name)
		}
	}
}
