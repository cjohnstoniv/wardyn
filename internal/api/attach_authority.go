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
)

// An attach socket is authenticated at its handshake, and a terminal can stay open far longer than
// the credential that opened it. While it is open the socket repeats the check that admitted it every
// attachAuthorityInterval, each lookup bounded by attachAuthorityLookup, and the first check that does
// not answer "still authorized" (a cut, an expiry, an ended portal grant, or a lookup that failed or
// timed out) ends it. A dropped terminal reconnects through a fresh ticket, which is checked anew.
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
//   - Ticket lane: what redemption asked (ticketAuthority), against the ticket's own issue time and
//     sequence. The ticket's 30-second redemption window is not the terminal's lifetime; with
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
		if s.cfg.SessionRevocations == nil && ta.via == nil && deadline.IsZero() {
			return nil
		}
		return func(lctx context.Context) error {
			if !deadline.IsZero() && !s.cfg.Now().Before(deadline) {
				return errAttachAuthorityEnded
			}
			if _, v := s.ticketAuthority(lctx, ta, runID); v != verdictOK {
				return errAttachAuthorityEnded
			}
			return nil
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
	return func(lctx context.Context) error {
		if !s.cfg.Now().Before(deadline) {
			return errAttachAuthorityEnded
		}
		if rev == nil {
			return nil
		}
		status, err := oidc.CheckSession(lctx, rev, sub, email, issue, epoch)
		if err != nil {
			return err
		}
		if status != oidc.SessionLive {
			return errAttachAuthorityEnded
		}
		return nil
	}
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

// holdAttachAuthority runs check every attachAuthorityEvery until ctx ends, and calls end once, on the
// first check that fails. It reports whether it did.
func (s *Server) holdAttachAuthority(ctx context.Context, check func(context.Context) error, end func()) func() bool {
	var ended atomic.Bool
	if check == nil {
		return ended.Load
	}
	go func() {
		t := time.NewTicker(s.attachAuthorityEvery())
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			lctx, cancel := context.WithTimeout(ctx, s.attachAuthorityLookupBound())
			err := check(lctx)
			cancel()
			if err == nil {
				continue
			}
			if ctx.Err() != nil {
				return // the socket ended on its own meanwhile
			}
			ended.Store(true)
			end()
			return
		}
	}()
	return ended.Load
}

// wsCloser is the part of *websocket.Conn endAttachAuthority uses.
type wsCloser interface {
	Close(code websocket.StatusCode, reason string) error
}

// attachEnders starts the two watchers that end an open attach from outside its pump, the mask fence
// and the re-check of the credential that opened the socket, and returns what the close reason is once
// the pump has returned: the fence's or the authority's when either ended it (the authority's last, as
// the handler read them before), else the pump's own.
func (s *Server) attachEnders(ctx context.Context, r *http.Request, runID uuid.UUID, c *websocket.Conn, holder *attachHolder, cancel context.CancelFunc) func(pumped string) string {
	maskFenced := s.endAttachOnMaskFence(ctx, runID, c, cancel)
	authEnded := s.holdAttachAuthority(ctx, s.attachAuthority(r, runID), func() { endAttachAuthority(c, holder, cancel) })
	return func(reason string) string {
		if maskFenced() {
			reason = maskFencedReason
		}
		if authEnded() {
			reason = attachAuthEndedReason
		}
		return reason
	}
}

// endAttachAuthority ends an attach socket whose credential stopped authorizing it. The holder stops
// driving the PTY at once (canWrite reads evicted), and writing the close frame stops output; the close
// frame is sent before cancel, because a cancelled socket context tears the connection down without one
// and the console would read a bare drop as a reason to reconnect. Close performs the handshake, so the
// teardown runs apart from the caller; cancel is the backstop when the peer never answers.
func endAttachAuthority(c wsCloser, holder *attachHolder, cancel context.CancelFunc) {
	holder.evicted.Store(true)
	go func() {
		_ = c.Close(attachAuthEndedCode, attachAuthEndedReason)
		cancel()
	}()
}
