// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Watching a background macOS run's desktop: the one exception to the background contract
// (run_mode_doors.go). A person who owns the run sees its screen and nothing else moves: no input, no
// clipboard either way, no wake, no keepalive. Three independent things hold that: the helper's forced filter
// in this boot (AgentRun.DesktopForcedViewOnly), the Mac's caps as its connected runner states them, and the
// gateway's own filter, pinned by the session's signed ViewOnly (desktopClipboardFilter).
//
// backgroundWatchRefusal is asked at the mint, the enter, the stream's first check and every re-check
// (uiSessionStillAuthorized through desktopRunCheck, each uiDesktopRecheck), so an open view ends within that
// interval of any answer changing. A run's stored experience is written once at create and doesn't change
// while it lives; a session marked as a watch on a run that isn't background is refused all the same.

// The words of a watch's refusals, and of a watch a re-check ended.
const (
	backgroundWatchEndedMsg       = "Watching this run's desktop is no longer allowed, so this view closed. Closing this view does not stop the run."
	backgroundWatchProfileMsg     = "Your organisation's governance profile doesn't allow watching a background run's desktop."
	backgroundWatchOldBootMsg     = "This run can't be watched. It started before this Mac's runner could keep a background desktop view only."
	backgroundWatchUnconfirmedMsg = "Wardyn can't confirm this desktop is view only, so it can't be watched."
	backgroundWatchOfflineMsg     = "This Mac's runner is offline, so Wardyn can't confirm the desktop is view only. Wardyn checks again when it reconnects."
	backgroundWatchControlMsg     = "This run is a background task on a Mac that allows desktop control, so its desktop can't be watched. Its logs, audit trail and status stay available."
	backgroundWatchPausedMsg      = "This run is paused. You can watch it once it is running again."
)

// The states of GET /runs/{id}'s background_watch. Only backgroundWatchOpen shows a button.
const (
	backgroundWatchOpen        = "open"
	backgroundWatchSiteOff     = "site_off"
	backgroundWatchGovernance  = "governance"
	backgroundWatchSignInsOff  = "sign_ins_off"
	backgroundWatchOldBoot     = "old_boot"
	backgroundWatchUnconfirmed = "unconfirmed"
	backgroundWatchOffline     = "offline"
	backgroundWatchControl     = "control"
	backgroundWatchPaused      = "paused"
	backgroundWatchNotRunOwner = "not_run_owner"
)

// backgroundWatchView is what GET /runs/{id} says of watching a background macOS run's desktop, computed by
// the door's own function so the console's button can't disagree with it.
type backgroundWatchView struct {
	// State is one of the backgroundWatch* states above.
	State string `json:"state"`
	// Message is the refusal's sentence: absent when open, and for a caller who isn't the run's owner.
	Message string `json:"message,omitempty"`
}

// backgroundWatchRefusal is nil only when run's desktop may be watched right now: a background macOS VM run,
// allowed by the site and by every limit of its profile as it stands now, whose helper reported this boot's
// desktop forced view only, on a connected runner whose caps say observe, and not paused. Anything unknown
// refuses. The caller is the door's to ask (mayEnterRun). It writes nothing, so a read may ask it.
//
// The order puts what no one can wait out first, so a sentence never promises what a later check would refuse.
func (s *Server) backgroundWatchRefusal(ctx context.Context, run types.AgentRun) *runRefusal {
	if !run.BackgroundOnly() {
		return runError(http.StatusConflict, reasonRunBackgroundWatchNotBackground, "This run isn't a background task. Open its desktop from the run page.")
	}
	if !run.MachineKind.IsMacOSVM() {
		return backgroundOnlyRefusal(run, "desktop")
	}
	if rf := s.backgroundWatchLimitRefusal(ctx, run); rf != nil {
		return rf
	}
	if rf := s.backgroundWatchProfileRefusal(ctx, run); rf != nil {
		return rf
	}
	if s.macOSDesktopSignInsOff(ctx, run) {
		return runError(http.StatusForbidden, reasonDesktopSignInsOff, macOSDesktopSignInsOffMsg(run.Agent))
	}
	if !run.DesktopForcedViewOnly() {
		if run.VMMachine != nil && !run.VMMachine.DesktopViewOnlyAsked {
			return runError(http.StatusConflict, reasonRunBackgroundWatchOldBoot, backgroundWatchOldBootMsg)
		}
		return runError(http.StatusConflict, reasonRunBackgroundWatchUnconfirmed, backgroundWatchUnconfirmedMsg)
	}
	switch offer, connected := s.macOSLiveDesktop(run); {
	case !connected:
		return runError(http.StatusConflict, reasonRunBackgroundWatchOffline, backgroundWatchOfflineMsg)
	case offer == runnerwire.MacVMDesktopControl:
		return runError(http.StatusConflict, reasonRunBackgroundWatchControl, backgroundWatchControlMsg)
	case offer != runnerwire.MacVMDesktopObserve:
		return runError(http.StatusConflict, reasonRunBackgroundWatchUnconfirmed, backgroundWatchUnconfirmedMsg)
	}
	if run.PausedAt != nil {
		return runError(http.StatusConflict, reasonRunBackgroundWatchPaused, backgroundWatchPausedMsg)
	}
	return nil
}

// backgroundWatchProfileRefusal is the two limits that close every desktop (uiSessionUnitDenies), which close a
// watch too, with no role exempt: a watch's ticket is member-stamped.
func (s *Server) backgroundWatchProfileRefusal(ctx context.Context, run types.AgentRun) *runRefusal {
	p, err := s.runUnit(ctx, run)
	if err != nil {
		return runServerError("read the run's governance profile", err)
	}
	if p == nil || (!p.Limits.DenyUIApps && !p.Limits.DenyInteractive) {
		return nil
	}
	knob := "deny_interactive"
	if p.Limits.DenyUIApps {
		knob = "deny_ui_apps"
	}
	return runDenied(authz.Deny(authz.ReasonGovernanceUnit, "runs.desktop_watch", backgroundWatchProfileMsg).
		OnRun(run.ID).WithPolicy(s.unitRefWithSiteHelp(ctx, p)).OnKnob(s.runLockOf(ctx, p, knob)))
}

// backgroundWatchStateOf names a refusal for GET /runs/{id}: its state and its sentence. A read that failed is
// unconfirmed, as anything unknown is.
func backgroundWatchStateOf(rf *runRefusal) backgroundWatchView {
	switch {
	case rf == nil:
		return backgroundWatchView{State: backgroundWatchOpen}
	case rf.err != nil:
		return backgroundWatchView{State: backgroundWatchUnconfirmed, Message: backgroundWatchUnconfirmedMsg}
	case rf.decision != nil:
		return backgroundWatchView{State: backgroundWatchGovernance, Message: backgroundWatchProfileMsg}
	}
	state := backgroundWatchUnconfirmed
	switch rf.body.Reason {
	case reasonRunBackgroundWatchSiteOff:
		state = backgroundWatchSiteOff
	case reasonDesktopSignInsOff:
		state = backgroundWatchSignInsOff
	case reasonRunBackgroundWatchOldBoot:
		state = backgroundWatchOldBoot
	case reasonRunBackgroundWatchOffline:
		state = backgroundWatchOffline
	case reasonRunBackgroundWatchControl:
		state = backgroundWatchControl
	case reasonRunBackgroundWatchPaused:
		state = backgroundWatchPaused
	}
	return backgroundWatchView{State: state, Message: rf.body.Error}
}

// backgroundWatchViewFor is GET /runs/{id}'s background_watch for the caller principal: nil on any run but a
// running background macOS VM run.
func (s *Server) backgroundWatchViewFor(ctx context.Context, run types.AgentRun, principal string) *backgroundWatchView {
	if !run.MachineKind.IsMacOSVM() || !run.BackgroundOnly() || run.State != types.RunRunning || runIsKept(run) {
		return nil
	}
	if !mayEnterRun(run, principal, false) {
		return &backgroundWatchView{State: backgroundWatchNotRunOwner}
	}
	rf := s.backgroundWatchRefusal(ctx, run)
	if rf != nil && rf.err != nil {
		slog.WarnContext(ctx, "wardynd: could not read whether a background desktop may be watched", "run_id", run.ID, "err", rf.err)
	}
	v := backgroundWatchStateOf(rf)
	return &v
}

// mountBackgroundWatchRoutes registers the watch's ticket on the authenticated group.
func (s *Server) mountBackgroundWatchRoutes(r chi.Router) {
	r.Post("/runs/{id}/desktop/watch-ticket", s.handleBackgroundWatchTicket)
	s.mountMacWindowRoutes(r) // the desktop door's second exception, beside the watch (run_mac_window.go)
}

// handleBackgroundWatchTicket mints a single-use ticket that opens a background macOS VM run's built-in desktop,
// view only, in a browser:
//
//	POST /api/v1/runs/{id}/desktop/watch-ticket
//
// The run's owner only: no sponsor arm, and no role reaches it, so a super admin's own run obeys its profile
// and an operator-owned run has no watch. getRunForEntry answers a foreign run as every entry route does. The
// ticket is member-stamped and bound to the desktop, as an app's ticket is (handleAppTicket), and a terminal
// refuses it (a ticket with an app never opens one).
func (s *Server) handleBackgroundWatchTicket(w http.ResponseWriter, r *http.Request) {
	// The ticket's authority time, taken before any store call (handleAttachTicket's convention).
	authorizedAt := s.cfg.Now().UTC()
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunForEntry(w, r, id)
	if !ok {
		return
	}
	at, principal := actorFromRequest(r)
	if !mayEnterRun(run, principal, false) {
		s.refuseRunOwnerOnly(w, r, run)
		return
	}
	if run.State != types.RunRunning {
		writeErrorReason(w, http.StatusConflict, reasonAttachNotRunning, "run is not RUNNING; cannot open its desktop (state="+string(run.State)+")")
		return
	}
	if runIsKept(run) {
		writeErrorReason(w, http.StatusConflict, reasonAttachRunKept, "run has ended; cannot open its desktop")
		return
	}
	if s.backgroundWatchRefusal(r.Context(), run).write(s, w, r) {
		return
	}
	s.issueAttachTicket(w, r, store.AttachTicket{RunID: run.ID, ActorType: at, Principal: principal, Role: oidc.RoleUser,
		App: uiDesktopAppName}, authorizedAt)
}

// ticketOpensBackgroundWatch says a redeemed ticket is the watch door's: bound to the built-in desktop, entering
// the desktop, callerless and member-stamped. A declared noVNC, an inbound app's ticket and an unbound ticket
// are none of these.
func ticketOpensBackgroundWatch(ta ticketActor, app string) bool {
	return app == uiDesktopAppName && ta.app == uiDesktopAppName && ta.caller == "" && ta.role == oidc.RoleUser
}

// uiBackgroundDoorRefused is uiEnterCommon's answer to a background run's refusal rf: an inbound app is let through,
// and every ticket but the watch door's takes the refusal. The watch's answers to the door's own checks, asked again
// here because a ticket outlives them by up to its lifetime, and mayEnterRun, because the sponsor arm passed
// uiTicketMayEnter. True when it has answered.
func (s *Server) uiBackgroundDoorRefused(w http.ResponseWriter, r *http.Request, rf *runRefusal, run types.AgentRun, app string, ta ticketActor) bool {
	if s.uiAppIsInbound(r.Context(), run.ID, app) {
		return false
	}
	if !ticketOpensBackgroundWatch(ta, app) {
		s.uiRefuseBackground(w, r, rf, run.ID, app, ta)
		return true
	}
	if !mayEnterRun(run, ta.principal, false) {
		s.auditUI(&run.ID, types.ActorHuman, ta.principal, "ui.authorize", app, "denied",
			ta.withVia(withBackgroundWatch(true, map[string]any{"reason": "not the run owner"})))
		writeErrorReason(w, http.StatusForbidden, reasonUIGatewayTicketRunMismatch, "attach ticket does not authorize this run")
		return true
	}
	refused := s.backgroundWatchRefusal(r.Context(), run)
	if refused == nil {
		return false
	}
	s.auditUI(&run.ID, types.ActorHuman, ta.principal, "ui.authorize", app, "denied",
		ta.withVia(withBackgroundWatch(true, map[string]any{"reason": backgroundWatchReasonOf(refused)})))
	return refused.write(s, w, r.WithContext(withTicketActor(r.Context(), ta)))
}

// backgroundWatchReasonOf is a refusal's audit reason.
func backgroundWatchReasonOf(rf *runRefusal) string {
	switch {
	case rf.err != nil:
		return reasonRunUnreadable
	case rf.decision != nil:
		return string(rf.decision.Reason)
	}
	return rf.body.Reason
}

// uiBackgroundWatchDenies is uiSessionStillAuthorized's arm for a watch: the door's questions as they stand now,
// the run's owner alone (a sponsor's arm passed the check before this). A refusal ends the watch with a 403, so a
// viewer's next socket stops rather than retries; a read that failed is unverifiable, and refuses too.
func (s *Server) uiBackgroundWatchDenies(ctx context.Context, sess uiSession, run types.AgentRun) *uiDialError {
	if !mayEnterRun(run, sess.Principal, false) {
		return s.uiDenyReassert(sess, uiDeniedReasonNotAuthorized, http.StatusForbidden, uiSessionNoLongerAuthorizedMsg)
	}
	rf := s.backgroundWatchRefusal(ctx, run)
	switch {
	case rf == nil:
		return nil
	case rf.err != nil:
		slog.ErrorContext(ctx, "wardynd: could not re-check a background desktop watch", "run_id", run.ID, "err", rf.err)
		return s.uiDenyReassert(sess, reasonRunUnreadable, http.StatusServiceUnavailable, uiSessionUnverifiableMsg)
	}
	return s.uiDenyReassert(sess, backgroundWatchReasonOf(rf), http.StatusForbidden, backgroundWatchEndedMsg)
}
