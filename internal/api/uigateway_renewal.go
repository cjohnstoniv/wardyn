// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A session or ticket that came through an issuer portal (a portal with its own identity issuer, whose
// people are delegate:person:<issuer key>:<person id>) answers to more than its grant. At ticket redemption,
// at every relay check and at every tick of an open stream it also asks the person's identity (a suspended,
// purged or deactivated person is refused even while their grant lives) and the portal's pin on the current
// unit tree, and it is never answered from the relay's pass cache. A console portal's session keeps today's
// checks.

// issuerPortalPerson reports whether via and principal are an issuer portal's: a portal grant names its own
// portal's person and no one else (portalGrantPrincipalOK), and a console portal's person is never a
// delegate:person: name.
func issuerPortalPerson(via *types.DelegationVia, principal string) bool {
	return via != nil && isPortalPersonName(principal)
}

// issuerPortalPersonHolds asks the two facts of an issuer-portal person that the grant and the revocation
// cutoff do not: the person's identity, read at epoch -1 as every credential lane reads it, and the portal's
// pin over run on the current tree. A store that cannot answer either refuses, 503; nil means both hold.
func (s *Server) issuerPortalPersonHolds(ctx context.Context, principal string, via types.DelegationVia, run types.AgentRun) *uiDialError {
	unavailable := &uiDialError{status: http.StatusServiceUnavailable, reason: uiDeniedReasonRevocationUnavailable, msg: uiSessionUnverifiableMsg}
	ib, ok := s.cfg.Store.(identityBlocker)
	if !ok {
		slog.ErrorContext(ctx, "wardynd: an issuer-portal session cannot be checked: the store has no identity read", "run_id", run.ID)
		return unavailable
	}
	blocked, err := ib.IdentityBlocked(ctx, principal, -1)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: an issuer-portal person's identity could not be read", "run_id", run.ID, "err", err)
		return unavailable
	}
	if blocked {
		return &uiDialError{status: http.StatusForbidden, reason: uiDeniedReasonRevoked, msg: uiSessionNoLongerAuthorizedMsg}
	}
	if rf := s.portalRunInPin(audit.WithDelegation(ctx, via), run); rf != nil {
		return &uiDialError{status: rf.status, reason: rf.body.Reason, msg: rf.body.Error}
	}
	return nil
}

// issuerPortalTicketHolds is issuerPortalPersonHolds for a ticket, on the run as it stands now, as a verdict:
// the refusal's reason is the verdict, so a refusal is the bad-ticket answer and an outage a 503. A run that is
// gone is a bad ticket (403), as on every other ticket's path, never an outage.
func (s *Server) issuerPortalTicketHolds(ctx context.Context, ta ticketActor, runID uuid.UUID) entryVerdict {
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if errors.Is(err, store.ErrNotFound) {
		return verdictInvalid
	}
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: an issuer-portal ticket's run could not be read", "run_id", runID, "err", err)
		return verdictRevocationUnavailable
	}
	if de := s.issuerPortalPersonHolds(ctx, ta.principal, *ta.via, run); de != nil {
		return entryVerdict(de.reason)
	}
	return verdictOK
}

// uiSessionRenewalShips is the release switch for longer browser-app sessions. False withholds them: an
// issuer portal's registration refuses ui_session_renewal (validatePortalRow), and a session that was opened
// renewable is checked against its own grant, so it ends with that grant as one without renewal does.
var uiSessionRenewalShips = true

// Renewal (ui_session_renewal, on an issuer portal only). A renewable browser-app session is not bound to the
// grant it was opened under but to its chain: the grants the same portal minted the same person while one of
// them was live. Its cookie carries the chain (uiSession.Chain) and expires at redemption plus the session TTL;
// every new request asks the chain, and is refused once no grant of it is live, the portal is revoked or the
// person or the pin refuses. An open stream never follows the chain: it ends at the end it was admitted with.
// Desktops, terminals and a support helper's sessions never renew.

// uiRenewalChain is the chain a ticket's UI session renews on: the minting grant's chain when the ticket came
// through an issuer portal that renews sessions, and uuid.Nil otherwise or when either read fails (the session
// then ends with its grant, the safe direction). Redemption itself is always against the minting grant.
func (s *Server) uiRenewalChain(ctx context.Context, ta ticketActor) uuid.UUID {
	if !uiSessionRenewalShips || !issuerPortalPerson(ta.via, ta.principal) {
		return uuid.Nil
	}
	pgs, ok := s.cfg.Store.(store.PortalGrantStore)
	if !ok {
		return uuid.Nil
	}
	g, err := pgs.GetPortalGrantByID(ctx, ta.via.Grant, s.cfg.Now().UTC())
	if err != nil || g.DelegateID != ta.via.Delegate || g.Principal != ta.principal {
		return uuid.Nil
	}
	end, renewal, err := pgs.PortalChainLive(ctx, g.DelegateID, g.Principal, g.ChainID)
	if err != nil || !renewal || end.IsZero() {
		return uuid.Nil
	}
	return g.ChainID
}

// uiPortalGrantEnd is the grant arm of an issuer portal's session check: the chain's effective end for a
// renewable session while its portal still renews and renewal ships, else the entry grant's end. A zero end
// with verdictOK never happens: no live grant is verdictDelegationEnded.
func (s *Server) uiPortalGrantEnd(ctx context.Context, sess uiSession) (time.Time, entryVerdict) {
	if sess.Chain != uuid.Nil && uiSessionRenewalShips {
		if pgs, ok := s.cfg.Store.(store.PortalGrantStore); ok {
			end, renewal, err := pgs.PortalChainLive(ctx, sess.Via.Delegate, sess.Principal, sess.Chain)
			if err != nil {
				slog.ErrorContext(ctx, "wardynd: a renewable session's chain could not be read", "run_id", sess.Run, "err", err)
				return time.Time{}, verdictDelegationUnavailable
			}
			if renewal {
				if end.IsZero() {
					return time.Time{}, verdictDelegationEnded
				}
				return end, verdictOK
			}
		}
	}
	return s.delegationLive(ctx, *sess.Via)
}

// relayUISession serves one request of an authenticated session, a cookie's or an inbound token's.
//
// The re-check runs first, and the watch over the open request is installed straight after it, before the
// presence work (the inbound admission, the idle clock, the presence stamp), any of which can wait on the
// store or the runner. For an issuer portal's session the watch is timed from the moment the re-check began:
// an event after that moment is seen by a probe that starts within one interval of it.
func (s *Server) relayUISession(w http.ResponseWriter, r *http.Request, sess uiSession) {
	checkStart := time.Now()
	// Re-assert the human here as well as in uiDial, debounced but for a helper and an issuer portal's
	// person: net/http dials only when its pool has nothing reusable, so most requests of a busy session
	// never reach uiDial at all (uiReassertRelay).
	actx, acancel := issuerAdmitCtx(r.Context(), sess)
	end, de := s.uiReassertRelayEnd(actx, sess)
	acancel()
	if de != nil {
		writeErrorReason(w, de.status, de.reason, de.msg)
		return
	}
	ctx, w, stop := s.relayWatchCtx(r.Context(), w, sess, checkStart, end)
	defer stop()
	r = r.WithContext(ctx)
	if sess.Inbound && !s.admitInbound(w, r, sess) {
		return
	}
	// Keep the run's idle clock alive for the life of the session, debounced
	// exactly like the decision-ingest and attach keepalives: a human reading
	// code in an editor is not idle, and the reaper must not stop the run under
	// them.
	// A human relaying UI traffic is always real presence, so only the debounce applies.
	if s.touchDue(sess.Run) { // a relay needs a Store (uiGatewayEnabled); uiReassertRelay above already read it
		_ = s.cfg.Store.TouchRun(ctx, sess.Run)
	}
	ctx = context.WithValue(ctx, uiSessionCtxKey{}, sess)
	if sess.Via != nil {
		ctx = audit.WithDelegation(ctx, *sess.Via) // a thaw here, or in uiDial, names the portal (#1234)
	}
	// The same human is presence for the pause, and thaws a paused run.
	_ = s.markPresent(ctx, sess.Run, uiActorType(sess), sess.Principal, "presence")
	ctx = context.WithValue(ctx, uiDialErrCtxKey{}, &uiDialErrBox{})
	s.serveUIRelay(w, r.WithContext(ctx))
}

// relayWatchCtx is the context and writer a relayed request runs under, and the stop its handler defers.
//
//   - An issuer portal's session, renewal on or off: a streamCut ends the request, a 101 and a streaming
//     answer included, at its known end (the earliest of the cookie's signed expiry, its issue time plus this
//     process's session TTL, and the end the re-check read: the chain's for a renewable session, else the
//     entry grant's), and re-checks it every uiReassertInterval from checkStart, each lookup bounded by
//     supportWatchWait; a check that fails cuts it, a blocked write included. The end never moves.
//   - A support helper's session: supportWatchCtx.
//   - Any other: ctx and w as they are, as today.
func (s *Server) relayWatchCtx(ctx context.Context, w http.ResponseWriter, sess uiSession, checkStart, end time.Time) (context.Context, http.ResponseWriter, func()) {
	if !issuerPortalPerson(sess.Via, sess.Principal) {
		ctx, stop := s.supportWatchCtx(ctx, sess)
		return ctx, w, stop
	}
	ctx, cancel := context.WithCancel(ctx)
	cut := newStreamCut(w, s.uiStreamEnd(sess, end), cancel)
	every := time.Duration(cmp.Or(supportWatchEveryNS.Load(), int64(uiReassertInterval)))
	bound := time.Duration(cmp.Or(supportWatchWaitNS.Load(), int64(supportWatchWait)))
	cut.watch(ctx, checkStart.Add(every), every, bound, func(lctx context.Context) error { return s.relayStillHolds(lctx, sess) })
	return ctx, cut.Writer(w), func() {
		defer cancel()
		cut.End()
	}
}

// issuerAdmitCtx bounds an issuer portal's foreground authorization reads (a desktop's Closed-set read, a
// request's re-check) by one deadline, set when its cookie was decoded (uiSession.admitBy), apart from the life of
// the stream they admit: a store that hangs answers 503 within it rather than holding the request, and a slow
// first read leaves the later ones only what remains. Any other session keeps ctx.
func issuerAdmitCtx(ctx context.Context, sess uiSession) (context.Context, context.CancelFunc) {
	if !issuerPortalPerson(sess.Via, sess.Principal) {
		return ctx, func() {}
	}
	if !sess.admitBy.IsZero() {
		return context.WithDeadline(ctx, sess.admitBy)
	}
	return context.WithTimeout(ctx, issuerAdmitBound())
}

// issuerOpeningRun reads run id under ctx, an issuer portal's opening deadline: the run, or gone for a run that
// does not exist (the caller answers it as its route answers a missing run), or, for a read that failed or ran
// out, false with the retryable 503 written with msg.
func (s *Server) issuerOpeningRun(ctx context.Context, w http.ResponseWriter, id uuid.UUID, msg string) (run types.AgentRun, gone, ok bool) {
	run, err := s.cfg.Store.GetRun(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return types.AgentRun{}, true, false
	case err != nil:
		slog.ErrorContext(ctx, "wardynd: an issuer portal's run could not be read at its opening", "run_id", id, "err", err)
		writeErrorReason(w, http.StatusServiceUnavailable, uiDeniedReasonRevocationUnavailable, msg)
		return types.AgentRun{}, false, false
	}
	return run, false, true
}

// issuerOpeningIn reports whether ctx carries an issuer portal ticket still in its opening (ticketActor.admitBy).
// A read that fails or runs out there answers refuseIssuerOpeningUnread's retryable 503, never a 500.
func issuerOpeningIn(ctx context.Context) bool {
	ta, ok := ticketActorFromContext(ctx)
	return ok && !ta.admitBy.IsZero()
}

// refuseIssuerOpeningUnread answers an issuer portal's opening read that failed or ran out: the retryable 503.
func refuseIssuerOpeningUnread(ctx context.Context, w http.ResponseWriter, what string, err error) {
	slog.ErrorContext(ctx, "wardynd: an issuer portal's opening read failed", "read", what, "err", err)
	writeErrorReason(w, http.StatusServiceUnavailable, uiDeniedReasonRevocationUnavailable, uiSessionUnverifiableMsg)
}

// issuerAdmitBound is the bound on an issuer portal's opening authorization reads, the watch's own.
func issuerAdmitBound() time.Duration {
	return time.Duration(cmp.Or(supportWatchWaitNS.Load(), int64(supportWatchWait)))
}

// uiStreamEnd is an issuer-portal stream's known end on the wall clock: the earliest of sess's signed
// expiry, its issue time plus this process's session TTL, and end (the chain's or the entry grant's, zero
// for none).
func (s *Server) uiStreamEnd(sess uiSession, end time.Time) time.Time {
	at := time.Unix(sess.Expires, 0)
	if ttl := time.Unix(sess.IssuedAt, 0).Add(s.uiSessionTTLFor(sess)); ttl.Before(at) {
		at = ttl
	}
	if !end.IsZero() && end.Before(at) {
		at = end
	}
	return time.Now().Add(at.Sub(s.cfg.Now()))
}

// errRelayAuthorityEnded is a relayed stream's re-check that refused.
var errRelayAuthorityEnded = errors.New("api: this UI session is no longer authorized")

// relayStillHolds is one tick of an open relayed request: the run read fresh and the full re-check, with no
// pass cache. A later end the chain now reports changes nothing: the stream keeps the end it was admitted with.
func (s *Server) relayStillHolds(ctx context.Context, sess uiSession) error {
	run, err := s.cfg.Store.GetRun(ctx, sess.Run)
	if err != nil {
		return err
	}
	if s.uiSessionStillAuthorized(ctx, sess, run) != nil {
		return errRelayAuthorityEnded
	}
	return nil
}

// desktopIssuerCut is the guarded cut over an issuer portal's desktop stream, and r's context, w and a no-op stop
// for any other. It is made before the upgrade, so the hijack registers with it, and its watch runs from
// checkStart, the opening check's start, before the start lock and the dial: the stream ends at its known end
// (uiStreamEnd; a desktop never renews), and the run's full re-check (desktopRunCheck) runs every
// uiDesktopRecheck, bounded by uiDesktopRecheckWait, on its own goroutine, apart from the Closed set's read. A
// check that fails cuts the stream, so it ends within one re-check of the event (40 s).
func (s *Server) desktopIssuerCut(r *http.Request, w http.ResponseWriter, sess uiSession, checkStart, end time.Time) (context.Context, http.ResponseWriter, *streamCut, func()) {
	if !issuerPortalPerson(sess.Via, sess.Principal) {
		return r.Context(), w, nil, func() {}
	}
	ctx, cancel := context.WithCancel(r.Context())
	cut := newStreamCut(w, s.uiStreamEnd(sess, end), cancel)
	every := cmp.Or(s.uiDesktop.recheckEvery, uiDesktopRecheck)
	cut.watch(ctx, checkStart.Add(every), every, cmp.Or(s.uiDesktop.recheckWait, uiDesktopRecheckWait), func(lctx context.Context) error {
		if _, code, _ := s.desktopRunCheck(lctx, sess); code != 0 {
			return errRelayAuthorityEnded
		}
		return nil
	})
	return ctx, cut.Writer(w), cut, func() {
		defer cancel()
		cut.End()
	}
}
