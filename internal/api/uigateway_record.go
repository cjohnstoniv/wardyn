// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// desktopApp is the UI app whose relayed stream is a desktop's RFB (deploy/images/novnc).
const desktopApp = "novnc"

// desktopRecordingSuffix starts a desktop recording's cast key suffix; the console tells a screen
// recording from a terminal cast by its desktop.recording.write row, never by the bytes.
const desktopRecordingSuffix = "desktop-"

// desktopRecordings holds every live desktop recording, by run, and each run's open pause, so a
// connection that opens during a pause starts paused. Process-local, like the relay's own bounds.
type desktopRecordings struct {
	mu    sync.Mutex
	byRun map[uuid.UUID]*runDesktopRecording
}

type runDesktopRecording struct {
	recs   map[*recording.RFBRecorder]struct{}
	paused *desktopPause
}

type desktopPause struct {
	at     time.Time
	reason string
}

// recordDesktopConn records conn's sandbox side when the run records screen_actions, the app is the
// desktop and the organisation keeps recordings; any other connection is returned as it is. The
// browser's side is never read (recording.ReadSideConn), so keystrokes are never recorded.
func (s *Server) recordDesktopConn(run types.AgentRun, sess uiSession, conn net.Conn) net.Conn {
	if sess.App != desktopApp || run.RecordingLevel != types.RecordingScreenActions || run.NotRecorded != "" ||
		s.cfg.RecordingStore == nil || runIsUnrecordable(run) {
		return conn
	}
	rec := recording.NewRFBRecorder(s.cfg.Now)
	session := desktopRecordingSuffix + uuid.NewString()
	s.desktopRecs.add(run.ID, rec)
	return recording.ReadSideConn(conn, rec, func() {
		s.desktopRecs.remove(run.ID, rec)
		s.saveDesktopRecording(s.cfg.BaseCtx, run, sess, session, rec)
	})
}

// saveDesktopRecording stores a closed connection's recording and writes desktop.recording.write.
// A connection that carried no RFB frames (a static file, or one that never upgraded) stores nothing.
func (s *Server) saveDesktopRecording(ctx context.Context, run types.AgentRun, sess uiSession, session string, rec *recording.RFBRecorder) {
	doc, had := rec.Playback()
	if !had {
		return
	}
	key := recording.CastKey(run.ID.String(), session)
	saveCtx, cancel := context.WithTimeout(ctx, sessionRecordingSaveWait)
	err := s.cfg.RecordingStore.SaveCastNamed(saveCtx, run.ID.String(), session, bytes.NewReader(doc))
	cancel()
	outcome, data := "success", map[string]any{"session": session, "key": key, "bytes": len(doc), "format": "novnc-playback"}
	if err != nil {
		outcome, data["error"] = "failure", err.Error()
	}
	s.auditUISession(sess, "desktop.recording.write", key, outcome, data)
}

func (d *desktopRecordings) add(run uuid.UUID, rec *recording.RFBRecorder) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.byRun == nil {
		d.byRun = map[uuid.UUID]*runDesktopRecording{}
	}
	r := d.byRun[run]
	if r == nil {
		r = &runDesktopRecording{recs: map[*recording.RFBRecorder]struct{}{}}
		d.byRun[run] = r
	}
	r.recs[rec] = struct{}{}
	if r.paused != nil {
		rec.Pause(r.paused.reason)
	}
}

func (d *desktopRecordings) remove(run uuid.UUID, rec *recording.RFBRecorder) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r := d.byRun[run]; r != nil {
		delete(r.recs, rec)
		if len(r.recs) == 0 && r.paused == nil {
			delete(d.byRun, run)
		}
	}
}

// PauseDesktopRecording stops keeping the run's screen frames (C11): the action log, the clipboard
// rules and the run itself carry on. The pause is written to the tamper-evident audit log as
// desktop.recording.pause, by Wardyn, never by the desktop. It reports false when already paused.
// Its caller is the "Needs you" takeover.
func (s *Server) PauseDesktopRecording(ctx context.Context, run types.AgentRun, reason string) bool {
	d := &s.desktopRecs
	d.mu.Lock()
	if d.byRun == nil {
		d.byRun = map[uuid.UUID]*runDesktopRecording{}
	}
	r := d.byRun[run.ID]
	if r == nil {
		r = &runDesktopRecording{recs: map[*recording.RFBRecorder]struct{}{}}
		d.byRun[run.ID] = r
	}
	if r.paused != nil {
		d.mu.Unlock()
		return false
	}
	at := s.cfg.Now().UTC()
	r.paused = &desktopPause{at: at, reason: reason}
	for rec := range r.recs {
		rec.Pause(reason)
	}
	d.mu.Unlock()
	s.recordStreamAudit(ctx, run.SandboxRef, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "desktop.recording.pause",
		run.ID.String(), "success", mustJSON(map[string]any{"reason": reason, "gap_start": at})))
	return true
}

// ResumeDesktopRecording keeps frames again and writes desktop.recording.resume with the whole gap:
// its start, end, length and reason. It reports false when the run was not paused.
func (s *Server) ResumeDesktopRecording(ctx context.Context, run types.AgentRun) bool {
	d := &s.desktopRecs
	d.mu.Lock()
	r := d.byRun[run.ID]
	if r == nil || r.paused == nil {
		d.mu.Unlock()
		return false
	}
	p := *r.paused
	r.paused = nil
	for rec := range r.recs {
		rec.Resume()
	}
	if len(r.recs) == 0 {
		delete(d.byRun, run.ID)
	}
	d.mu.Unlock()
	end := s.cfg.Now().UTC()
	s.recordStreamAudit(ctx, run.SandboxRef, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "desktop.recording.resume",
		run.ID.String(), "success", mustJSON(map[string]any{"reason": p.reason, "gap_start": p.at, "gap_end": end,
			"gap_ms": end.Sub(p.at).Milliseconds()})))
	return true
}
