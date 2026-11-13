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
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/rfbfilter"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// uiDesktopAppName is the desktop's app segment. A declared ui_app can never be named it
	// (uiAppNameRE starts with an alphanumeric), so the two never share a cookie or a path.
	uiDesktopAppName = "__desktop"
	// uiDesktopSessionTTL bounds one desktop session, whatever WARDYN_UI_SANDBOX_SESSION_TTL allows.
	uiDesktopSessionTTL = time.Hour
	// uiDesktopRecheck is how often an open desktop stream re-checks its person and its run.
	uiDesktopRecheck = 30 * time.Second
	// uiDesktopRecheckWait bounds one re-check's reads, so a store that hangs can't leave a stream relaying
	// unchecked up to its session's bound while the check waits.
	uiDesktopRecheckWait = 10 * time.Second
	uiDesktopViewer      = "desktop.html"
	uiDesktopRFB         = "/__rfb"
	uiDesktopClose       = "/__close"
	uiDesktopAssets      = "/assets/"

	// uiDesktopClosedRecheck is how often an open desktop stream re-reads the Closed set, so a Close
	// on another replica ends it within this bound.
	uiDesktopClosedRecheck = 15 * time.Second

	uiTicketInvalidMsg = "Invalid, expired, or already-used attach ticket"
	uiNoSessionMsg     = "No valid UI session for this run — open the app again from its run page"
	uiNoDesktopMsg     = "This run has no desktop: only a desktop VM run has one"
)

// The desktop stream's close codes (RFC 6455 private range): the viewer tells an ended session or
// run, which it must not retry, from a desktop that is not reachable yet, which it retries.
const (
	desktopCloseSessionEnded  websocket.StatusCode = 4001
	desktopCloseRunEnded      websocket.StatusCode = 4002
	desktopCloseNotAuthorized websocket.StatusCode = 4003
	desktopCloseUnreachable   websocket.StatusCode = 4004
	// desktopCloseWatchEnded: a re-check ended a background run's watch. It says so, never the hour's words.
	desktopCloseWatchEnded websocket.StatusCode = 4005
	// desktopCloseWindowOpen: a window is open on the Mac, so a stream with control ends (one keyboard at a
	// time). The viewer says so and sends the person to the run page, never retrying.
	desktopCloseWindowOpen websocket.StatusCode = 4006
)

// desktopWindowOpenMsg is desktopCloseWindowOpen's reason.
const desktopWindowOpenMsg = "A window is open on the Mac, so this desktop only watches. Open the desktop again from the run page to watch it."

// uiDesktopSessions is the process-local state of desktop sessions, keyed by uiReassertKey: a cache
// of those known closed (until the cookie would have expired anyway) and the streams still open.
type uiDesktopSessions struct {
	mu     sync.Mutex
	closed map[string]int64
	live   map[string]map[uint64]context.CancelFunc
	next   uint64
	// recheckEvery replaces uiDesktopRecheck: tests only, so a re-check can be observed without its wait.
	recheckEvery time.Duration
	// recheckWait replaces uiDesktopRecheckWait: tests only, the same.
	recheckWait time.Duration
}

func (s *Server) uiSessionTTLFor(sess uiSession) time.Duration {
	if sess.Desktop {
		return min(s.uiSessionTTL(), uiDesktopSessionTTL)
	}
	return s.uiSessionTTL()
}

// uiDesktopClosedKey is the Closed set's key for sess in the store: a digest, never principal text.
func uiDesktopClosedKey(sess uiSession) string {
	sum := sha256.Sum256([]byte(uiReassertKey(sess)))
	return hex.EncodeToString(sum[:])
}

// uiDesktopClosedHere reports whether this process knows sess closed (its cache), dropping expired entries.
func (s *Server) uiDesktopClosedHere(sess uiSession, now time.Time) bool {
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

// uiDesktopClosed reports whether sess was closed on any replica. Only a closed answer is cached: an
// open one is read from the store every time, and a store that cannot answer counts as closed.
func (s *Server) uiDesktopClosed(ctx context.Context, sess uiSession, now time.Time) bool {
	closed, unread := s.uiDesktopClosedRead(ctx, sess, now)
	return closed || unread
}

// uiDesktopClosedRead is uiDesktopClosed with its two answers apart: sess was closed, or the store couldn't say.
func (s *Server) uiDesktopClosedRead(ctx context.Context, sess uiSession, now time.Time) (closed, unread bool) {
	if s.uiDesktopClosedHere(sess, now) {
		return true, false
	}
	st, ok := s.cfg.Store.(store.UIDesktopClosedStore)
	if !ok {
		slog.WarnContext(ctx, "wardynd: desktop session refused: this store keeps no Closed set", "run_id", sess.Run)
		return false, true
	}
	closed, err := st.UIDesktopClosed(ctx, uiDesktopClosedKey(sess), now)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: desktop session refused: the Closed set could not be read", "run_id", sess.Run, "err", err)
		return false, true
	}
	if closed {
		s.cacheDesktopClosed(sess)
	}
	return closed, false
}

func (s *Server) cacheDesktopClosed(sess uiSession) {
	d := &s.uiDesktop
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed == nil {
		d.closed = map[string]int64{}
	}
	d.closed[uiReassertKey(sess)] = sess.Expires
}

// closeDesktop ends sess: its cookie is refused from now on on every replica, wherever it was
// copied, and every stream it opened here is cut (one on another replica ends at its next
// uiDesktopClosedRecheck). An error means nothing was closed: the Close was not stored.
func (s *Server) closeDesktop(ctx context.Context, sess uiSession) error {
	st, ok := s.cfg.Store.(store.UIDesktopClosedStore)
	if !ok {
		return errors.New("this store keeps no desktop Closed set")
	}
	if err := st.CloseUIDesktop(ctx, uiDesktopClosedKey(sess), time.Unix(sess.Expires, 0)); err != nil {
		return err
	}
	s.cacheDesktopClosed(sess)
	d := &s.uiDesktop
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, cancel := range d.live[uiReassertKey(sess)] {
		cancel()
	}
	return nil
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
		s.auditUI(&run.ID, types.ActorHuman, ta.principal, "ui.authorize", uiDesktopAppName, "denied",
			ta.withVia(map[string]any{"reason": "the run is not a desktop VM"}))
		writeErrorReason(w, http.StatusForbidden, reasonUIGatewayAppNotDeclared, uiNoDesktopMsg)
		return
	}
	if s.macOSDesktopSignInsOff(r.Context(), run) {
		s.auditUI(&run.ID, types.ActorHuman, ta.principal, "ui.authorize", uiDesktopAppName, "denied",
			ta.withVia(map[string]any{"reason": "the agent does not allow sign-ins in a desktop, which a macOS desktop cannot block"}))
		writeErrorReason(w, http.StatusForbidden, reasonDesktopSignInsOff, macOSDesktopSignInsOffMsg(run.Agent))
		return
	}
	if (!superAdmin || !s.adminDoorExempt(run)) && s.refuseInteractiveAttach(w, r.WithContext(withTicketActor(r.Context(), ta)), run) {
		return
	}
	sess := s.newUISession(run, ta, uiDesktopAppName, 0, min(s.uiSessionTTL(), uiDesktopSessionTTL))
	sess.Desktop = true
	// A background run reaches here only through the watch door; interactive sessions stay unpinned.
	sess.ViewOnly = run.BackgroundOnly() && ticketOpensBackgroundWatch(ta, uiDesktopAppName)
	data := withBackgroundWatch(sess.backgroundWatch(), map[string]any{"app": uiDesktopAppName, "machine_kind": string(run.MachineKind)})
	s.startUISession(w, r, run, ta, sess, withViewOnly(sess.ViewOnly || s.macOSDesktopViewOnly(run), data), "/", redirectStatus)
}

// handleDesktopRelay serves /r/<run>/__desktop/...: the viewer's own files (no session: they are the
// console bundle's public assets), then, with a desktop session for this run, the viewer page, the
// RFB stream and Close.
func (s *Server) handleDesktopRelay(w http.ResponseWriter, r *http.Request, runID uuid.UUID) {
	rest := strings.TrimPrefix(r.URL.Path, uiRelayPrefix(runID, uiDesktopAppName))
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
		boot := map[string]any{
			"state": "open", "expires": sess.Expires,
			"rfb":   s.uiBasePath() + uiRelayPrefix(runID, uiDesktopAppName) + uiDesktopRFB,
			"close": s.uiBasePath() + uiRelayPrefix(runID, uiDesktopAppName) + uiDesktopClose,
		}
		// From the signed session, so the viewer's strip is right even if the run below can't be read.
		if sess.backgroundWatch() {
			boot["view_only"], boot["background"] = true, true
		}
		// The run's machine kind, so the viewer names a macOS desktop while it starts. A run that
		// cannot be read is served without it: the viewer then says what it says of any desktop.
		if run, err := s.cfg.Store.GetRun(r.Context(), runID); err == nil {
			if run.MachineKind != "" {
				boot["machine_kind"] = string(run.MachineKind)
				if s.macOSDesktopViewOnly(run) {
					boot["view_only"] = true
				}
			}
			// The run's name and page, for the viewer's way back to it: both or neither. Only on an open
			// session's page, never on a refusal, which anyone holding the link is shown.
			if href := s.desktopRunHref(runID); run.Title != "" && href != "" {
				boot["run_title"], boot["run_href"] = run.Title, href
			}
		}
		s.serveDesktopViewer(w, r, runID, boot, http.StatusOK)
	case rest == uiDesktopRFB:
		s.serveDesktopRFB(w, r, sess)
	case rest == uiDesktopClose && r.Method == http.MethodPost:
		if err := s.closeDesktop(r.Context(), sess); err != nil {
			slog.WarnContext(r.Context(), "wardynd: desktop Close could not be stored", "run_id", runID, "err", err)
			writeErrorReason(w, http.StatusServiceUnavailable, reasonStoreError, "could not close this desktop session; try again")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: uiCookieName, Value: "", Path: s.uiBasePath() + uiCookiePath(runID, uiDesktopAppName),
			HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.cfg.OIDCSecureCookies, MaxAge: -1})
		w.WriteHeader(http.StatusNoContent)
	default:
		writeErrorReason(w, http.StatusNotFound, reasonUIGatewayNotFound, "not found on the Wardyn UI gateway (open an app from the run detail page)")
	}
}

// desktopRunHref is the run's page on the console, absolute because the viewer is served from the gateway's
// origin: the console's origin as WARDYN_OIDC_REDIRECT_URL names it (uiOriginIsConsole) and its base path. ""
// when no http(s) console URL is configured.
func (s *Server) desktopRunHref(runID uuid.UUID) string {
	u, err := url.Parse(strings.TrimSpace(s.cfg.OIDCRedirectURL))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return u.Scheme + "://" + u.Host + s.cfg.BasePath + "/runs/" + runID.String()
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
	return app == uiDesktopAppName && s.serveDesktopRefusal(w, r, runID, uiTicketInvalidMsg)
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
	root := s.uiBasePath() + uiRelayPrefix(runID, uiDesktopAppName) + "/"
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
	// The run's desktop rules, read once before the socket opens: rules that cannot be read refuse the
	// stream, never relay it under the defaults (runDesktopPolicy, as the novnc relay does).
	_, rules, _, err := s.runDesktopPolicy(r.Context(), sess.Run)
	if err != nil {
		slog.WarnContext(r.Context(), "wardynd: desktop rules unreadable; the desktop stream is refused", "run_id", sess.Run, "err", err)
		writeErrorReason(w, http.StatusServiceUnavailable, reasonDesktopRulesUnreadable, desktopRulesUnreadableMsg)
		return
	}
	// A recorded run's stream keeps no frames while the run's row says its agent is paused to sign in or
	// a take over is open, from the first frame and at each vmSignInRecheck after; a mark or take over
	// that cannot be read refuses the stream rather than record it.
	signIn, err := s.watchVMSignIn(r.Context(), sess.Run)
	if err != nil {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonDesktopSignInsUnavailable, desktopSignInsUnreadableMsg)
		return
	}
	defer signIn.stop()
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"binary"}})
	if err != nil {
		return // Accept answered the request
	}
	c := desktopSocket{ws}
	// ctx ends the stream at the bound and at Close; the socket itself lives on the request, so the
	// stream can tell the viewer why before it goes.
	// A window that opens on the Mac ends a stream with control with errMacWindowTookControl as its cause.
	yielding, yield := context.WithCancelCause(r.Context())
	defer yield(nil)
	ctx, cancel := context.WithDeadline(yielding, time.Unix(sess.Expires, 0))
	defer cancel()
	go signIn.watch(ctx) // from here on, so a pause during the dial is held before the first frame
	untrack := s.trackDesktop(sess, cancel)
	defer untrack()
	// A Close here between this request's decode and trackDesktop cancelled nothing of this stream.
	if s.uiDesktopClosedHere(sess, s.cfg.Now()) {
		_ = c.Close(desktopCloseSessionEnded, "this desktop session ended")
		return
	}

	run, code, why := s.desktopBoundedRunCheck(ctx, sess)
	if code != 0 {
		_ = c.Close(code, why)
		return
	}
	dd, ok := s.cfg.Runner.(runner.DesktopDialer)
	if !ok {
		_ = c.Close(desktopCloseUnreachable, "no runner configured; UI apps unavailable")
		return
	}
	// A watch is not a person entering: it neither resumes the run nor stamps presence nor keeps the run alive.
	watch := sess.backgroundWatch()
	if !watch {
		if err := s.thawForExec(ctx, run, types.ActorHuman, sess.Principal, "presence"); err != nil {
			_ = c.Close(desktopCloseUnreachable, "run is paused and could not be resumed; try again")
			return
		}
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

	if !watch {
		go s.attachKeepalive(ctx, sess.Run)
	}
	opened := s.cfg.Now()
	viewOnly := sess.ViewOnly || s.macOSDesktopViewOnly(run)
	yielded := func() {} // forgets this stream's control; a window waits for it, never for the close frame
	if !viewOnly && run.MachineKind.IsMacOSVM() {
		// A stream with control is known to a window that opens (macWindowYieldControl). Registered before the
		// window is looked for once more, so a window that opened since viewOnly was read ends this stream
		// instead of running beside it.
		yielded = s.desktopControl.track(sess.Run, func() { yield(errMacWindowTookControl) })
		defer yielded()
		if s.macWindows.open(sess.Run) != nil {
			yielded()
			_ = c.Close(desktopCloseWindowOpen, desktopWindowOpenMsg)
			return
		}
	}
	s.auditUISession(sess, "ui.open", uiDesktopAppName, "success", withBackgroundWatch(watch, withViewOnly(viewOnly, map[string]any{"app": uiDesktopAppName})))
	defer func() {
		s.auditUISession(sess, "ui.close", uiDesktopAppName, "success",
			withBackgroundWatch(watch, withViewOnly(viewOnly, map[string]any{"app": uiDesktopAppName, "duration_sec": int(s.cfg.Now().Sub(opened).Seconds())})))
	}()

	conn := websocket.NetConn(r.Context(), ws, websocket.MessageBinary)
	recorded, rec := s.recordVMDisplay(run, sess, &wsFramed{raw: display, wsUnframer: wsUnframer{w: display}})
	filtered := s.desktopClipboardFilter(ctx, sess, rules, viewOnly, rec, recorded)
	defer func() { _ = filtered.Close() }()
	// A window waits for this stream's control to end (yielded), so its input stops before the close frame goes.
	windowOpened := func() {
		_ = filtered.Close()
		yielded()
		_ = c.Close(desktopCloseWindowOpen, desktopWindowOpenMsg)
	}
	done := make(chan struct{}, 2)
	go func() { _ = relayViewer(r.Context(), ws, filtered); done <- struct{}{} }()
	go func() { _, _ = io.Copy(&wsUnframer{w: conn}, filtered); done <- struct{}{} }()
	tick := time.NewTicker(cmp.Or(s.uiDesktop.recheckEvery, uiDesktopRecheck))
	defer tick.Stop()
	closedTick := time.NewTicker(cmp.Or(s.uiDesktopClosedEvery, uiDesktopClosedRecheck))
	defer closedTick.Stop()
	for {
		select {
		case <-done:
			if windowTookControl(ctx) {
				windowOpened()
				return
			}
			_ = c.Close(desktopCloseUnreachable, "the desktop closed the connection")
			return
		case <-ctx.Done():
			// A window that opened, the bound, Close, or the browser leaving: only the first three are told.
			switch {
			case windowTookControl(ctx):
				windowOpened()
			case errors.Is(ctx.Err(), context.DeadlineExceeded) || s.uiDesktopClosedHere(sess, s.cfg.Now()):
				_ = c.Close(desktopCloseSessionEnded, "this desktop session ended")
			}
			return
		case <-closedTick.C:
			// A Close on another replica, or a Closed set that cannot be read, ends the stream.
			if code, why := s.desktopBoundedClosedCheck(ctx, sess); code != 0 {
				_ = c.Close(code, why)
				return
			}
		case <-tick.C:
			if _, code, why := s.desktopBoundedRunCheck(ctx, sess); code != 0 {
				_ = c.Close(code, why)
				return
			}
		}
	}
}

// withViewOnly marks an audit row of a view-only desktop session.
func withViewOnly(viewOnly bool, data map[string]any) map[string]any {
	if viewOnly {
		data["view_only"] = true
	}
	return data
}

// withBackgroundWatch marks an audit row of a background run's watch: view only, and background.
func withBackgroundWatch(watch bool, data map[string]any) map[string]any {
	if watch {
		data["view_only"], data["background"] = true, true
	}
	return data
}

// windowTookControl reports that ctx's stream ended because a window opened on the Mac (macWindowYieldControl).
func windowTookControl(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errMacWindowTookControl)
}

// desktopSocket closes with words cut to what a close frame carries (123 bytes).
type desktopSocket struct{ *websocket.Conn }

func (d desktopSocket) Close(code websocket.StatusCode, why string) error {
	if len(why) > 123 {
		why = strings.ToValidUTF8(why[:123], "")
	}
	return d.Conn.Close(code, why)
}

// desktopBoundedRunCheck is desktopRunCheck under uiDesktopRecheckWait: a read that runs out of it is a read that failed.
func (s *Server) desktopBoundedRunCheck(ctx context.Context, sess uiSession) (types.AgentRun, websocket.StatusCode, string) {
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(s.uiDesktop.recheckWait, uiDesktopRecheckWait))
	defer cancel()
	return s.desktopRunCheck(ctx, sess)
}

// desktopBoundedClosedCheck is an open stream's read of the Closed set under uiDesktopRecheckWait, answering the
// close code and words that end it, or 0 while it may go on. A watch that can't be verified ends as any re-check
// ends a watch; a session its person closed says so, and any other session keeps the one answer it has.
func (s *Server) desktopBoundedClosedCheck(ctx context.Context, sess uiSession) (websocket.StatusCode, string) {
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(s.uiDesktop.recheckWait, uiDesktopRecheckWait))
	defer cancel()
	closed, unread := s.uiDesktopClosedRead(ctx, sess, s.cfg.Now())
	switch {
	case unread && sess.backgroundWatch():
		return desktopCloseWatchEnded, backgroundWatchEndedMsg
	case closed || unread:
		return desktopCloseSessionEnded, "this desktop session ended"
	}
	return 0, ""
}

// desktopRunCheck loads the session's run fresh and answers the close code and words that end its
// stream, or 0 while it may go on. On a watch, a run that can't be read ends the view rather than retry it.
func (s *Server) desktopRunCheck(ctx context.Context, sess uiSession) (types.AgentRun, websocket.StatusCode, string) {
	run, err := s.cfg.Store.GetRun(ctx, sess.Run)
	if err != nil {
		if sess.backgroundWatch() {
			return run, desktopCloseWatchEnded, backgroundWatchEndedMsg
		}
		return run, desktopCloseUnreachable, "run not found"
	}
	if run.State != types.RunRunning || run.SandboxRef == "" || runIsKept(run) {
		return run, desktopCloseRunEnded, "run has ended; cannot open a UI app"
	}
	if de := s.uiSessionStillAuthorized(ctx, sess, run); de != nil {
		if sess.backgroundWatch() {
			return run, desktopCloseWatchEnded, backgroundWatchEndedMsg
		}
		return run, desktopCloseNotAuthorized, de.msg
	}
	return run, 0, ""
}

// desktopClipboardFilter is uiFilterUpgrade's clipboard filter on a VM's display: the same desktop
// copy_out and paste_in rules and the same ui.clipboard.transfer rows (direction and size, never the
// text). QEMU's VNC has no clipboard peer in the guest (no qemu-vdagent, internal/vm/qemu/args.go),
// so no cut text flows today; the filter holds the rules for when one does. viewOnly also keeps the
// viewer's input from the display. A background run's watch moves nothing but the picture, whatever the run's
// rules and the Mac's caps say, and this filter holds that independently of the helper's.
func (s *Server) desktopClipboardFilter(ctx context.Context, sess uiSession, d types.DesktopControls, viewOnly bool, rec *recording.RFBRecorder, display io.ReadWriteCloser) *rfbfilter.Conn {
	rules := rfbfilter.Rules{PasteIn: d.PasteIn == types.TransferAllowed, CopyOut: d.CopyOut == types.TransferAllowed, ViewOnly: viewOnly}
	if sess.backgroundWatch() {
		rules = rfbfilter.Rules{ViewOnly: true}
	}
	rules.Recorded = rec != nil // recordVMDisplay's recorder sits under the filter (Rules.Recorded)
	filtered := rfbfilter.Wrap(display, rules, func(ev rfbfilter.Event) { s.auditUIClipboard(sess, ev) }, func(err error) {
		slog.WarnContext(ctx, "wardynd: desktop relay closed a VM display it could not follow", "run_id", sess.Run, "err", err)
		stopRefusedRecording(rec, err)
		if errors.Is(err, rfbfilter.ErrNotRFB) {
			s.auditUISession(sess, "ui.clipboard.transfer", "", "failure", map[string]any{"app": sess.App, "reason": "not_rfb"})
		}
	})
	if s.uiDesktopSink != nil {
		filtered.SetSink(s.uiDesktopSink(sess))
	}
	return filtered
}

// rfbfilter follows RFB inside WebSocket frames, as websockify's socket carries it on the noVNC
// path. Both hops of a VM's display are raw RFB, so each chunk goes into the filter as one unmasked
// binary frame (wsFramed on the display's side, wsFramer on the viewer's) and comes out unframed.
const wsFrameHeader = 10 // the longest unmasked header: 2 bytes and a 64-bit length

func appendWSBinary(dst, p []byte) []byte {
	switch n := len(p); {
	case n < 126:
		dst = append(dst, 0x82, byte(n))
	case n <= 0xffff:
		dst = binary.BigEndian.AppendUint16(append(dst, 0x82, 126), uint16(n))
	default:
		dst = binary.BigEndian.AppendUint64(append(dst, 0x82, 127), uint64(n))
	}
	return append(dst, p...)
}

// wsFramer frames what the viewer sends.
type wsFramer struct{ w io.Writer }

func (f wsFramer) Write(p []byte) (int, error) {
	if _, err := f.w.Write(appendWSBinary(nil, p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// relayViewer hands the viewer's messages to the filter as they came: a binary message as its bytes (wsFramer), a
// text message as an empty text frame, its text never relayed, which the filter refuses (or holds and then refuses)
// as it does on a container desktop, so a recorded display's stop is marked with what was refused. NetConn's Read
// would refuse a text message itself, unseen by the filter. It returns at the first error.
func relayViewer(ctx context.Context, ws *websocket.Conn, filtered io.Writer) error {
	for {
		typ, msg, err := ws.Reader(ctx)
		if err != nil {
			return err
		}
		var dst io.Writer = wsFramer{filtered}
		if typ != websocket.MessageBinary {
			if _, err = filtered.Write([]byte{0x81, 0}); err != nil {
				return err
			}
			dst = io.Discard
		}
		if _, err = io.Copy(dst, msg); err != nil {
			return err
		}
	}
}

// wsUnframer writes the payloads of the filter's frames. They are the unmasked frames the framers
// made, re-encoded by the filter, so no mask is ever set.
type wsUnframer struct {
	w       io.Writer
	pending []byte
}

func (u *wsUnframer) Write(p []byte) (int, error) {
	u.pending = append(u.pending, p...)
	for {
		b := u.pending
		if len(b) < 2 {
			return len(p), nil
		}
		size, off := uint64(b[1]&0x7f), 2
		switch size {
		case 126:
			if len(b) < 4 {
				return len(p), nil
			}
			size, off = uint64(binary.BigEndian.Uint16(b[2:])), 4
		case 127:
			if len(b) < 10 {
				return len(p), nil
			}
			size, off = binary.BigEndian.Uint64(b[2:]), 10
		}
		if uint64(len(b)-off) < size {
			return len(p), nil
		}
		end := off + int(size)
		if end > off {
			if _, err := u.w.Write(b[off:end]); err != nil {
				return 0, err
			}
		}
		u.pending = b[end:]
	}
}

// wsFramed is the display as the filter's backend: reads come framed, writes go out unframed.
type wsFramed struct {
	raw io.ReadWriteCloser
	wsUnframer
}

func (f *wsFramed) Read(p []byte) (int, error) {
	if len(p) <= wsFrameHeader {
		return 0, io.ErrShortBuffer
	}
	n, err := f.raw.Read(p[wsFrameHeader:])
	if n == 0 {
		return 0, err
	}
	return copy(p, appendWSBinary(nil, p[wsFrameHeader:wsFrameHeader+n])), err
}

func (f *wsFramed) Close() error { return f.raw.Close() }
