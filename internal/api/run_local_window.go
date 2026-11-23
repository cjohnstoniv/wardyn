// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A local window onto a Linux VM run: the person's own CLI, on the computer whose runner holds the run, shows the
// VM's display in a native window. The CLI asks the runner's local socket for a window nonce and brings it here;
// the control plane decides, per open, and grants it with an in-flight call to the runner (localWindowSub) that
// names the nonce. The runner splices its socket to the display, so no frame crosses the organisation, and the
// window lives exactly as long as that call's session: wardynd ends it by closing the session, the runner ends it
// by ending it, and every end writes one takeover.close row.
//
// localWindowCheck is the one question the open and the re-check ask, in the order below. Unlike the Mac's window
// it is no exception to the background contract: a background-only run's desktop stays closed (run_mode_doors.go).
// An open window is the person's presence: each re-check that keeps it stamps the run, so the run does not idle
// out from under them, and a re-check that finds the run paused closes the window rather than thawing it.
//
// What is open lives in this replica's registry, one window per run, beside the Mac's.

// The words of a local window's refusals.
const (
	localWindowSiteOffMsg         = "Opening a Linux VM run in a window on your own computer is turned off for this install. An admin can turn it on with features.local_window.enabled in the site config."
	localWindowKindMsg            = "Only a Linux VM run opens in a window on your computer."
	localWindowNotYourComputerMsg = "Wardyn opens a window only onto a run on a runner you registered on your own computer. Watch its desktop from the run page instead."
	localWindowRecordingMsg       = "This run's recording level is Screen and actions, and a window on your computer isn't recorded, so Wardyn won't open one. Watch its desktop from the run page instead."
	localWindowOfflineMsg         = "This run's runner isn't connected to this server, so Wardyn can't open a window onto it."
	localWindowRunnerTooOldMsg    = "This run's runner can't open a window on your computer: it is older than this server, or it was started with --local-attach=off."
	localWindowPausedMsg          = "This run is paused. You can open its window once it is running again."
	localWindowPreparingMsg       = "This run is still being prepared. You can open its window once preparation finishes."
	localWindowOpenMsg            = "A window onto this run is already open."
	localWindowDesktopControlMsg  = "A browser desktop that has control of this run didn't close in time, so Wardyn can't open the window. Close it and open the window again."
	localWindowNonceMsg           = "The runner on your computer didn't accept this window's nonce: it expired, or it was asked for another run. Run `wardyn window` again."
	localWindowFailedMsg          = "Wardyn couldn't open the window on your computer. The run goes on; try again."
	localWindowNonceMissingMsg    = "The request needs local_nonce, the window nonce your computer's runner handed out."
)

const (
	localWindowSurface      = "local_window"
	localWindowRecheckEvery = 30 * time.Second
	localWindowOpenWait     = 30 * time.Second
	// localWindowMaxNonce bounds local_nonce; the runner's nonce is 43 characters.
	localWindowMaxNonce = 128
	// localWindowDisplayRefused is the takeover.close reason of a window whose runner's display filter refused
	// the stream; the row carries refusal_kind.
	localWindowDisplayRefused = "display_refused"
)

// errLocalWindowNonce is the runner's refusal of the window's nonce (unknown, expired, a terminal's, or asked for
// another run). Any other error from localWindowSub.LocalWindow is localWindowFailedMsg.
var errLocalWindowNonce = errors.New("local window: the runner refused the window's nonce")

// localWindowSub is a runner's end of the grant. The runner's adapter implements it.
type localWindowSub interface {
	// LocalWindowSupported is the runner's live caps: supported is Caps.LocalWindow, reported is false while no
	// caps have arrived.
	LocalWindowSupported() (supported, reported bool)
	// LocalWindow asks the runner to show the sandbox ref's display to the CLI holding req.Nonce. ctx bounds the
	// open only; the returned session outlives it. It returns errLocalWindowNonce when the runner refuses the nonce.
	LocalWindow(ctx context.Context, ref string, req localWindowRequest) (localWindowSession, error)
}

// localWindowRequest is one open: the CLI's window nonce, the grant id and the run's title for the window.
type localWindowRequest struct {
	Nonce   string
	GrantID uuid.UUID
	Title   string
}

// localWindowSession is the org's end of an open local window.
type localWindowSession interface {
	// Ended is closed once the window has ended on the runner's side.
	Ended() <-chan struct{}
	// EndReason, RefusalKind and CutTextDropped are the runner's end line, after Ended: the reason (person,
	// run_ended, refused, helper_failed; anything else reads as link_lost), what the display filter refused, and
	// how many cut-text messages it dropped.
	EndReason() string
	RefusalKind() string
	CutTextDropped() int
	// Close ends the window (it resets the stream, and the runner closes the CLI's socket). Idempotent.
	Close() error
}

// remoteLocalWindowSub is a runner's remote substrate as the grant's localWindowSub.
type remoteLocalWindowSub struct{ sub *remote.Substrate }

func (r remoteLocalWindowSub) LocalWindowSupported() (supported, reported bool) {
	return r.sub.LocalWindowSupported()
}

func (r remoteLocalWindowSub) LocalWindow(ctx context.Context, ref string, req localWindowRequest) (localWindowSession, error) {
	w, err := r.sub.LocalWindow(ctx, ref, remote.LocalWindowRequest{Nonce: req.Nonce, GrantID: req.GrantID, Title: req.Title})
	switch {
	case errors.Is(err, remote.ErrLocalWindowRefused):
		return nil, errLocalWindowNonce
	case err != nil:
		return nil, err
	}
	return w, nil
}

// localWindowSubFor is runner id's window end, and whether the runner is connected to this replica (the test seam
// localWindowSubs replaces it; a nil sub there reads as not connected).
func (s *Server) localWindowSubFor(id uuid.UUID) (localWindowSub, bool) {
	if s.localWindowSubs != nil {
		sub := s.localWindowSubs(id)
		return sub, sub != nil
	}
	s.runnerStreams.subsMu.Lock()
	ep, live := s.runnerStreams.subs[id]
	s.runnerStreams.subsMu.Unlock()
	if !live {
		return nil, false
	}
	if _, online := ep.sub.Advertised(); !online {
		return nil, false
	}
	return remoteLocalWindowSub{sub: ep.sub}, true
}

// localWindowTicks is the re-check clock (the test seam localWindowTick replaces it).
func (s *Server) localWindowTicks(d time.Duration) (<-chan time.Time, func()) {
	if s.localWindowTick != nil {
		return s.localWindowTick(d)
	}
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// localWindow is one open local window on this replica.
type localWindow struct {
	runID    uuid.UUID
	grantID  uuid.UUID
	runnerID uuid.UUID
	runner   string
	auth     macWindowAuthority
	opened   time.Time

	sess   localWindowSession
	cancel context.CancelFunc
	once   sync.Once
}

// localWindowHost is what the Server holds for local windows, embedded so its fields read as the Server's own.
type localWindowHost struct {
	// localWindows is this replica's open local windows, one per run. Zero value is ready.
	localWindows localWindowRegistry
	// localWindowSubs replaces a runner's window end (localWindowSubFor), and localWindowTick the re-check clock
	// (localWindowTicks), tests only.
	localWindowSubs func(id uuid.UUID) localWindowSub
	localWindowTick func(d time.Duration) (<-chan time.Time, func())
}

// localWindowRegistry is this replica's open local windows, one per run.
type localWindowRegistry struct {
	mu    sync.Mutex
	byRun map[uuid.UUID]*localWindow
}

// reserve puts w in the registry and says false, leaving it as it was, when the run already has a window.
func (g *localWindowRegistry) reserve(w *localWindow) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.byRun == nil {
		g.byRun = map[uuid.UUID]*localWindow{}
	}
	if _, held := g.byRun[w.runID]; held {
		return false
	}
	g.byRun[w.runID] = w
	return true
}

// release drops w, and only w: a later window for the run stays.
func (g *localWindowRegistry) release(w *localWindow) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.byRun[w.runID] == w {
		delete(g.byRun, w.runID)
	}
}

// open is the run's local window on this replica, nil for none. A slot reserved by an open still in progress
// counts.
func (g *localWindowRegistry) open(runID uuid.UUID) *localWindow {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.byRun[runID]
}

// localWindowAsk says which rows localWindowCheck skips: reopen is the re-check of a window that is itself the one
// open (row 11 would refuse it).
type localWindowAsk struct {
	reopen bool
}

// localWindowCheck is nil only when a local window may open on run for a right now, and returns the run's runner
// row, the caller's own when the answer is nil. The rows, in order:
//
//	1  the site switch features.local_window is on (an unreadable site config is a server error)
//	2  a Linux VM run
//	3  running (or starting: row 10 names it), not kept
//	4  not background-only: the door contract keeps a background run's desktop closed
//	5  the caller's own runner, strictly (macWindowOwnRunner): no delegate, service identity or operator's run
//	6  the run's profile allows an interactive desktop (runs.desktop_window)
//	7  the run's recording level is not Screen and actions
//	8  the runner is connected to this replica and its caps serve local windows
//	9  not paused
//	10 not still being prepared
//	11 no window already open on this run (this replica's local registry or the Mac's)
//
// The rows write nothing, so a read may ask them.
func (s *Server) localWindowCheck(ctx context.Context, run types.AgentRun, a macWindowAuthority, ask localWindowAsk) (types.Runner, *runRefusal) {
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return types.Runner{}, runServerError("read site config", err)
	}
	if !localWindowOn(sc) {
		return types.Runner{}, runError(http.StatusForbidden, reasonLocalWindowSiteOff, localWindowSiteOffMsg)
	}
	if run.MachineKind != types.MachineLinuxVM {
		return types.Runner{}, runError(http.StatusConflict, reasonLocalWindowKind, localWindowKindMsg)
	}
	if run.State != types.RunRunning && run.State != types.RunStarting {
		return types.Runner{}, runError(http.StatusConflict, reasonAttachNotRunning, "run is not RUNNING; cannot open its window (state="+string(run.State)+")")
	}
	if runIsKept(run) {
		return types.Runner{}, runError(http.StatusConflict, reasonAttachRunKept, "run has ended; cannot open its window")
	}
	if rf := backgroundOnlyRefusal(run, "desktop"); rf != nil {
		return types.Runner{}, rf
	}
	rn, rf := s.macWindowOwnRunner(ctx, run, a)
	if rf != nil {
		if rf.err == nil && rf.decision == nil && rf.body.Reason == reasonMacWindowNotYourMac {
			rf = runError(http.StatusForbidden, reasonLocalWindowNotYourComputer, localWindowNotYourComputerMsg)
		}
		return rn, rf
	}
	if rf := s.localWindowGovernanceRefusal(ctx, run); rf != nil {
		return rn, rf
	}
	if run.RecordingLevel == types.RecordingScreenActions {
		return rn, runError(http.StatusForbidden, reasonLocalWindowRecording, localWindowRecordingMsg)
	}
	sub, connected := s.localWindowSubFor(rn.ID)
	supported, reported := false, false
	if connected {
		supported, reported = sub.LocalWindowSupported()
	}
	switch {
	case !connected || !reported:
		return rn, runError(http.StatusConflict, reasonLocalWindowOffline, localWindowOfflineMsg)
	case !supported:
		return rn, runError(http.StatusConflict, reasonLocalWindowRunnerTooOld, localWindowRunnerTooOldMsg)
	}
	if run.PausedAt != nil {
		return rn, runError(http.StatusConflict, reasonLocalWindowPaused, localWindowPausedMsg)
	}
	if macRunPreparing(run) {
		return rn, runError(http.StatusConflict, reasonLocalWindowPreparing, localWindowPreparingMsg)
	}
	if !ask.reopen && (s.localWindows.open(run.ID) != nil || s.macWindows.open(run.ID) != nil) {
		return rn, runError(http.StatusConflict, reasonLocalWindowOpen, localWindowOpenMsg)
	}
	return rn, nil
}

// localWindowGovernanceRefusal is row 6: the run's profile, as the Mac's window asks it (runs.desktop_window).
func (s *Server) localWindowGovernanceRefusal(ctx context.Context, run types.AgentRun) *runRefusal {
	p, err := s.runUnit(ctx, run)
	if err != nil {
		if u, ok := isOverlayUnsatisfiable(err); ok {
			return runDenied(authz.Deny(authz.ReasonGovernanceOverlayUnsatisfiable, "runs.desktop_window", u.Error()).OnRun(run.ID))
		}
		return runServerError("read the run's governance profile", err)
	}
	if p == nil {
		return nil
	}
	return s.macWindowGovernanceRefusal(ctx, governanceCeiling{Limits: p.Limits, Unit: p}, run.ID)
}

// localWindowData is the fields every takeover row of a local window carries.
func localWindowData(runnerID *uuid.UUID) map[string]any {
	data := map[string]any{"surface": localWindowSurface, "recorded": false}
	if runnerID != nil {
		data["runner_id"] = runnerID.String()
	}
	return data
}

// auditLocalWindowDenied writes the denied takeover.begin row for refusal rf, with the run as the target.
func (s *Server) auditLocalWindowDenied(ctx context.Context, run types.AgentRun, a macWindowAuthority, rf *runRefusal) {
	data := localWindowData(run.RunnerID)
	data["reason"] = macWindowRefusalReason(rf)
	s.auditMacWindowBegin(ctx, &run.ID, a, run.ID.String(), "denied", data)
}

// localWindowEnd is why a window ended, as takeover.close writes it: the reason, the check that refused when a
// re-check ended it, and the runner's display filter's refusal kind and dropped cut-text count.
type localWindowEnd struct {
	reason      string
	by          string
	refusalKind string
	dropped     int
}

// auditLocalWindowClose writes takeover.close: why the window ended, and for how long it was open.
func (s *Server) auditLocalWindowClose(ctx context.Context, w *localWindow, end localWindowEnd) {
	data := localWindowData(&w.runnerID)
	data["grant_id"] = w.grantID.String()
	data["held_ms"] = s.cfg.Now().Sub(w.opened).Milliseconds()
	data["reason"] = end.reason
	if end.by != "" {
		data["refused_by"] = end.by
	}
	if end.refusalKind != "" {
		data["refusal_kind"] = end.refusalKind
	}
	if end.dropped > 0 {
		data["cut_text_dropped"] = end.dropped
	}
	s.recordAudit(ctx, s.auditEvent(&w.runID, w.auth.ActorType, w.auth.Principal, "takeover.close", w.grantID.String(), "success", mustJSON(data)))
}

// openLocalWindow grants a local window on run, for the CLI holding nonce, to the person a stands for. Every
// refusal writes a denied takeover.begin; a window that opens writes takeover.begin and is watched until it ends.
// The order is the Mac window's: the registry slot is reserved first, so two opens cannot both reach the runner;
// then every browser desktop session with control of the run is ended (macWindowYieldControl); only then is the
// runner called. The open stamps the run's presence once; a stamp that fails closes the window.
func (s *Server) openLocalWindow(ctx context.Context, run types.AgentRun, a macWindowAuthority, nonce string) (*localWindow, *runRefusal) {
	refuse := func(rf *runRefusal) (*localWindow, *runRefusal) {
		s.auditLocalWindowDenied(ctx, run, a, rf)
		return nil, rf
	}
	rn, rf := s.localWindowCheck(ctx, run, a, localWindowAsk{})
	if rf != nil {
		return refuse(rf)
	}
	revoked, err := s.macWindowRevoked(ctx, a)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: could not check a local window's credential", "run_id", run.ID, "err", err)
		if errors.Is(err, errMacWindowNoBinding) {
			return refuse(runError(http.StatusForbidden, uiDeniedReasonRevocationUnavailable, macWindowRevocationUnavailableMsg))
		}
		return refuse(runServerError("check the session", err))
	}
	if revoked {
		return refuse(runError(http.StatusForbidden, reasonMacWindowRevoked, macWindowRevokedMsg))
	}
	if nonce == "" || len(nonce) > localWindowMaxNonce {
		return refuse(runError(http.StatusBadRequest, reasonInvalidRequestBody, localWindowNonceMissingMsg))
	}
	sub, ok := s.localWindowSubFor(rn.ID)
	if !ok {
		return refuse(runError(http.StatusConflict, reasonLocalWindowOffline, localWindowOfflineMsg))
	}
	wctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	w := &localWindow{runID: run.ID, grantID: uuid.New(), runnerID: rn.ID, runner: rn.Name, auth: a, opened: s.cfg.Now(), cancel: cancel}
	if !s.localWindows.reserve(w) {
		cancel()
		return refuse(runError(http.StatusConflict, reasonLocalWindowOpen, localWindowOpenMsg))
	}
	fail := func(rf *runRefusal) (*localWindow, *runRefusal) {
		if w.sess != nil {
			_ = w.sess.Close()
		}
		s.localWindows.release(w)
		cancel()
		return refuse(rf)
	}
	if !s.macWindowYieldControl(ctx, run.ID) {
		return fail(runError(http.StatusConflict, reasonLocalWindowDesktopControl, localWindowDesktopControlMsg))
	}
	title := run.Title
	if title == "" {
		title = "Run " + run.ID.String()[:8]
	}
	octx, ocancel := context.WithTimeout(ctx, localWindowOpenWait)
	sess, err := sub.LocalWindow(octx, run.SandboxRef, localWindowRequest{Nonce: nonce, GrantID: w.grantID, Title: title})
	ocancel()
	switch {
	case errors.Is(err, errLocalWindowNonce):
		return fail(runError(http.StatusConflict, reasonLocalWindowNonce, localWindowNonceMsg))
	case err != nil || sess == nil:
		slog.WarnContext(ctx, "wardynd: the runner could not open a local window", "run_id", run.ID, "err", err)
		return fail(runError(http.StatusBadGateway, reasonLocalWindowFailed, localWindowFailedMsg))
	}
	w.sess = sess
	if _, err := s.stampPresence(ctx, run.ID, types.ActorHuman, a.Principal, localWindowSurface); err != nil {
		return fail(runError(http.StatusConflict, reasonLocalWindowPaused, localWindowPausedMsg))
	}
	data := localWindowData(&w.runnerID)
	data["grant_id"] = w.grantID.String()
	s.auditMacWindowBegin(ctx, &run.ID, a, w.grantID.String(), "success", data)
	_ = s.cfg.Store.TouchRun(ctx, run.ID) // an error only leaves the run to the reaper, as in attachKeepalive
	s.goBackground(func() { s.watchLocalWindow(wctx, w) })
	return w, nil
}

// closeLocalWindow ends w once, whoever asks first: it closes the runner's session (which closes the CLI's
// socket), frees the registry slot and writes takeover.close.
func (s *Server) closeLocalWindow(ctx context.Context, w *localWindow, end localWindowEnd) {
	w.once.Do(func() {
		if err := w.sess.Close(); err != nil {
			slog.WarnContext(ctx, "wardynd: closing a local window's session failed", "run_id", w.runID, "err", err)
		}
		s.localWindows.release(w)
		w.cancel()
		s.auditLocalWindowClose(context.WithoutCancel(ctx), w, end)
	})
}

// localWindowRunnerEnd is the end of a window the runner ended, read after sess.Ended.
func localWindowRunnerEnd(sess localWindowSession) localWindowEnd {
	end := localWindowEnd{reason: macWindowLinkLost, refusalKind: sess.RefusalKind(), dropped: sess.CutTextDropped()}
	switch sess.EndReason() {
	case runnerwire.LocalWindowEndPerson:
		end.reason = macWindowClosedByPerson
	case runnerwire.LocalWindowEndRunEnded:
		end.reason = macWindowRunEnded
	case runnerwire.LocalWindowEndRefused:
		end.reason = localWindowDisplayRefused
	case runnerwire.LocalWindowEndHelperFailed:
		end.reason = macWindowHelperFailed
	}
	return end
}

// watchLocalWindow holds w until it ends: the runner ends it, wardynd is going down, or a re-check, every
// localWindowRecheckEvery, finds it no longer allowed.
func (s *Server) watchLocalWindow(ctx context.Context, w *localWindow) {
	tick, stop := s.localWindowTicks(localWindowRecheckEvery)
	defer stop()
	var down <-chan struct{}
	if s.cfg.BaseCtx != nil {
		down = s.cfg.BaseCtx.Done()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.sess.Ended():
			s.closeLocalWindow(ctx, w, localWindowRunnerEnd(w.sess))
			return
		case <-down:
			s.closeLocalWindow(ctx, w, localWindowEnd{reason: macWindowLinkLost})
			return
		case <-tick:
			if reason, by := s.localWindowRecheck(ctx, w); reason != "" {
				s.closeLocalWindow(ctx, w, localWindowEnd{reason: reason, by: by})
				return
			}
		}
	}
}

// localWindowRecheck asks again what the open asked: the run as it stands now, the person's credential, the same
// runner, and rows 1-10 of localWindowCheck. It returns the close reason, "" while the window may stay, and the
// check that refused. Anything it cannot read ends the window (fail closed). A paused run refuses at row 9 before
// any stamp, so the window closes and nothing is stamped or touched. A window that stays stamps the run's presence
// and touches the run; a stamp that fails closes it as paused.
func (s *Server) localWindowRecheck(ctx context.Context, w *localWindow) (reason, by string) {
	run, err := s.cfg.Store.GetRun(ctx, w.runID)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: could not re-check a local window", "run_id", w.runID, "err", err)
		return macWindowRefusedRecheck, reasonRunUnreadable
	}
	if isTerminalRunState(run.State) || runIsKept(run) {
		return macWindowRunEnded, ""
	}
	revoked, err := s.macWindowRevoked(ctx, w.auth)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: could not re-check a local window's credential", "run_id", w.runID, "err", err)
		return macWindowRefusedRecheck, uiDeniedReasonRevocationUnavailable
	}
	if revoked {
		return macWindowRevoked, ""
	}
	if run.RunnerID == nil || *run.RunnerID != w.runnerID {
		return macWindowRefusedRecheck, reasonLocalWindowNotYourComputer
	}
	_, rf := s.localWindowCheck(ctx, run, w.auth, localWindowAsk{reopen: true})
	switch {
	case rf == nil:
		if _, err := s.stampPresence(ctx, w.runID, types.ActorHuman, w.auth.Principal, localWindowSurface); err != nil {
			return macWindowRefusedRecheck, reasonLocalWindowPaused
		}
		_ = s.cfg.Store.TouchRun(ctx, w.runID) // as attachKeepalive: an error only leaves the run to the reaper
		return "", ""
	case rf.err == nil && rf.decision == nil && rf.body.Reason == reasonLocalWindowOffline:
		return macWindowRunnerOffline, rf.body.Reason
	}
	return macWindowRefusedRecheck, macWindowRefusalReason(rf)
}

// localWindowView is what GET /runs/{id} says of a local window onto a live Linux VM run, to its owner.
type localWindowView struct {
	// Open is whether a window onto the run is open (or opening) through this server.
	Open bool `json:"open"`
	// OpenedAt is when it opened; absent when none is open.
	OpenedAt *time.Time `json:"opened_at,omitempty"`
}

// localWindowViewFor is GET /runs/{id}'s local_window for principal: nil on any run but a live Linux VM run, and
// for a caller who may not enter it.
func (s *Server) localWindowViewFor(run types.AgentRun, principal string) *localWindowView {
	if run.MachineKind != types.MachineLinuxVM || (run.State != types.RunRunning && run.State != types.RunStarting) ||
		runIsKept(run) || !mayEnterRun(run, principal, false) {
		return nil
	}
	w := s.localWindows.open(run.ID)
	if w == nil {
		return &localWindowView{}
	}
	at := w.opened
	return &localWindowView{Open: true, OpenedAt: &at}
}

// mountLocalWindowRoutes registers the local window's route on the authenticated group.
func (s *Server) mountLocalWindowRoutes(r chi.Router) {
	r.Post("/runs/{id}/desktop/local-window", s.handleLocalWindow)
}

// localWindowBody is the body of POST /runs/{id}/desktop/local-window.
type localWindowBody struct {
	// LocalNonce is the window nonce the runner's local socket handed the CLI.
	LocalNonce string `json:"local_nonce"`
}

// localWindowGrant is the answer of an open: the grant and the runner the window is on, by id and name. The ids
// are the ones the window's takeover rows carry. The CLI reads exactly these three fields.
type localWindowGrant struct {
	GrantID  uuid.UUID `json:"grant_id"`
	RunnerID uuid.UUID `json:"runner_id"`
	Runner   string    `json:"runner"`
}

// handleLocalWindow grants a window onto a Linux VM run on the caller's own computer:
//
//	POST /api/v1/runs/{id}/desktop/local-window  {"local_nonce": "..."}
//
// The run's owner only, never a super admin or a sponsor, and only on a runner the caller registered
// (localWindowCheck). A foreign run answers as every entry route does (getRunForEntry), so a non-owner never
// learns the switch's state. It answers once the runner has bound the window; a refusal answers with its sentence
// and writes a denied takeover.begin. The nonce is for the runner alone: no answer or log line quotes it.
func (s *Server) handleLocalWindow(w http.ResponseWriter, r *http.Request) {
	authorizedAt := s.cfg.Now().UTC()
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunForEntry(w, r, id)
	if !ok {
		return
	}
	var body localWindowBody
	if err := types.DecodeOneJSON(http.MaxBytesReader(w, r.Body, 4096), &body, false); err != nil && !errors.Is(err, io.EOF) {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, "invalid request body")
		return
	}
	at, principal := actorFromRequest(r)
	win, rf := s.openLocalWindow(r.Context(), run, macWindowAuthorityFrom(r.Context(), at, principal, authorizedAt), body.LocalNonce)
	if rf.write(s, w, r) {
		return
	}
	writeJSON(w, http.StatusOK, localWindowGrant{GrantID: win.grantID, RunnerID: win.runnerID, Runner: win.runner})
}
