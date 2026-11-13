// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An attended window onto a macOS VM run: a keyboard-and-pointer window drawn on the run's own Mac by the
// run's helper, for the person whose Mac it is. The control plane decides, per open, with a grant for one run,
// one runner and one person; no token rests on the Mac. The grant is an in-flight call to the run's runner
// (macWindowSub), and the window lives exactly as long as that call's session: wardynd ends it by closing the
// session, the runner ends it by ending it, and every end writes one takeover.close row.
//
// macWindowRefusal is the one question every trigger asks (the route, the launch hook and the re-check), in the
// order below. The order puts what nobody can wait out first, as backgroundWatchRefusal does, and anything that
// cannot be read refuses. A window is a second exception to the background contract (run_mode_doors.go): a
// background run's owner may sit at it, as a takeover, and the rows say so (background: true).
//
// What is open lives in this replica's registry, one window per run. The same rule holds on the runner's side
// (types.MacWindowOpenMsg is both ends' sentence), and the runner holds the stream, so a second replica that
// routes a second grant to it is refused there.

// The words of a window's refusals.
const (
	macWindowSiteOffMsg        = "Opening a window onto a macOS VM run is turned off for this install. An admin can turn it on in Settings, under macOS virtual machines, or set macos_vm.allow_attended_window in the site config."
	macWindowNotMacOSMsg       = "Only a macOS VM run has a window."
	macWindowProfileMsg        = "Your organisation's governance profile doesn't allow an interactive desktop on this run."
	macWindowUnrecordedMsg     = "This run's recording level is Screen and actions, and a window on your Mac isn't recorded, so Wardyn won't open one. Watch its desktop from the run page instead."
	macWindowOfflineMsg        = "This Mac's runner is offline, so Wardyn can't open a window on it."
	macWindowRunnerTooOldMsg   = "This Mac's runner can't open a window. Upgrade it to this server's version."
	macWindowDesktopControlMsg = "A browser desktop that has control of this run didn't close in time, so Wardyn can't open the window. Close it and open the window again."
	macWindowNotHereMsg        = "Wardyn couldn't reach this Mac's runner from here. Try again in a moment."
	macWindowPausedMsg         = "This run is paused. You can open its window once it is running again."
	macWindowPreparingMsg      = "This run is still being prepared; the window opens when preparation finishes."
	macWindowRevokedMsg        = "This session is no longer authorized, so Wardyn won't open a window. Sign in again."
	macWindowFailedMsg         = "Wardyn couldn't open the window on the run's Mac. The run goes on; try again."
	macWindowLockUnknownMsg    = "Wardyn can't tell whether this Mac's screen is locked, so the window stays closed."
)

// macWindowRevocationUnavailableMsg is the refusal of a window whose grant names no token or CLI session, so
// its credential cannot be checked.
const macWindowRevocationUnavailableMsg = "Wardyn can't check the credential this window was asked for with. Open it again with `wardyn window`."

// macWindowNotYourMacMsg is the refusal of a window on a run that is not on a Mac whose runner the caller
// registered, naming the computer it is on.
func macWindowNotYourMacMsg(runner string) string {
	return "Wardyn opens a window only on a Mac whose runner you registered. This run is on " + runner +
		", so Wardyn can't tell who is at its keyboard. Watch its desktop from the run page instead."
}

// The states of GET /runs/{id}'s mac_window. Only macWindowStateOpen shows a button.
const (
	macWindowStateOpen         = "open"
	macWindowStateWindowOpen   = "window_open"
	macWindowStateSiteOff      = "site_off"
	macWindowStateGovernance   = "governance"
	macWindowStateSignInsOff   = "sign_ins_off"
	macWindowStateNotYourMac   = "not_your_mac"
	macWindowStateUnrecorded   = "unrecorded"
	macWindowStateOffline      = "offline"
	macWindowStateTooOld       = "runner_too_old"
	macWindowStateNotHere      = "not_here"
	macWindowStatePaused       = "paused"
	macWindowStatePreparing    = "preparing"
	macWindowStateUnconfirmed  = "unconfirmed"
	macWindowStateNotRunOwner  = "not_run_owner"
	macWindowSurface           = "mac_window"
	macWindowRecheckEvery      = 30 * time.Second
	macWindowOpenWait          = 30 * time.Second
	macWindowTriggerLaunch     = "launch"
	macWindowTriggerCLI        = "cli"
	macWindowTriggerRunPage    = "run_page"
	macWindowRunnerFallbackMsg = "another computer"
)

// The reasons a window ended, as takeover.close writes them.
const (
	macWindowClosedByPerson = "closed_by_person"
	macWindowHostLocked     = "host_locked"
	macWindowRefusedRecheck = "refused_on_recheck"
	macWindowRevoked        = "revoked"
	macWindowRunEnded       = "run_ended"
	macWindowRunnerOffline  = "runner_offline"
	macWindowLinkLost       = "link_lost"
	macWindowHelperFailed   = "helper_failed"
)

// errMacWindowNotAtMac, errMacWindowAlreadyOpen and errMacWindowLockUnknown are the refusals the runner makes that
// the control plane words itself; any other error from macWindowSub.MacWindow is macWindowFailedMsg. The runner's
// adapter maps its codes onto them.
var (
	errMacWindowNotAtMac    = errors.New("mac window: the person is not at the Mac's unlocked screen")
	errMacWindowAlreadyOpen = errors.New("mac window: the runner already holds a window for this run")
	errMacWindowLockUnknown = errors.New("mac window: the Mac can't tell whether its screen is locked")
)

// macWindowSub is a runner's end of the grant. The runner's adapter implements it; the zero state of a runner
// that does not is "can't open a window" (macWindowRunnerTooOldMsg), so an unwired runner refuses.
type macWindowSub interface {
	// MacWindowSupported is the runner's live caps: supported is MacVMCaps.Window, reported is false while no
	// caps have arrived.
	MacWindowSupported() (supported, reported bool)
	// MacWindow asks the runner to put a window on the live VM of the sandbox ref. ctx bounds the open only;
	// the returned session outlives it. It returns errMacWindowNotAtMac when the person's session is not the
	// Mac's unlocked console, errMacWindowLockUnknown when the Mac can't tell whether its screen is locked, and
	// errMacWindowAlreadyOpen when it already holds a window for the run.
	MacWindow(ctx context.Context, ref string, req macWindowRequest) (macWindowSession, error)
}

// macWindowRequest is one open: the grant id, the run's title for the window, and the person it is for.
type macWindowRequest struct {
	GrantID uuid.UUID
	Title   string
	Person  string
}

// macWindowSession is the org's end of an open window.
type macWindowSession interface {
	// Ended is closed once the window has ended on the runner's side.
	Ended() <-chan struct{}
	// EndReason is why, after Ended: closed_by_person, host_locked, helper_failed or link_lost. Anything else
	// reads as link_lost.
	EndReason() string
	// Close ends the window (it resets the stream, and the runner tears the view down). Idempotent.
	Close() error
}

// macWindowSubFor is runner id's window end (the test seam macWindowSubs replaces it). A runner that has not
// reported the window cap reads as unreported (MacWindowSupported).
func (s *Server) macWindowSubFor(id uuid.UUID) (macWindowSub, bool) {
	if s.macWindowSubs != nil {
		sub := s.macWindowSubs(id)
		return sub, sub != nil
	}
	return remoteMacWindowSub{sub: s.runnerEndpoint(id).sub}, true
}

// macWindowTicks is the re-check clock: a channel that ticks every d, and its stop. The test seam macWindowTick
// replaces it, so a test moves the clock by sending on the channel.
func (s *Server) macWindowTicks(d time.Duration) (<-chan time.Time, func()) {
	if s.macWindowTick != nil {
		return s.macWindowTick(d)
	}
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// macWindowAuthority is whom a grant is for and what they signed in with, kept so a re-check can ask whether
// that credential has been cut since.
type macWindowAuthority struct {
	ActorType types.ActorType
	Principal string
	Email     string
	Issue     oidc.Issue
	// Token: an API token or a delegated token admitted the request. A token is ended by a revoke or by its
	// owner's identity, never by a browser-only cut (admittedRevoked's rule).
	Token bool
	// Via: a portal's delegated token admitted the request. A window is never opened through one.
	Via bool
	// TokenID is the personal API token that admitted the request, and CLISessionID the CLI session whose access
	// token did: the credential the window is bound to, ended with it. Only ids, never the secret. A CLI sign-in
	// binds to its session alone, because a refresh replaces its access token and the session lives on. Both are
	// nil on a console session.
	TokenID      uuid.UUID
	CLISessionID uuid.UUID
}

// macWindowAuthorityFrom is the authority of the request ctx carries, admitted at.
func macWindowAuthorityFrom(ctx context.Context, actor types.ActorType, principal string, at time.Time) macWindowAuthority {
	_, via := audit.DelegationFrom(ctx)
	tokenID, sessionID := credentialBinding(ctx)
	return macWindowAuthority{ActorType: actor, Principal: principal, Email: oidcEmailFromContext(ctx),
		Issue: admittedIssue(ctx, principal, at), Token: admittedByToken(ctx), Via: via,
		TokenID: tokenID, CLISessionID: sessionID}
}

// errMacWindowNoBinding is a token authority that names neither a token nor a CLI session, a grant kept before
// the window was bound to its credential: nothing says which credential to ask after, so it cannot be judged.
var errMacWindowNoBinding = errors.New("the window's grant names no token or CLI session")

// macWindowCredentialGone is whether the token or CLI session a's window is bound to has ended: a token revoked or
// expired, a CLI session ended or past its lifetime. A store that cannot say is an error.
func (s *Server) macWindowCredentialGone(ctx context.Context, a macWindowAuthority) (bool, error) {
	switch {
	case a.CLISessionID != uuid.Nil:
		cs, ok := s.cfg.Store.(interface {
			GetCLISession(ctx context.Context, id uuid.UUID) (store.CLISessionRow, error)
		})
		if !ok {
			return false, errors.New("the store keeps no CLI sessions")
		}
		sess, err := cs.GetCLISession(ctx, a.CLISessionID)
		if errors.Is(err, store.ErrNotFound) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return sess.EndedAt != nil || !s.cfg.Now().Before(sess.ExpiresAt), nil
	case a.TokenID != uuid.Nil:
		tr, ok := s.cfg.Store.(store.LiveAPITokenReader)
		if !ok {
			return false, errors.New("the store cannot re-read an API token")
		}
		_, err := tr.GetLiveAPIToken(ctx, a.TokenID)
		if errors.Is(err, store.ErrNotFound) {
			return true, nil
		}
		return false, err
	}
	return false, errMacWindowNoBinding
}

// macWindowRevoked is whether a's credential has been cut since it was issued. A store that cannot answer is an
// error, and the caller refuses: continuing would serve the credential an admin may have just cancelled.
func (s *Server) macWindowRevoked(ctx context.Context, a macWindowAuthority) (bool, error) {
	if a.Token {
		if gone, err := s.macWindowCredentialGone(ctx, a); gone || err != nil {
			return gone, err
		}
	}
	rev := s.cfg.SessionRevocations
	if rev == nil {
		return false, nil
	}
	if a.Token {
		status, err := oidc.CheckSession(ctx, rev, a.Principal, a.Email, a.Issue, -1)
		if err != nil {
			return false, err
		}
		return status != oidc.SessionLive, nil
	}
	return oidc.IsRevoked(ctx, rev, a.Principal, a.Email, a.Issue)
}

// macWindow is one open window on this replica.
type macWindow struct {
	runID      uuid.UUID
	grantID    uuid.UUID
	runnerID   uuid.UUID
	runner     string
	auth       macWindowAuthority
	trigger    string
	background bool
	opened     time.Time

	sess   macWindowSession
	cancel context.CancelFunc
	once   sync.Once
}

// macWindowHost is what the Server holds for windows, embedded so its fields read as the Server's own.
type macWindowHost struct {
	// macWindows is this replica's open attended windows, one per run. Zero value is ready.
	macWindows macWindowRegistry
	// desktopControl is the open browser desktop streams of macOS VM runs that carry the viewer's input, which an
	// opening window ends (macWindowYieldControl).
	desktopControl desktopControlSet
	// macWindowSubs replaces a runner's window end (macWindowSubFor), and macWindowTick the re-check clock
	// (macWindowTicks), tests only.
	macWindowSubs func(id uuid.UUID) macWindowSub
	macWindowTick func(d time.Duration) (<-chan time.Time, func())
}

// macWindowRegistry is this replica's open windows, one per run.
type macWindowRegistry struct {
	mu    sync.Mutex
	byRun map[uuid.UUID]*macWindow
}

// reserve puts w in the registry and says false, leaving it as it was, when the run already has a window.
func (g *macWindowRegistry) reserve(w *macWindow) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.byRun == nil {
		g.byRun = map[uuid.UUID]*macWindow{}
	}
	if _, held := g.byRun[w.runID]; held {
		return false
	}
	g.byRun[w.runID] = w
	return true
}

// release drops w, and only w: a later window for the run stays.
func (g *macWindowRegistry) release(w *macWindow) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.byRun[w.runID] == w {
		delete(g.byRun, w.runID)
	}
}

// open is the run's window on this replica, nil for none.
func (g *macWindowRegistry) open(runID uuid.UUID) *macWindow {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.byRun[runID]
}

// macWindowAsk says which rows macWindowRefusal skips: reopen is the re-check of a window that is itself the
// one open (the registry row would refuse it).
type macWindowAsk struct {
	reopen bool
}

// macWindowRunnerStore is the store's read of a registered runner.
type macWindowRunnerStore interface {
	GetRunner(ctx context.Context, id uuid.UUID) (types.Runner, error)
}

// macWindowSiteRefusal is row 1: the site switch, off when absent. An unreadable site config refuses too.
func (s *Server) macWindowSiteRefusal(ctx context.Context) *runRefusal {
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return runServerError("read site config", err)
	}
	if !sc.MacOSVM.AttendedWindowAllowed() {
		return runError(http.StatusForbidden, reasonMacWindowSiteOff, macWindowSiteOffMsg)
	}
	return nil
}

// macWindowGovernanceRefusal is row 4, on c's limits and the unit that set them (nil at create for a caller
// with none, whose refusal names the deployment's policy): neither deny_ui_apps nor deny_interactive, no role
// exempt, a super admin's own run included. runID is uuid.Nil at create, before the run has one.
func (s *Server) macWindowGovernanceRefusal(ctx context.Context, c governanceCeiling, runID uuid.UUID) *runRefusal {
	if !c.Limits.DenyUIApps && !c.Limits.DenyInteractive {
		return nil
	}
	d := authz.Deny(authz.ReasonGovernanceUnit, "runs.desktop_window", macWindowProfileMsg).WithPolicy(s.ceilingPolicy(ctx, c))
	if runID != uuid.Nil {
		d = d.OnRun(runID)
	}
	if c.Unit != nil {
		knob := "deny_interactive"
		if c.Limits.DenyUIApps {
			knob = "deny_ui_apps"
		}
		d = d.OnKnob(s.runLockOf(ctx, c.Unit, knob))
	}
	return runDenied(d)
}

// macWindowNotYourMac is row 3's refusal, naming the computer ("another computer" when it is not known).
func macWindowNotYourMac(runner string) *runRefusal {
	if runner == "" {
		runner = macWindowRunnerFallbackMsg
	}
	return runError(http.StatusForbidden, reasonMacWindowNotYourMac, macWindowNotYourMacMsg(runner))
}

// macRunPreparing says the run is still being prepared: its sandbox is not up (STARTING), or it is an
// interactive macOS VM run whose prep (repo clone, harness setup) has not recorded done.
func macRunPreparing(run types.AgentRun) bool {
	return run.State == types.RunStarting || (run.Interactive && run.MachineKind.IsMacOSVM() && !macPrepDone(run))
}

// macWindowOwnRunner is row 3: strictly the caller's own runner, by local attach's rule (localAttachCheck) and
// one more. The caller must be a human, with a principal, in no local mode, and the run's creator, on a run in
// no operator's name, placed local on a claimed runner registered to that person as its owner; and the request
// must not come through a portal's delegated token. A service identity, a system or agent actor, a delegate
// and an operator-owned run are all refused here. A runner that cannot be read refuses. On a refusal it returns
// the runner row it read, if any.
func (s *Server) macWindowOwnRunner(ctx context.Context, run types.AgentRun, a macWindowAuthority) (types.Runner, *runRefusal) {
	if s.cfg.LocalMode || run.OperatorOwned || a.ActorType != types.ActorHuman || a.Principal == "" || a.Via ||
		a.Principal != run.CreatedBy || run.Placement != types.PlacementLocal || run.RunnerID == nil {
		return types.Runner{}, macWindowNotYourMac("")
	}
	rs, ok := s.cfg.Store.(macWindowRunnerStore)
	if !ok {
		return types.Runner{}, macWindowNotYourMac("")
	}
	rn, err := rs.GetRunner(ctx, *run.RunnerID)
	if errors.Is(err, store.ErrNotFound) {
		return types.Runner{}, macWindowNotYourMac("")
	}
	if err != nil {
		return types.Runner{}, runServerError("read the run's runner", err)
	}
	if rn.State != types.RunnerClaimed || rn.OwnerKind != types.RunnerOwnerPerson || rn.Owner != a.Principal {
		return rn, macWindowNotYourMac(rn.Name)
	}
	return rn, nil
}

// macWindowRefusal is nil only when a window may open on run for principal right now. The rows, in order:
//
//	1  the site switch macos_vm.allow_attended_window is on (absent is off)
//	2  a macOS VM run, running (or starting: the preparing row names it), not kept
//	3  the caller's own runner, strictly: local attach's rule plus no delegated token (macWindowOwnRunner)
//	4  the run's profile allows an interactive desktop (deny_ui_apps, deny_interactive)
//	5  the run's recording level is not Screen and actions
//	6  the agent's desktop sign-ins are allowed
//	7  the runner is connected (to this replica) and can open a window
//	8  not paused
//	9  not still being prepared
//	10 no window already open on this run (this replica's registry)
//
// One keyboard at a time is not a row: while a window is open the run's browser desktop is forced view only
// (macOSDesktopViewOnly), and the open ends or waits out every browser session that has control first
// (macWindowYieldControl). The runner's own check, that the person is at the Mac's unlocked screen, comes after,
// in the open. The rows write nothing, so a read may ask them.
func (s *Server) macWindowRefusal(ctx context.Context, run types.AgentRun, a macWindowAuthority, ask macWindowAsk) *runRefusal {
	_, rf := s.macWindowCheck(ctx, run, a, ask)
	return rf
}

// macWindowCheck is macWindowRefusal that also returns the run's runner row, which is the caller's own when the
// answer is nil.
func (s *Server) macWindowCheck(ctx context.Context, run types.AgentRun, a macWindowAuthority, ask macWindowAsk) (types.Runner, *runRefusal) {
	if rf := s.macWindowSiteRefusal(ctx); rf != nil {
		return types.Runner{}, rf
	}
	if !run.MachineKind.IsMacOSVM() {
		return types.Runner{}, runError(http.StatusConflict, reasonMacWindowNotMacOSVM, macWindowNotMacOSMsg)
	}
	if run.State != types.RunRunning && run.State != types.RunStarting {
		return types.Runner{}, runError(http.StatusConflict, reasonAttachNotRunning, "run is not RUNNING; cannot open its window (state="+string(run.State)+")")
	}
	if runIsKept(run) {
		return types.Runner{}, runError(http.StatusConflict, reasonAttachRunKept, "run has ended; cannot open its window")
	}
	rn, rf := s.macWindowOwnRunner(ctx, run, a)
	if rf != nil {
		return rn, rf
	}
	p, err := s.runUnit(ctx, run)
	if err != nil {
		if u, ok := isOverlayUnsatisfiable(err); ok {
			return rn, runDenied(authz.Deny(authz.ReasonGovernanceOverlayUnsatisfiable, "runs.desktop_window", u.Error()).OnRun(run.ID))
		}
		return rn, runServerError("read the run's governance profile", err)
	}
	if p != nil {
		if rf := s.macWindowGovernanceRefusal(ctx, governanceCeiling{Limits: p.Limits, Unit: p}, run.ID); rf != nil {
			return rn, rf
		}
	}
	if run.RecordingLevel == types.RecordingScreenActions {
		return rn, runError(http.StatusForbidden, reasonMacWindowUnrecorded, macWindowUnrecordedMsg)
	}
	if s.macOSDesktopSignInsOff(ctx, run) {
		return rn, runError(http.StatusForbidden, reasonDesktopSignInsOff, macOSDesktopSignInsOffMsg(run.Agent))
	}
	if _, connected := s.macOSLiveDesktop(run); !connected {
		if s.runnerOnline(rn.ID) {
			// Online somewhere, but its stream is not on this replica: nothing routes the call there.
			return rn, runError(http.StatusConflict, reasonMacWindowNotHere, macWindowNotHereMsg)
		}
		return rn, runError(http.StatusConflict, reasonMacWindowOffline, macWindowOfflineMsg)
	}
	sub, wired := s.macWindowSubFor(rn.ID)
	supported, reported := false, false
	if wired {
		supported, reported = sub.MacWindowSupported()
	}
	switch {
	case wired && !reported:
		return rn, runError(http.StatusConflict, reasonMacWindowOffline, macWindowOfflineMsg)
	case !wired || !supported:
		return rn, runError(http.StatusConflict, reasonMacWindowRunnerTooOld, macWindowRunnerTooOldMsg)
	}
	if run.PausedAt != nil {
		return rn, runError(http.StatusConflict, reasonMacWindowPaused, macWindowPausedMsg)
	}
	if macRunPreparing(run) {
		return rn, runError(http.StatusConflict, reasonMacWindowPreparing, macWindowPreparingMsg)
	}
	if rf := s.macGUIEnvRefusal(ctx, run, ask.reopen); rf != nil { // the settings are still being applied to the desktop
		return rn, rf
	}
	if !ask.reopen && s.macWindows.open(run.ID) != nil {
		return rn, runError(http.StatusConflict, reasonMacWindowOpen, types.MacWindowOpenMsg(rn.Name))
	}
	return rn, nil
}

// macWindowRefusalReason is a refusal's audit reason.
func macWindowRefusalReason(rf *runRefusal) string {
	switch {
	case rf.err != nil:
		return reasonRunUnreadable
	case rf.decision != nil:
		return string(rf.decision.Reason)
	}
	return rf.body.Reason
}

// macWindowRefusalSentence is the sentence a refusal answers with; "" for a read that failed.
func macWindowRefusalSentence(rf *runRefusal) string {
	switch {
	case rf.err != nil:
		return ""
	case rf.decision != nil:
		ref, _ := authz.Lookup(rf.decision.Reason)
		return cmp.Or(rf.decision.Sentence, ref.Sentence)
	}
	return rf.body.Error
}

// macWindowData is the fields every takeover row of a window carries.
func macWindowData(background bool, trigger string, runnerID *uuid.UUID) map[string]any {
	data := map[string]any{"surface": macWindowSurface, "trigger": trigger, "background": background, "recorded": false}
	if runnerID != nil {
		data["runner_id"] = runnerID.String()
	}
	return data
}

// auditMacWindowBegin writes takeover.begin: success with the grant as the target, or denied with the run (or
// nothing, at create) as the target and the refusal's reason. A window is a takeover of the run by its person;
// a consumer that joins this row to an approval reads surface first.
func (s *Server) auditMacWindowBegin(ctx context.Context, runID *uuid.UUID, a macWindowAuthority, target, outcome string, data map[string]any) {
	s.recordAudit(ctx, s.auditEvent(runID, a.ActorType, a.Principal, "takeover.begin", target, outcome, mustJSON(data)))
}

// auditMacWindowDenied writes the denied takeover.begin row for refusal rf.
func (s *Server) auditMacWindowDenied(ctx context.Context, run types.AgentRun, a macWindowAuthority, trigger string, rf *runRefusal) {
	data := macWindowData(run.BackgroundOnly(), trigger, run.RunnerID)
	data["reason"] = macWindowRefusalReason(rf)
	s.auditMacWindowBegin(ctx, &run.ID, a, run.ID.String(), "denied", data)
}

// auditMacWindowClose writes takeover.close: why the window ended, and for how long it was open. by names the
// check that refused when a re-check ended it.
func (s *Server) auditMacWindowClose(ctx context.Context, w *macWindow, reason, by string) {
	data := macWindowData(w.background, w.trigger, &w.runnerID)
	data["grant_id"] = w.grantID.String()
	data["held_ms"] = s.cfg.Now().Sub(w.opened).Milliseconds()
	data["reason"] = reason
	if by != "" {
		data["refused_by"] = by
	}
	s.recordAudit(ctx, s.auditEvent(&w.runID, w.auth.ActorType, w.auth.Principal, "takeover.close", w.grantID.String(), "success", mustJSON(data)))
}

// openMacWindow asks for a window on run for the person a stands for. Every refusal writes a denied
// takeover.begin; a window that opens writes takeover.begin and is watched until it ends. The registry slot is
// taken before anything else moves, so two opens at once cannot both reach the runner, and so the run's browser
// desktop is already forced view only (macOSDesktopViewOnly) while it yields. The runner is told to open only
// after every browser desktop session that had control is closed (macWindowYieldControl).
func (s *Server) openMacWindow(ctx context.Context, run types.AgentRun, a macWindowAuthority, trigger string) (*macWindow, *runRefusal) {
	refuse := func(rf *runRefusal) (*macWindow, *runRefusal) {
		s.auditMacWindowDenied(ctx, run, a, trigger, rf)
		return nil, rf
	}
	rn, rf := s.macWindowCheck(ctx, run, a, macWindowAsk{})
	if rf != nil {
		return refuse(rf)
	}
	revoked, err := s.macWindowRevoked(ctx, a)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: could not check a window's credential", "run_id", run.ID, "err", err)
		if errors.Is(err, errMacWindowNoBinding) {
			return refuse(runError(http.StatusForbidden, uiDeniedReasonRevocationUnavailable, macWindowRevocationUnavailableMsg))
		}
		return refuse(runServerError("check the session", err))
	}
	if revoked {
		return refuse(runError(http.StatusForbidden, reasonMacWindowRevoked, macWindowRevokedMsg))
	}
	sub, ok := s.macWindowSubFor(rn.ID)
	if !ok {
		return refuse(runError(http.StatusConflict, reasonMacWindowRunnerTooOld, macWindowRunnerTooOldMsg))
	}
	wctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	w := &macWindow{runID: run.ID, grantID: uuid.New(), runnerID: rn.ID, runner: rn.Name, auth: a, trigger: trigger,
		background: run.BackgroundOnly(), opened: s.cfg.Now(), cancel: cancel}
	if !s.macWindows.reserve(w) {
		cancel()
		return refuse(runError(http.StatusConflict, reasonMacWindowOpen, types.MacWindowOpenMsg(rn.Name)))
	}
	fail := func(rf *runRefusal) (*macWindow, *runRefusal) {
		s.macWindows.release(w)
		cancel()
		return refuse(rf)
	}
	if !s.macWindowYieldControl(ctx, run.ID) {
		return fail(runError(http.StatusConflict, reasonMacWindowDesktopControl, macWindowDesktopControlMsg))
	}
	title := run.Title
	if title == "" {
		title = "Run " + run.ID.String()[:8]
	}
	octx, ocancel := context.WithTimeout(ctx, macWindowOpenWait)
	sess, err := sub.MacWindow(octx, run.SandboxRef, macWindowRequest{GrantID: w.grantID, Title: title, Person: a.Principal})
	ocancel()
	switch {
	case errors.Is(err, errMacWindowNotAtMac):
		return fail(runError(http.StatusConflict, reasonMacWindowNotAtMac, types.MacWindowNotAtMacMsg(rn.Name)))
	case errors.Is(err, errMacWindowLockUnknown):
		return fail(runError(http.StatusConflict, reasonMacWindowLockUnknown, macWindowLockUnknownMsg))
	case errors.Is(err, errMacWindowAlreadyOpen):
		return fail(runError(http.StatusConflict, reasonMacWindowOpen, types.MacWindowOpenMsg(rn.Name)))
	case err != nil || sess == nil:
		slog.WarnContext(ctx, "wardynd: the runner could not open a window", "run_id", run.ID, "err", err)
		return fail(runError(http.StatusBadGateway, reasonMacWindowFailed, macWindowFailedMsg))
	}
	w.sess = sess
	data := macWindowData(w.background, trigger, &w.runnerID)
	data["grant_id"] = w.grantID.String()
	s.auditMacWindowBegin(ctx, &run.ID, a, w.grantID.String(), "success", data)
	_ = s.cfg.Store.TouchRun(ctx, run.ID) // an error only leaves the run to the reaper, as in attachKeepalive
	s.goBackground(func() { s.watchMacWindow(wctx, w) })
	return w, nil
}

// errMacWindowTookControl is the cause a window that opens ends a browser desktop stream with control by, so
// the stream tells its viewer (desktopCloseWindowOpen) rather than drop it.
var errMacWindowTookControl = errors.New("a window opened on the Mac")

// desktopControlSet is the open browser desktop streams of macOS VM runs that carry the viewer's input, by run:
// the streams a window must wait out. A view-only stream is not in it.
type desktopControlSet struct {
	mu    sync.Mutex
	next  uint64
	byRun map[uuid.UUID]map[uint64]context.CancelFunc
}

// track registers a stream that has control of run, cancelled by end; the returned func forgets it.
func (d *desktopControlSet) track(runID uuid.UUID, cancel context.CancelFunc) func() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.byRun == nil {
		d.byRun = map[uuid.UUID]map[uint64]context.CancelFunc{}
	}
	if d.byRun[runID] == nil {
		d.byRun[runID] = map[uint64]context.CancelFunc{}
	}
	d.next++
	id := d.next
	d.byRun[runID][id] = cancel
	return func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		delete(d.byRun[runID], id)
		if len(d.byRun[runID]) == 0 {
			delete(d.byRun, runID)
		}
	}
}

// end cancels every stream with control of run, and says how many are left.
func (d *desktopControlSet) end(runID uuid.UUID) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, cancel := range d.byRun[runID] {
		cancel()
	}
	return len(d.byRun[runID])
}

// macWindowYieldWait bounds how long an open waits for the run's browser desktop streams to close (a variable
// so a test can shorten it).
var macWindowYieldWait = 5 * time.Second

// macWindowYieldControl makes the run's browser desktop view only before a window opens: the window is already
// in the registry, so a stream opened from now on is view only, and every stream that had control is ended now
// rather than at its next re-check: its viewer is told a window is open and opens the desktop again from the run
// page, view only. True once none is left; a bounded wait that runs out is false, and the open refuses.
func (s *Server) macWindowYieldControl(ctx context.Context, runID uuid.UUID) bool {
	deadline := time.Now().Add(macWindowYieldWait)
	for s.desktopControl.end(runID) > 0 {
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

// closeMacWindow ends w once, whoever asks first: it closes the runner's session (which tears the view down),
// frees the registry slot and writes takeover.close with reason.
func (s *Server) closeMacWindow(ctx context.Context, w *macWindow, reason, by string) {
	w.once.Do(func() {
		if err := w.sess.Close(); err != nil {
			slog.WarnContext(ctx, "wardynd: closing a window's session failed", "run_id", w.runID, "err", err)
		}
		s.macWindows.release(w)
		w.cancel()
		s.auditMacWindowClose(context.WithoutCancel(ctx), w, reason, by)
		s.macWindowStoreLastClose(context.WithoutCancel(ctx), w.auth.Principal, w.runID, macWindowLastClose{Reason: macWindowLastCloseReason(reason)})
	})
}

// The last-close reasons GET /runs/{id}'s mac_window.last_close carries, for the console.
const (
	macWindowEndedPerson     = "person"
	macWindowEndedHostLocked = "host_locked"
	macWindowEndedRunEnded   = "run_ended"
	macWindowEndedFailed     = "failed"
	// macWindowEndedNotOpened is the window a create asked for that was refused as the run started.
	macWindowEndedNotOpened = "not_opened"
)

// macWindowLastClose is why a run's last window closed and when, kept per person and run so any replica can
// answer it. The reason is the console's words, not the audit row's: every close that is not the person's
// own, the Mac locking or the run ending reads as failed. Message is the refusal's sentence of a not_opened.
type macWindowLastClose struct {
	Reason  string    `json:"reason"`
	Message string    `json:"message,omitempty"`
	At      time.Time `json:"at"`
}

const macWindowLastCloseKey = "mac_window_last:"

// macWindowLastCloseReason maps a takeover.close reason onto the console's.
func macWindowLastCloseReason(reason string) string {
	switch reason {
	case macWindowClosedByPerson:
		return macWindowEndedPerson
	case macWindowHostLocked:
		return macWindowEndedHostLocked
	case macWindowRunEnded:
		return macWindowEndedRunEnded
	}
	return macWindowEndedFailed
}

// macWindowStoreLastClose records c, stamped now, as principal's last close on run for the run read. A failed
// write loses only the console's line; the audit row is already written.
func (s *Server) macWindowStoreLastClose(ctx context.Context, principal string, runID uuid.UUID, c macWindowLastClose) {
	st, ok := s.cfg.Store.(store.PrincipalPrefStore)
	if !ok {
		return
	}
	c.At = s.cfg.Now()
	raw, err := json.Marshal(c)
	if err == nil {
		err = st.PutPrincipalPref(ctx, principal, macWindowLastCloseKey+runID.String(), raw)
	}
	if err != nil {
		slog.WarnContext(ctx, "wardynd: recording a window's close failed", "run_id", runID, "err", err)
	}
}

// macWindowLoadLastClose is the person's last window close on run, nil when there is none or it can't be read.
func (s *Server) macWindowLoadLastClose(ctx context.Context, runID uuid.UUID, principal string) *macWindowLastClose {
	st, ok := s.cfg.Store.(store.PrincipalPrefStore)
	if !ok {
		return nil
	}
	raw, err := st.GetPrincipalPref(ctx, principal, macWindowLastCloseKey+runID.String())
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.WarnContext(ctx, "wardynd: reading a window's last close failed", "run_id", runID, "err", err)
		}
		return nil
	}
	var c macWindowLastClose
	if json.Unmarshal(raw, &c) != nil || c.Reason == "" {
		return nil
	}
	return &c
}

// macWindowEndReason is the close reason of a window the runner ended.
func macWindowEndReason(sess macWindowSession) string {
	switch r := sess.EndReason(); r {
	case macWindowClosedByPerson, macWindowHostLocked, macWindowHelperFailed, macWindowLinkLost, macWindowRunEnded:
		return r
	}
	return macWindowLinkLost // including the runner's helper_lost
}

// watchMacWindow holds w until it ends: the runner ends it, wardynd is going down, or a re-check, every
// macWindowRecheckEvery, finds it no longer allowed.
func (s *Server) watchMacWindow(ctx context.Context, w *macWindow) {
	tick, stop := s.macWindowTicks(macWindowRecheckEvery)
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
			s.closeMacWindow(ctx, w, macWindowEndReason(w.sess), "")
			return
		case <-down:
			s.closeMacWindow(ctx, w, macWindowLinkLost, "")
			return
		case <-tick:
			if reason, by := s.macWindowRecheck(ctx, w); reason != "" {
				s.closeMacWindow(ctx, w, reason, by)
				return
			}
		}
	}
}

// macWindowRecheck asks again what the open asked: the run as it stands now, the person's credential and the
// rows of macWindowRefusal, with the same own-runner rule (no delegate, no service identity, no operator-owned
// run). It returns the close reason, "" while the window may stay, and the check that refused. Anything it
// cannot read ends the window (fail closed). A window that stays stamps the run's presence and touches the run,
// so an open window keeps the run from idling out from under the person. A window that ends touches nothing.
func (s *Server) macWindowRecheck(ctx context.Context, w *macWindow) (reason, by string) {
	run, err := s.cfg.Store.GetRun(ctx, w.runID)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: could not re-check a window", "run_id", w.runID, "err", err)
		return macWindowRefusedRecheck, reasonRunUnreadable
	}
	if isTerminalRunState(run.State) || runIsKept(run) {
		return macWindowRunEnded, ""
	}
	revoked, err := s.macWindowRevoked(ctx, w.auth)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: could not re-check a window's credential", "run_id", w.runID, "err", err)
		return macWindowRefusedRecheck, uiDeniedReasonRevocationUnavailable
	}
	if revoked {
		return macWindowRevoked, ""
	}
	if run.RunnerID == nil || *run.RunnerID != w.runnerID {
		return macWindowRefusedRecheck, reasonMacWindowNotYourMac
	}
	rf := s.macWindowRefusal(ctx, run, w.auth, macWindowAsk{reopen: true})
	switch {
	case rf == nil:
		_, _ = s.stampPresence(ctx, w.runID, types.ActorHuman, w.auth.Principal, macWindowSurface)
		_ = s.cfg.Store.TouchRun(ctx, w.runID) // as attachKeepalive: an error only leaves the run to the reaper
		return "", ""
	case rf.err == nil && rf.decision == nil && (rf.body.Reason == reasonMacWindowOffline || rf.body.Reason == reasonMacWindowNotHere):
		return macWindowRunnerOffline, rf.body.Reason
	}
	return macWindowRefusedRecheck, macWindowRefusalReason(rf)
}

// macWindowView is what GET /runs/{id} says of a window onto the run.
type macWindowView struct {
	// State is one of the macWindowState* states. Only macWindowStateOpen shows a button.
	State string `json:"state"`
	// Message is the refusal's sentence: absent when open, and for a caller who isn't the run's owner.
	Message string `json:"message,omitempty"`
	// LastClose is why this person's last window on the run closed, absent when none has.
	LastClose *macWindowLastClose `json:"last_close,omitempty"`
}

// macWindowStateOf names a refusal for GET /runs/{id}: its state and its sentence. A read that failed is
// unconfirmed, as anything unknown is.
func macWindowStateOf(rf *runRefusal) macWindowView {
	switch {
	case rf == nil:
		return macWindowView{State: macWindowStateOpen}
	case rf.err != nil:
		return macWindowView{State: macWindowStateUnconfirmed}
	case rf.decision != nil:
		return macWindowView{State: macWindowStateGovernance, Message: macWindowProfileMsg}
	}
	state := macWindowStateUnconfirmed
	switch rf.body.Reason {
	case reasonMacWindowSiteOff:
		state = macWindowStateSiteOff
	case reasonMacWindowNotYourMac:
		state = macWindowStateNotYourMac
	case reasonMacWindowUnrecorded:
		state = macWindowStateUnrecorded
	case reasonDesktopSignInsOff:
		state = macWindowStateSignInsOff
	case reasonMacWindowOffline:
		state = macWindowStateOffline
	case reasonMacWindowRunnerTooOld:
		state = macWindowStateTooOld
	case reasonMacWindowNotHere:
		state = macWindowStateNotHere
	case reasonMacWindowPaused:
		state = macWindowStatePaused
	case reasonMacWindowPreparing:
		state = macWindowStatePreparing
	case reasonMacWindowOpen:
		state = macWindowStateWindowOpen
	}
	return macWindowView{State: state, Message: rf.body.Error}
}

// macWindowViewFor is GET /runs/{id}'s mac_window for the caller a: nil on any run but a live macOS VM
// run.
func (s *Server) macWindowViewFor(ctx context.Context, run types.AgentRun, a macWindowAuthority) *macWindowView {
	if !run.MachineKind.IsMacOSVM() || (run.State != types.RunRunning && run.State != types.RunStarting) || runIsKept(run) {
		return nil
	}
	if !mayEnterRun(run, a.Principal, false) {
		return &macWindowView{State: macWindowStateNotRunOwner}
	}
	rf := s.macWindowRefusal(ctx, run, a, macWindowAsk{})
	if rf != nil && rf.err != nil {
		slog.WarnContext(ctx, "wardynd: could not read whether a window may open", "run_id", run.ID, "err", rf.err)
	}
	v := macWindowStateOf(rf)
	s.macGUIEnvNote(ctx, run, &v)
	v.LastClose = s.macWindowLoadLastClose(ctx, run.ID, a.Principal)
	return &v
}

// mountMacWindowRoutes registers the window's route on the authenticated group.
func (s *Server) mountMacWindowRoutes(r chi.Router) {
	r.Post("/runs/{id}/desktop/window", s.handleMacWindow)
}

// macWindowBody is the optional body of POST /runs/{id}/desktop/window.
type macWindowBody struct {
	// Trigger is where the ask came from: "cli" (the default) or "run_page".
	Trigger string `json:"trigger,omitempty"`
}

// macWindowGrant is the answer of an open: the grant and where the window is.
type macWindowGrant struct {
	GrantID  uuid.UUID `json:"grant_id"`
	RunnerID uuid.UUID `json:"runner_id"`
	Runner   string    `json:"runner,omitempty"`
	Trigger  string    `json:"trigger"`
}

// handleMacWindow asks for a window onto a macOS VM run on the caller's own Mac:
//
//	POST /api/v1/runs/{id}/desktop/window
//
// The run's owner only, never a super admin or a sponsor, and only on a runner the caller registered
// (macWindowRefusal). It answers once the window is up on the Mac; a refusal answers with its sentence and
// writes a denied takeover.begin. A foreign run answers as every entry route does (getRunForEntry).
func (s *Server) handleMacWindow(w http.ResponseWriter, r *http.Request) {
	authorizedAt := s.cfg.Now().UTC()
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunForEntry(w, r, id)
	if !ok {
		return
	}
	var body macWindowBody
	if err := types.DecodeOneJSON(http.MaxBytesReader(w, r.Body, 4096), &body, false); err != nil && !errors.Is(err, io.EOF) {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, "invalid request body")
		return
	}
	trigger := body.Trigger
	switch trigger {
	case "":
		trigger = macWindowTriggerCLI
	case macWindowTriggerCLI, macWindowTriggerRunPage:
	default:
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, `trigger must be "cli" or "run_page"`)
		return
	}
	at, principal := actorFromRequest(r)
	win, rf := s.openMacWindow(r.Context(), run, macWindowAuthorityFrom(r.Context(), at, principal, authorizedAt), trigger)
	if rf.write(s, w, r) {
		return
	}
	writeJSON(w, http.StatusOK, macWindowGrant{GrantID: win.grantID, RunnerID: win.runnerID, Runner: win.runner, Trigger: trigger})
}

// macWindowAuthorityOf is the authority of the request r, admitted now.
func (s *Server) macWindowAuthorityOf(r *http.Request) macWindowAuthority {
	at, principal := actorFromRequest(r)
	return macWindowAuthorityFrom(r.Context(), at, principal, s.cfg.Now().UTC())
}

// auditMacWindowUnexpected records a window the run's helper reported open for which the runner holds no grant
// this control plane issued (the runner's mac_window_unexpected report): a host process of the runner's user
// reached the helper directly. The row is takeover.begin with the surface and the helper's grant text clamped,
// and the reason mac_window_unexpected. Its outcome is denied, the one of the audit table's three that says
// nothing was granted; the table does not take a fourth.
func (s *Server) auditMacWindowUnexpected(ctx context.Context, runnerID uuid.UUID, runID *uuid.UUID, grant string) {
	if len(grant) > 64 {
		grant = grant[:64]
	}
	data := map[string]any{"surface": macWindowSurface, "reason": "mac_window_unexpected", "recorded": false, "runner_id": runnerID.String()}
	if grant != "" {
		data["grant_id"] = grant
	}
	s.recordAudit(ctx, s.auditEvent(runID, types.ActorSystem, "runner:"+runnerID.String(), "takeover.begin", runnerID.String(), "denied", mustJSON(data)))
}
