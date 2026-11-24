// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An attach socket is authenticated at its handshake, and a terminal can stay open far longer than
// the credential that opened it. While it is open the socket repeats the check that admitted it every
// attachAuthorityInterval, each lookup bounded by attachAuthorityLookup, and the first check that does
// answers no (a cut, an expiry, an ended portal grant, the run's profile now denying interactive sessions)
// ends it. A lookup that failed or timed out ends it only once no check has answered for the site's session
// budget (session_budget.go); it closes 1013, which the console reconnects on. A dropped terminal reconnects
// through a fresh ticket, which is checked anew.
const (
	attachAuthorityInterval = 30 * time.Second
	attachAuthorityLookup   = 5 * time.Second
)

// attachAuthEndedCode is the close code of a socket whose credential stopped authorizing it (the
// console does not reconnect on it), and attachAuthEndedReason its close reason and session.detach reason.
const (
	attachAuthEndedCode   websocket.StatusCode = 4401
	attachAuthEndedReason                      = "session ended"
)

var errAttachAuthorityEnded = errors.New("attach: the credential that opened this socket no longer authorizes it")

// errAttachSupportUnread is what a helper's socket check marks unconfirmed when the grant could not be read.
var errAttachSupportUnread = errors.New("attach: the helper's support grant could not be read")

// attachAuthorityTimings overrides attachAuthorityInterval and attachAuthorityLookup for one server
// (tests only); zero means the constant.
type attachAuthorityTimings struct {
	every, lookup time.Duration
}

func (s *Server) attachAuthorityEvery() time.Duration {
	if s.authorityTest.every > 0 {
		return s.authorityTest.every
	}
	return attachAuthorityInterval
}

func (s *Server) attachAuthorityLookupBound() time.Duration {
	if s.authorityTest.lookup > 0 {
		return s.authorityTest.lookup
	}
	return attachAuthorityLookup
}

// attachAuthority returns the check an attach socket opened by r repeats while it is open, or nil for a
// lane with nothing to repeat: the admin bearer token, local mode and a token presented directly on the
// socket keep their handshake-only semantics. A ticket is checked whatever credential minted it, so a
// ticket socket in local mode is handshake-only only while RoleStampTTL is unset: with it set, the
// socket still ends at the ticket's issue time plus it.
//
// Both lanes also ask the door's deny_interactive again (attachUnitCheck), with the door's exemption: on
// the ticket lane a super admin's ticket on a run adminDoorExempt exempts, on the cookie lane an ungoverned
// caller (runUngoverned) or an operator-owned run (refuseNoTicketAttach).
//
//   - Ticket lane: what redemption asked (ticketAuthority), against the ticket's own issue time and
//     sequence, the personal token or CLI session the ticket was minted under (still live: a revoke, an
//     expiry or a sign-out of it ends the socket), and the run's entry rule (a service identity's sponsor
//     still its sponsor since the ticket's authority time). The ticket's 30-second redemption window is not the terminal's lifetime; with
//     RoleStampTTL set the socket ends at the ticket's issue time plus it, and when the handshake also
//     carries the same person's session cookie, at that cookie's deadline too.
//   - Session cookie lane: the cookie's owner check as the OIDC middleware asks it (identity, authority
//     epoch, revocation sequence, session-only cuts included), and its deadline.
//
// A session cookie's deadline is the earlier of its expiry and its sign-in time plus RoleStampTTL.
func (s *Server) attachAuthority(r *http.Request, runID uuid.UUID) func(context.Context) error {
	ctx := r.Context()
	ttl := s.cfg.RoleStampTTL
	if ta, ok := ticketActorFromContext(ctx); ok {
		var deadline time.Time
		if ttl > 0 {
			deadline = ta.authorizedAt.Add(ttl)
		}
		if s.cfg.OIDC != nil {
			if sub, expiry, issuedAt, ok := s.cfg.OIDC.SessionBounds(r); ok && sub == ta.principal {
				deadline = earlierDeadline(deadline, sessionDeadline(expiry, issuedAt, ttl))
			}
		}
		superAdmin := ta.role == oidc.RoleAdmin
		bound := ta.tokenID != uuid.Nil || ta.cliSessionID != uuid.Nil
		if s.cfg.SessionRevocations == nil && ta.via == nil && deadline.IsZero() && superAdmin && !s.cfg.GovernAdminRuns && !bound {
			return nil
		}
		return func(lctx context.Context) error {
			if !deadline.IsZero() && !s.cfg.Now().Before(deadline) {
				return errAttachAuthorityEnded
			}
			if _, v := s.ticketAuthority(lctx, ta, runID); v != verdictOK {
				if v.unavailable() {
					return unconfirmed(errors.New(string(v)))
				}
				return errAttachAuthorityEnded
			}
			entry := func(run types.AgentRun) error {
				if ta.support != uuid.Nil {
					// A helper's socket lasts while its grant is live and ticks the terminal door: revoked, ended,
					// expired and the helper signed out end it; a grant read that fails rides the session budget.
					switch _, v := s.supportMayEnter(lctx, run, ta.principal, ta.support, supportDoorTerminal); v {
					case supportOK:
						return nil
					case supportUnavailable:
						return unconfirmed(errAttachSupportUnread)
					}
					return errAttachAuthorityEnded
				}
				ok, err := s.sponsorEntry(lctx, run, ta.principal, superAdmin, ta.authorizedAt)
				switch {
				case err != nil:
					return unconfirmed(err)
				case !ok:
					return errAttachAuthorityEnded
				}
				return nil
			}
			return s.attachUnitCheck(lctx, runID, entry, func(run types.AgentRun) bool { return superAdmin && s.adminDoorExempt(run) })
		}
	}
	sub := oidc.PrincipalFromContext(ctx)
	if sub == "" {
		return nil
	}
	email := oidc.EmailFromContext(ctx)
	issue := oidc.Issue{At: oidc.IssuedAtFromContext(ctx), Seq: oidc.RevocationSeqFor(ctx, sub)}
	epoch, _ := oidc.AuthorityEpochFromContext(ctx)
	deadline := sessionDeadline(oidc.ExpiryFromContext(ctx), issue.At, ttl)
	rev := s.cfg.SessionRevocations
	governed := !s.runUngoverned(ctx)
	return func(lctx context.Context) error {
		if !s.cfg.Now().Before(deadline) {
			return errAttachAuthorityEnded
		}
		if rev != nil {
			status, err := oidc.CheckSession(lctx, rev, sub, email, issue, epoch)
			if err != nil {
				return unconfirmed(err)
			}
			if status != oidc.SessionLive {
				return errAttachAuthorityEnded
			}
		}
		return s.attachUnitCheck(lctx, runID, nil, func(run types.AgentRun) bool { return !governed || run.OperatorOwned })
	}
}

// attachUnitCheck is the attach door asked again of runID as it stands now: the entry rule when entry is
// set (the ticket's, refuseAttachEntry's sponsor rule, so a sponsorship that has ended ends the socket; the
// grant half, service_attach, stays a mint-time check), then deny_interactive (refuseInteractiveAttach). entry
// answers nil (may enter), errAttachAuthorityEnded (may not) or an unconfirmed error (could not say). It
// returns errAttachAuthorityEnded once either refuses or the run is gone, and an unconfirmed error when the
// run or its profile cannot be read; a profile composition that nothing satisfies is a refusal, not a failed
// read. exempt is the door's exemption from deny_interactive, asked of the run.
func (s *Server) attachUnitCheck(ctx context.Context, runID uuid.UUID, entry func(types.AgentRun) error, exempt func(types.AgentRun) bool) error {
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if errors.Is(err, store.ErrNotFound) {
		return errAttachAuthorityEnded
	}
	if err != nil {
		return unconfirmed(err)
	}
	if entry != nil {
		if err := entry(run); err != nil {
			return err
		}
	}
	if exempt(run) {
		return nil
	}
	p, err := s.interactiveDeniedBy(ctx, run)
	if err != nil {
		if unitReadFailed(err) {
			return unconfirmed(err)
		}
		return errAttachAuthorityEnded
	}
	if p != nil {
		return errAttachAuthorityEnded
	}
	return nil
}

// sessionDeadline is a session cookie's deadline: the earlier of its expiry and issuedAt + ttl (ttl > 0).
func sessionDeadline(expiry, issuedAt time.Time, ttl time.Duration) time.Time {
	if ttl > 0 {
		return earlierDeadline(expiry, issuedAt.Add(ttl))
	}
	return expiry
}

// earlierDeadline is the earlier of two deadlines, a zero one meaning none.
func earlierDeadline(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}

// holdAttachAuthority runs check every attachAuthorityEvery until ctx ends and folds each answer into b; while a
// check has no answer it also asks on b's retry clock. It calls end once: with false on a refusal, with true when
// b's budget ran out. The returned func reports why the socket ended: "", attachAuthEndedReason or
// sessionUnconfirmedReason.
func (s *Server) holdAttachAuthority(ctx context.Context, b *sessionBudget, check func(context.Context) error, end func(spent bool)) func() string {
	var ended atomic.Pointer[string]
	reason := func() string {
		if p := ended.Load(); p != nil {
			return *p
		}
		return ""
	}
	if check == nil {
		return reason
	}
	go func() {
		t := time.NewTicker(s.attachAuthorityEvery())
		defer t.Stop()
		defer b.leave()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			case <-b.retryC():
			}
			lctx, cancel := context.WithTimeout(ctx, s.attachAuthorityLookupBound())
			err := check(lctx)
			cancel()
			if ctx.Err() != nil {
				return // the socket ended on its own meanwhile
			}
			v := b.observe("authority", err)
			if !v.ends() {
				continue
			}
			why := attachAuthEndedReason
			if v == budgetSpent {
				why = sessionUnconfirmedReason
				b.end()
			}
			ended.Store(&why)
			end(v == budgetSpent)
			return
		}
	}()
	return reason
}

// wsCloser is the part of *websocket.Conn endAttachAuthority uses.
type wsCloser interface {
	Close(code websocket.StatusCode, reason string) error
}

// attachEnders starts the two watchers that end an open attach from outside its pump, the mask fence
// and the re-check of the credential that opened the socket, and returns what the close reason is once
// the pump has returned: the fence's or the authority's when either ended it (the authority's last, as
// the handler read them before), else the pump's own.
//
// An issuer portal's socket has its authority on cut (attachIssuerCut) instead: a cut stops the holder driving
// the PTY and reads as the authority's end.
func (s *Server) attachEnders(ctx context.Context, r *http.Request, runID uuid.UUID, c *websocket.Conn, holder *attachHolder, cancel context.CancelFunc, cut *streamCut) func(pumped string) string {
	maskFenced := s.endAttachOnMaskFence(ctx, runID, c, cancel)
	var authEnded func() string
	if cut != nil {
		cut.addCloser(func() { holder.evicted.Store(true) })
		cut.setFence(holder.unconfirmed.Store)
		authEnded = func() string {
			if cut.isCut() {
				return attachAuthEndedReason
			}
			return ""
		}
	} else {
		b := s.newSessionBudget(ctx, attachWatchID(r, runID))
		b.fence = holder.unconfirmed.Store
		authEnded = s.holdAttachAuthority(ctx, b, s.attachAuthority(r, runID), func(spent bool) { endAttachAuthority(c, holder, cancel, spent) })
	}
	return func(reason string) string {
		if maskFenced() {
			reason = maskFencedReason
		}
		if why := authEnded(); why != "" {
			reason = why
		}
		return reason
	}
}

// attachWatchID is whose terminal r opened, as attachAuthority reads it: the ticket's principal, else the cookie's.
func attachWatchID(r *http.Request, runID uuid.UUID) sessionWatchID {
	principal := oidc.PrincipalFromContext(r.Context())
	if ta, ok := ticketActorFromContext(r.Context()); ok {
		principal = ta.principal
	}
	return sessionWatchID{Run: runID, Actor: types.ActorHuman, Principal: principal, Surface: sessionSurfaceTerminal}
}

// endAttachAuthority ends an attach socket whose credential stopped authorizing it. The holder stops
// driving the PTY at once (canWrite reads evicted), and writing the close frame stops output; the close
// frame is sent before cancel, because a cancelled socket context tears the connection down without one
// and the console would read a bare drop as a reason to reconnect. Close performs the handshake, so the
// teardown runs apart from the caller; cancel is the backstop when the peer never answers. When spent, the
// session budget ran out rather than a check refusing: the close is 1013 with sessionUnconfirmedReason, which
// the console reconnects on.
func endAttachAuthority(c wsCloser, holder *attachHolder, cancel context.CancelFunc, spent bool) {
	holder.evicted.Store(true)
	go func() {
		if spent {
			_ = c.Close(websocket.StatusTryAgainLater, sessionUnconfirmedReason)
		} else {
			_ = c.Close(attachAuthEndedCode, attachAuthEndedReason)
		}
		cancel()
	}()
}

// attachIssuerCut is the guarded cut over an issuer portal's terminal socket, and r's context, w and a no-op
// stop for any other socket. It is made before the upgrade, so the hijack registers with it. The socket ends at
// its ticket's grant end, and the ticket's authority (attachAuthority: the grant, the person's identity and the
// portal's pin included) is asked every attachAuthorityEvery from the moment redemption began
// (ticketActor.checkStart), each lookup bounded by attachAuthorityLookupBound. A check that fails cuts the
// socket, both directions at once, so it ends within one re-check of the event (35 s), or once no check has
// answered for the session budget after its first unanswered one; the socket takes no input meanwhile.
func (s *Server) attachIssuerCut(r *http.Request, w http.ResponseWriter, runID uuid.UUID) (context.Context, http.ResponseWriter, *streamCut, func()) {
	ta, ok := ticketActorFromContext(r.Context())
	if !ok || !issuerPortalPerson(ta.via, ta.principal) {
		return r.Context(), w, nil, func() {}
	}
	check := s.attachAuthority(r, runID)
	ctx, cancel := context.WithCancel(r.Context())
	// The socket ends at the grant end redemption read under the opening deadline (ticketActor.grantExpires),
	// not read again here: a setup that spent that deadline (the thaw's wait for the run's lock) never reads a
	// live grant as ended. A revoke since redemption is the watch's, timed from checkStart.
	hardEnd := time.Now() // no grant end: the socket ends before it opens
	if !ta.grantExpires.IsZero() {
		hardEnd = time.Now().Add(ta.grantExpires.Sub(s.cfg.Now()))
	}
	cut := newStreamCut(w, hardEnd, cancel)
	start := ta.checkStart
	if start.IsZero() {
		start = time.Now()
	}
	every := s.attachAuthorityEvery()
	b := s.newSessionBudget(ctx, attachWatchID(r, runID))
	b.fence = cut.budgetFenced
	cut.watch(ctx, start.Add(every), every, s.attachAuthorityLookupBound(), b, check)
	return ctx, cut.Writer(w), cut, func() {
		defer cancel()
		cut.End()
	}
}
