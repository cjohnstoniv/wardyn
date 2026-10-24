// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/livebus"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/store"
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

// desktopPause is one gap: it opens with the first hold on the run and closes when the last one ends,
// so take overs and a sign-in that overlap keep one gap, named by the reason that opened it.
type desktopPause struct {
	at      time.Time
	reason  string
	holders []string // the id of every take over or sign-in still holding the gap open, the opener's included
}

// recordDesktopConn records conn's sandbox side when the run records screen_actions, the app is the
// desktop and the organisation keeps recordings; any other connection is returned as it is. The
// browser's side is never read (recording.ReadSideConn), so keystrokes are never recorded. A run its
// runner declined to record is still recorded here: this relay is Wardyn's, as for a console session.
func (s *Server) recordDesktopConn(run types.AgentRun, sess uiSession, conn net.Conn) net.Conn {
	if sess.App != desktopApp || !s.desktopRecorded(run) {
		return conn
	}
	rec := recording.NewRFBRecorder(s.cfg.Now)
	return recording.ReadSideConn(conn, rec, s.trackDesktopRecording(run, sess, rec))
}

// recordVMDisplay is recordDesktopConn on a Linux VM's display, framed as the clipboard filter reads it
// (wsFramed): the same rules, pauses and stored recording, with the viewer's side never read.
func (s *Server) recordVMDisplay(run types.AgentRun, sess uiSession, display io.ReadWriteCloser) io.ReadWriteCloser {
	if !s.desktopRecorded(run) {
		return display
	}
	rec := recording.NewRFBFrameRecorder(s.cfg.Now)
	return recording.ReadSide(display, rec, s.trackDesktopRecording(run, sess, rec))
}

// trackDesktopRecording is the hook both desktop relays share: rec joins the run's live recordings,
// so a pause open now or later reaches it, and the returned func, run once when its connection
// closes, stores it (desktop.recording.write) and lets the run's entry go.
func (s *Server) trackDesktopRecording(run types.AgentRun, sess uiSession, rec *recording.RFBRecorder) func() {
	session := desktopRecordingSuffix + uuid.NewString()
	s.desktopRecs.add(run.ID, rec)
	return func() {
		s.desktopRecs.remove(run.ID, rec)
		s.saveDesktopRecording(s.cfg.BaseCtx, run, sess, session, rec)
		s.forgetEndedDesktopPause(s.cfg.BaseCtx, run.ID)
	}
}

// desktopRecorded reports whether run's desktop screen is recorded: the run records screen_actions and
// the organisation keeps recordings.
func (s *Server) desktopRecorded(run types.AgentRun) bool {
	return run.RecordingLevel == types.RecordingScreenActions && run.NotRecorded != types.NotRecordedPolicyOff &&
		s.cfg.RecordingStore != nil && !runIsUnrecordable(run)
}

// Why Wardyn paused a desktop recording, as the gap names it: a "Needs you" takeover, or a desktop
// sign-in (board M7-Replay's words).
const (
	desktopPauseTakeover = "you took over"
	desktopPauseSignin   = "you took over to sign in"
)

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
	outcome, data := "success", map[string]any{"session": session, "key": key, "bytes": len(doc), "format": "novnc-playback",
		"clipboard_omitted": rec.ClipboardOmitted()}
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

// forgetEndedDesktopPause drops a paused run's entry once its last connection closed and the run has
// ended (or is gone), so a pause nothing will resume does not stay for the life of the process, and
// writes the gap's resume row: every pause row has one. A live run keeps it: a connection that opens
// during the pause must start paused.
func (s *Server) forgetEndedDesktopPause(ctx context.Context, runID uuid.UUID) {
	if s.cfg.Store == nil {
		return
	}
	cur, err := s.cfg.Store.GetRun(ctx, runID)
	if !errors.Is(err, store.ErrNotFound) && (err != nil || !cur.State.IsTerminal()) {
		return
	}
	d := &s.desktopRecs
	d.mu.Lock()
	r := d.byRun[runID]
	if r == nil || len(r.recs) > 0 {
		d.mu.Unlock()
		return
	}
	delete(d.byRun, runID)
	d.mu.Unlock()
	if r.paused != nil {
		s.auditDesktopResume(ctx, runID, cur.SandboxRef, *r.paused)
	}
}

// PauseDesktopRecording stops keeping the run's screen frames (C11): the action log, the clipboard
// rules and the run itself carry on. The pause is written to the tamper-evident audit log as
// desktop.recording.pause, by Wardyn, never by the desktop. A pause while one is open joins it: no
// new gap or row, and the gap stays open until every reason holding it has resumed. It reports
// whether it opened the gap. Its callers are the "Needs you" takeover and a desktop sign-in, on a run
// whose screen is recorded; hold is that take over's or sign-in's id, which its end resumes.
func (s *Server) PauseDesktopRecording(ctx context.Context, run types.AgentRun, reason, hold string) bool {
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
		if !slices.Contains(r.paused.holders, hold) {
			r.paused.holders = append(r.paused.holders, hold)
		}
		d.mu.Unlock()
		return false
	}
	at := s.cfg.Now().UTC()
	r.paused = &desktopPause{at: at, reason: reason, holders: []string{hold}}
	for rec := range r.recs {
		rec.Pause(reason)
	}
	d.mu.Unlock()
	s.recordStreamAudit(ctx, run.SandboxRef, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "desktop.recording.pause",
		run.ID.String(), "success", mustJSON(map[string]any{"reason": reason, "gap_start": at})))
	return true
}

// ResumeDesktopRecording ends hold's hold on the gap. When it was the last one, frames are kept
// again and desktop.recording.resume is written with the whole gap: its start, end, length and the
// reason that opened it. It reports whether the recording resumed, so one pause's end never ends
// another's.
func (s *Server) ResumeDesktopRecording(ctx context.Context, run types.AgentRun, hold string) bool {
	d := &s.desktopRecs
	d.mu.Lock()
	r := d.byRun[run.ID]
	if r == nil || r.paused == nil {
		d.mu.Unlock()
		return false
	}
	r.paused.holders = slices.DeleteFunc(r.paused.holders, func(h string) bool { return h == hold })
	if len(r.paused.holders) > 0 {
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
	s.auditDesktopResume(ctx, run.ID, run.SandboxRef, p)
	return true
}

// endDesktopHold ends a desktop sign-in's hold here and, on a screen-recorded run, over the live bus on
// the replica that holds it: the leader's sweep may close a sign-in another replica began. A take
// over's hand back tells the other replicas in its own recording_gap notice (recordingGap).
func (s *Server) endDesktopHold(ctx context.Context, run types.AgentRun, hold string) {
	if !s.ResumeDesktopRecording(ctx, run, hold) && s.desktopRecorded(run) {
		s.publishNotice(livebus.KindRecordingGap, run.ID, recordingGapNotice{Reason: desktopSigninGapReason, DesktopHold: hold})
	}
}

// desktopSigninGapReason is the reason a sign-in's recording_gap notice names.
const desktopSigninGapReason = "desktop_signin"

// onRecordingGapNotice ends another replica's resumed hold on this replica's desktop recording, when
// this replica holds it.
func (s *Server) onRecordingGapNotice(m livebus.Message) {
	var n recordingGapNotice
	if m.Origin == s.replicaName() || unmarshalNotice(m, &n) != nil || n.Paused || n.DesktopHold == "" || !s.desktopRecs.holds(m.Run, n.DesktopHold) {
		return
	}
	run := types.AgentRun{ID: m.Run}
	if s.cfg.Store != nil {
		if cur, err := s.cfg.Store.GetRun(s.cfg.BaseCtx, m.Run); err == nil {
			run = cur
		}
	}
	s.ResumeDesktopRecording(s.cfg.BaseCtx, run, n.DesktopHold)
}

// holds reports whether hold holds run's desktop recording paused here.
func (d *desktopRecordings) holds(run uuid.UUID, hold string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.byRun[run]
	return r != nil && r.paused != nil && slices.Contains(r.paused.holders, hold)
}

// heldWith lists the holds on run's open gap here whose id starts with prefix.
func (d *desktopRecordings) heldWith(run uuid.UUID, prefix string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.byRun[run]
	if r == nil || r.paused == nil {
		return nil
	}
	var out []string
	for _, h := range r.paused.holders {
		if strings.HasPrefix(h, prefix) {
			out = append(out, h)
		}
	}
	return out
}

// auditDesktopResume writes desktop.recording.resume for gap p, ending now.
func (s *Server) auditDesktopResume(ctx context.Context, runID uuid.UUID, sandboxRef string, p desktopPause) {
	end := s.cfg.Now().UTC()
	s.recordStreamAudit(ctx, sandboxRef, s.auditEvent(&runID, types.ActorSystem, "wardynd", "desktop.recording.resume",
		runID.String(), "success", mustJSON(map[string]any{"reason": p.reason, "gap_start": p.at, "gap_end": end,
			"gap_ms": end.Sub(p.at).Milliseconds()})))
}
