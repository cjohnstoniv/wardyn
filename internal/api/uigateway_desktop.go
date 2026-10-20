// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The desktop of a Linux VM run (machine_kind linux_vm) on the UI gateway: the same ticket → bind →
// enter → cookie hand-off as a UI app (uigateway.go), with three differences.
//
//   - Nothing inside the sandbox is reached. The display is QEMU's RFB server on a unix socket outside
//     the guest, opened through the run's runner (runner.DesktopDialer), so the guest cannot tamper
//     with it and no exec or socat runs in the sandbox.
//   - The page is Wardyn's own viewer, desktop.html from the console bundle (WARDYN_UI_DIR), not
//     sandbox-authored content. Its RFB bytes ride one WebSocket, /r/<run>/__desktop/__rfb.
//   - The session is narrower than a UI app's: at most uiDesktopSessionTTL (1 h) instead of the relay's
//     8 h default, and Close ends it for every copy of the cookie, not just the tab.
//
// The door obeys both limits an app or a terminal obeys: deny_ui_apps (refuseUIAppsDenied, already
// checked by uiEnterCommon) and deny_interactive (refuseInteractiveAttach). A desktop is a person
// driving the machine with a keyboard and a mouse, and it rides the UI gateway, so reading only one of
// the two would let it reach a run that a policy had closed to the other.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// uiDesktopApp is the desktop's app segment. A declared ui_app can never be named it
	// (uiAppNameRE starts with an alphanumeric), so the two never share a cookie or a path.
	uiDesktopApp = "__desktop"
	// uiDesktopSessionTTL bounds one desktop session, whatever WARDYN_UI_SANDBOX_SESSION_TTL allows.
	uiDesktopSessionTTL = time.Hour
	// uiDesktopRecheck is how often an open desktop stream re-checks its person and its run.
	uiDesktopRecheck = 30 * time.Second
	uiDesktopViewer  = "desktop.html"
	uiDesktopRFB     = "/__rfb"
	uiDesktopClose   = "/__close"
	uiDesktopAssets  = "/assets/"

	uiTicketInvalidMsg = "invalid, expired, or already-used attach ticket"
	uiNoSessionMsg     = "no valid UI session for this run — open the app again from its run page"
	uiNoDesktopMsg     = "this run has no desktop: only a Linux desktop VM run has one"
)

// The desktop stream's close codes (RFC 6455 private range): the viewer tells an ended session or
// run, which it must not retry, from a desktop that is not reachable yet, which it retries.
const (
	desktopCloseSessionEnded  websocket.StatusCode = 4001
	desktopCloseRunEnded      websocket.StatusCode = 4002
	desktopCloseNotAuthorized websocket.StatusCode = 4003
	desktopCloseUnreachable   websocket.StatusCode = 4004
)

// uiDesktopSessions is the process-local state of desktop sessions, keyed by uiReassertKey: those
// their person closed (until the cookie would have expired anyway) and the streams still open.
type uiDesktopSessions struct {
	mu     sync.Mutex
	closed map[string]int64
	live   map[string]map[uint64]context.CancelFunc
	next   uint64
}

func (s *Server) uiSessionTTLFor(sess uiSession) time.Duration {
	if sess.Desktop {
		return min(s.uiSessionTTL(), uiDesktopSessionTTL)
	}
	return s.uiSessionTTL()
}

func (s *Server) uiDesktopClosed(sess uiSession, now time.Time) bool {
	d := &s.uiDesktop
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, exp := range d.closed {
		if now.Unix() >= exp {
			delete(d.closed, k)
		}
	}
	_, ok := d.closed[uiReassertKey(sess)]
	return ok
}

// closeDesktop ends sess: its cookie is refused from now on, wherever it was copied, and every
// stream it opened is cut.
func (s *Server) closeDesktop(sess uiSession) {
	d := &s.uiDesktop
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed == nil {
		d.closed = map[string]int64{}
	}
	key := uiReassertKey(sess)
	d.closed[key] = sess.Expires
	for _, cancel := range d.live[key] {
		cancel()
	}
}

// trackDesktop registers an open stream of sess, cancelled by closeDesktop; the returned func forgets it.
func (s *Server) trackDesktop(sess uiSession, cancel context.CancelFunc) func() {
	d := &s.uiDesktop
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.live == nil {
		d.live = map[string]map[uint64]context.CancelFunc{}
	}
	key := uiReassertKey(sess)
	if d.live[key] == nil {
		d.live[key] = map[uint64]context.CancelFunc{}
	}
	d.next++
	id := d.next
	d.live[key][id] = cancel
	return func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		delete(d.live[key], id)
		if len(d.live[key]) == 0 {
			delete(d.live, key)
		}
	}
}

// uiEnterDesktop is uiEnterCommon's arm for the desktop, past the checks every app shares (the
// ticket, the person, background-only, deny_ui_apps, a RUNNING run that is not kept).
func (s *Server) uiEnterDesktop(w http.ResponseWriter, r *http.Request, run types.AgentRun, ta ticketActor, superAdmin bool, redirectStatus int) {
	if !run.MachineKind.IsVM() {
		s.auditUI(&run.ID, types.ActorHuman, ta.principal, "ui.authorize", uiDesktopApp, "denied",
			ta.withVia(map[string]any{"reason": "the run is not a desktop VM"}))
		writeErrorReason(w, http.StatusForbidden, reasonUIGatewayAppNotDeclared, uiNoDesktopMsg)
		return
	}
	if (!superAdmin || !s.adminDoorExempt(run)) && s.refuseInteractiveAttach(w, r.WithContext(withTicketActor(r.Context(), ta)), run) {
		return
	}
	sess := s.newUISession(run, ta, uiDesktopApp, 0, min(s.uiSessionTTL(), uiDesktopSessionTTL))
	sess.Desktop = true
	s.startUISession(w, r, run, ta, sess, map[string]any{"app": uiDesktopApp, "machine_kind": string(run.MachineKind)}, "/", redirectStatus)
}

// handleDesktopRelay serves /r/<run>/__desktop/...: the viewer's own files (no session: they are the
// console bundle's public assets), then, with a desktop session for this run, the viewer page, the
// RFB stream and Close.
func (s *Server) handleDesktopRelay(w http.ResponseWriter, r *http.Request, runID uuid.UUID) {
	rest := strings.TrimPrefix(r.URL.Path, uiRelayPrefix(runID, uiDesktopApp))
	if strings.HasPrefix(rest, uiDesktopAssets) {
		s.serveDesktopAsset(w, r, strings.TrimPrefix(rest, uiDesktopAssets))
		return
	}
	sess, ok := s.decodeUISession(r, s.cfg.Now())
	if !ok || sess.Run != runID || !sess.Desktop {
		if rest == "/" && s.serveDesktopRefusal(w, r, runID, uiNoSessionMsg) {
			return
		}
		writeErrorReason(w, http.StatusForbidden, reasonUIGatewayNoSession, uiNoSessionMsg)
		return
	}
	if de := s.uiReassertRelay(r.Context(), sess); de != nil {
		writeErrorReason(w, de.status, de.reason, de.msg)
		return
	}
	switch {
	case rest == "/" && r.Method == http.MethodGet:
		s.serveDesktopViewer(w, r, runID, map[string]any{
			"state": "open", "expires": sess.Expires,
			"rfb":   s.uiBasePath() + uiRelayPrefix(runID, uiDesktopApp) + uiDesktopRFB,
			"close": s.uiBasePath() + uiRelayPrefix(runID, uiDesktopApp) + uiDesktopClose,
		}, http.StatusOK)
	case rest == uiDesktopRFB:
		s.serveDesktopRFB(w, r, sess)
	case rest == uiDesktopClose && r.Method == http.MethodPost:
		s.closeDesktop(sess)
		http.SetCookie(w, &http.Cookie{Name: uiCookieName, Value: "", Path: s.uiBasePath() + uiCookiePath(runID, uiDesktopApp),
			HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.cfg.OIDCSecureCookies, MaxAge: -1})
		w.WriteHeader(http.StatusNoContent)
	default:
		writeErrorReason(w, http.StatusNotFound, reasonUIGatewayNotFound, "not found on the Wardyn UI gateway (open an app from the run detail page)")
	}
}

// serveDesktopAsset serves one file of the console bundle's assets/ directory, the viewer's script
// and styles. http.Dir refuses a path that climbs out of it.
func (s *Server) serveDesktopAsset(w http.ResponseWriter, r *http.Request, name string) {
	if s.cfg.UIDir == "" || name == "" || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}
	f, err := http.Dir(filepath.Join(s.cfg.UIDir, "assets")).Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// uiDesktopLinkRefused answers a desktop link opened a second time, or too late, with the viewer's
// refusal page and the gateway's words; false (nothing written) for any other app, or with no viewer.
func (s *Server) uiDesktopLinkRefused(w http.ResponseWriter, r *http.Request, runID uuid.UUID, app string) bool {
	return app == uiDesktopApp && s.serveDesktopRefusal(w, r, runID, uiTicketInvalidMsg)
}

// serveDesktopRefusal answers a desktop link that no longer opens anything with the viewer's refusal
// page, carrying the gateway's words; false when there is no viewer to serve (no WARDYN_UI_DIR).
func (s *Server) serveDesktopRefusal(w http.ResponseWriter, r *http.Request, runID uuid.UUID, said string) bool {
	return s.serveDesktopViewer(w, r, runID, map[string]any{"state": "refused", "said": said}, http.StatusForbidden)
}

// serveDesktopViewer writes desktop.html with its assets rooted under the run's desktop path (the
// bundle is built with a relative base, ui/vite.config.ts) and data-wardyn-desktop carrying what the
// viewer needs. The page is Wardyn's own: a strict CSP, and no framing by any other page.
func (s *Server) serveDesktopViewer(w http.ResponseWriter, r *http.Request, runID uuid.UUID, data map[string]any, status int) bool {
	if s.cfg.UIDir == "" {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(s.cfg.UIDir, uiDesktopViewer))
	if err != nil {
		return false
	}
	attr, _ := json.Marshal(data)
	root := s.uiBasePath() + uiRelayPrefix(runID, uiDesktopApp) + "/"
	page := strings.NewReplacer(`="./`, `="`+root, `data-wardyn-desktop=""`, `data-wardyn-desktop="`+html.EscapeString(string(attr))+`"`).Replace(string(raw))
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self' data: blob:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, page)
	return true
}

// serveDesktopRFB carries the VM's display between the viewer's WebSocket and the runner, re-checking
// the person and the run as uiDial does, and ending at the session's bound, at Close, when the run
// stops, or when the person may no longer enter it.
func (s *Server) serveDesktopRFB(w http.ResponseWriter, r *http.Request, sess uiSession) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"binary"}})
	if err != nil {
		return // Accept answered the request
	}
	c := desktopSocket{ws}
	// ctx ends the stream at the bound and at Close; the socket itself lives on the request, so the
	// stream can tell the viewer why before it goes.
	ctx, cancel := context.WithDeadline(r.Context(), time.Unix(sess.Expires, 0))
	defer cancel()
	untrack := s.trackDesktop(sess, cancel)
	defer untrack()

	run, code, why := s.desktopRunCheck(ctx, sess)
	if code != 0 {
		_ = c.Close(code, why)
		return
	}
	dd, ok := s.cfg.Runner.(runner.DesktopDialer)
	if !ok {
		_ = c.Close(desktopCloseUnreachable, "no runner configured; UI apps unavailable")
		return
	}
	if err := s.thawForExec(ctx, run, types.ActorHuman, sess.Principal, "presence"); err != nil {
		_ = c.Close(desktopCloseUnreachable, "run is paused and could not be resumed; try again")
		return
	}
	release, ok := s.acquireUIConn(sess.Run)
	if !ok {
		_ = c.Close(desktopCloseUnreachable, uiConnCapMsg)
		return
	}
	defer release()
	display, err := dd.DialDesktop(ctx, run.SandboxRef)
	if err != nil {
		_ = c.Close(desktopCloseUnreachable, uiExecFailMsg(ctx, "open the desktop", err))
		return
	}
	defer func() { _ = display.Close() }()

	go s.attachKeepalive(ctx, sess.Run)
	opened := s.cfg.Now()
	s.auditUISession(sess, "ui.open", uiDesktopApp, "success", map[string]any{"app": uiDesktopApp})
	defer func() {
		s.auditUISession(sess, "ui.close", uiDesktopApp, "success",
			map[string]any{"app": uiDesktopApp, "duration_sec": int(s.cfg.Now().Sub(opened).Seconds())})
	}()

	conn := websocket.NetConn(r.Context(), ws, websocket.MessageBinary)
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(display, conn); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, display); done <- struct{}{} }()
	tick := time.NewTicker(uiDesktopRecheck)
	defer tick.Stop()
	for {
		select {
		case <-done:
			_ = c.Close(desktopCloseUnreachable, "the desktop closed the connection")
			return
		case <-ctx.Done():
			// The bound, Close, or the browser leaving: only the first two are told.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) || s.uiDesktopClosed(sess, s.cfg.Now()) {
				_ = c.Close(desktopCloseSessionEnded, "this desktop session ended")
			}
			return
		case <-tick.C:
			if _, code, why := s.desktopRunCheck(ctx, sess); code != 0 {
				_ = c.Close(code, why)
				return
			}
		}
	}
}

// desktopSocket closes with words cut to what a close frame carries (123 bytes).
type desktopSocket struct{ *websocket.Conn }

func (d desktopSocket) Close(code websocket.StatusCode, why string) error {
	if len(why) > 123 {
		why = strings.ToValidUTF8(why[:123], "")
	}
	return d.Conn.Close(code, why)
}

// desktopRunCheck loads the session's run fresh and answers the close code and words that end its
// stream, or 0 while it may go on.
func (s *Server) desktopRunCheck(ctx context.Context, sess uiSession) (types.AgentRun, websocket.StatusCode, string) {
	run, err := s.cfg.Store.GetRun(ctx, sess.Run)
	if err != nil {
		return run, desktopCloseUnreachable, "run not found"
	}
	if run.State != types.RunRunning || run.SandboxRef == "" || runIsKept(run) {
		return run, desktopCloseRunEnded, "run has ended; cannot open a UI app"
	}
	if de := s.uiSessionStillAuthorized(ctx, sess, run); de != nil {
		return run, desktopCloseNotAuthorized, de.msg
	}
	return run, 0, ""
}
