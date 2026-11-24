// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An open session (a terminal, a helper's command, a desktop, a window on a Mac, a relayed app, an inbound stream or
// an SSH connection) asks again on a clock whether its person may still be there. A check that answers "no" ends it
// at that check, as it always has. A check that gets no answer (a store read that failed or ran out of its bound)
// opens the session's unconfirmed-access window (#4057): the session asks again every sessionRetryEvery and ends,
// with a close its client reconnects on, once no check has answered for the site's budget, counted from that first
// unanswered check. A budget of 0 ends it at the first one. Each window writes one session.unconfirmed row:
// rode_through when a later check answered, ended when the budget ran out, refused when a definite refusal ended
// the session inside the window, closed when the session ended by itself inside the window.

const (
	// sessionRetryEvery is how often a session whose access is unconfirmed asks again.
	sessionRetryEvery = 5 * time.Second
	// sessionBudgetReadEvery is how long this replica reuses the budget it last read from the site config, and
	// sessionBudgetReadWait bounds that read. A read that fails keeps the last value, or the default before any.
	sessionBudgetReadEvery = time.Minute
	sessionBudgetReadWait  = 2 * time.Second
	// sessionUnconfirmedAuditWait bounds the session.unconfirmed write, which runs off the watch.
	sessionUnconfirmedAuditWait = 10 * time.Second
)

// sessionUnconfirmedReason is the close reason, and the session.detach reason, of a terminal the budget ended.
const sessionUnconfirmedReason = "Access could not be confirmed"

// The surfaces a session.unconfirmed row names.
const (
	sessionSurfaceTerminal = "terminal"
	sessionSurfaceCommand  = "command"
	sessionSurfaceDesktop  = "desktop"
	sessionSurfaceWindow   = "mac_window"
	sessionSurfaceUIApp    = "ui_app"
	sessionSurfaceInbound  = "inbound"
	sessionSurfaceSSH      = "ssh"
)

// errSessionUnconfirmed marks a re-check that got no answer; unconfirmed wraps a failed read with it. Every other
// non-nil answer is a refusal and ends the session at once, so an error nobody marked fails closed.
var errSessionUnconfirmed = errors.New("api: an open session's access could not be confirmed")

// errSessionRefused is step's refusal when its check answers with a reason rather than an error.
var errSessionRefused = errors.New("api: an open session's re-check refused it")

func unconfirmed(err error) error { return fmt.Errorf("%w: %w", errSessionUnconfirmed, err) }

// unitReadFailed reports whether err, from runUnit or a check built on it, is a read that failed, which a check may
// mark unconfirmed. A profile composition that nothing satisfies is an answer, not a failed read: it refuses again
// on every later check (a *composer.OverlayError, wrapped by foldRestrictionLimits, or an
// overlayUnsatisfiableError), so riding it would only delay the end by the budget.
func unitReadFailed(err error) bool {
	var oe *composer.OverlayError
	_, u := isOverlayUnsatisfiable(err)
	return !u && !errors.As(err, &oe)
}

// sessionWatchID is whose session a window is, for its row.
type sessionWatchID struct {
	Run       uuid.UUID
	Actor     types.ActorType
	Principal string
	Surface   string
}

// budgetVerdict is what observe tells a watch to do.
type budgetVerdict int

const (
	budgetHolds   budgetVerdict = iota // every check of the window has answered: go on at the session's own interval
	budgetRetry                        // a check has no answer and the budget has time left: ask again on retryC
	budgetSpent                        // no answer for the whole budget: end with the session's reconnecting close
	budgetRefused                      // a definite refusal: end the session as before
)

// ends reports whether v ends the session.
func (v budgetVerdict) ends() bool { return v == budgetSpent || v == budgetRefused }

// sessionBudget is one open session's window. Its one watch goroutine owns it: nothing else calls it.
type sessionBudget struct {
	s       *Server
	ctx     context.Context // the session's opening context, never cancelled (WithoutCancel), for the row's stamps
	id      sessionWatchID
	n       time.Duration
	retry   time.Duration
	since   time.Time        // the window's first unanswered check (monotonic); zero while confirmed
	failing map[string]error // the window's checks that have not answered since, by name
	seen    map[string]bool  // every check that went unanswered in the window, for a rode_through row
	timer   *time.Timer
	fence   func(on bool) // optional: true as a window opens, false as it closes
}

// newSessionBudget is the window of a session whose watch starts now, with the budget in force now.
func (s *Server) newSessionBudget(ctx context.Context, id sessionWatchID) *sessionBudget {
	return &sessionBudget{s: s, ctx: context.WithoutCancel(ctx), id: id, n: s.sessionUnconfirmedBudget(ctx),
		retry: s.sessionRetry(), failing: map[string]error{}, seen: map[string]bool{}}
}

// observe folds one answer of the check named check into the window. A nil budget is strict: any error refuses.
func (b *sessionBudget) observe(check string, err error) budgetVerdict {
	if b == nil {
		if err != nil {
			return budgetRefused
		}
		return budgetHolds
	}
	switch {
	case err != nil && !errors.Is(err, errSessionUnconfirmed):
		b.stopTimer()
		if !b.since.IsZero() {
			// A refusal that ends a window writes its row here, so a watch calls end() only on budgetSpent.
			b.auditWindow("refused", time.Since(b.since), slices.Sorted(maps.Keys(b.failing)))
			b.since = time.Time{}
		}
		return budgetRefused
	case err != nil:
		if b.since.IsZero() {
			b.since = time.Now()
			if b.fence != nil {
				b.fence(true)
			}
		}
		b.failing[check] = err
		b.seen[check] = true
	default:
		delete(b.failing, check)
		if len(b.failing) == 0 {
			if !b.since.IsZero() {
				b.auditWindow("rode_through", time.Since(b.since), slices.Sorted(maps.Keys(b.seen)))
				clear(b.seen)
				b.since = time.Time{}
				if b.fence != nil {
					b.fence(false)
				}
			}
			b.stopTimer()
			return budgetHolds
		}
	}
	if time.Since(b.since) >= b.n {
		return budgetSpent
	}
	if b.timer == nil {
		b.timer = time.NewTimer(b.retry)
	} else {
		b.timer.Reset(b.retry)
	}
	return budgetRetry
}

// step is observe for a check that answers whether it refused and whether that refusal was a read that failed.
func (b *sessionBudget) step(check string, refused, unread bool) budgetVerdict {
	switch {
	case !refused:
		return b.observe(check, nil)
	case unread:
		return b.observe(check, unconfirmed(errSessionRefused))
	}
	return b.observe(check, errSessionRefused)
}

// stepEnds is step for a watch with one check: it reports whether the session ends, and has written the ended
// row when the budget ran out.
func (b *sessionBudget) stepEnds(check string, refused, unread bool) bool {
	v := b.step(check, refused, unread)
	if v == budgetSpent {
		b.end()
	}
	return v.ends()
}

// end writes the ended row of a window whose budget ran out. A watch calls it once, as it ends the session on
// budgetSpent (stepEnds does so for a watch with one check), and never on budgetRefused: observe has written that
// window's refused row. It closes the window, so the watch's deferred leave writes no second row.
func (b *sessionBudget) end() {
	if b == nil || b.since.IsZero() {
		return
	}
	b.stopTimer()
	b.auditWindow("ended", time.Since(b.since), slices.Sorted(maps.Keys(b.failing)))
	b.since = time.Time{}
}

// leave is the watch's deferred exit: it stops the retry timer and, when the session ends by itself inside a
// window, writes that window's row (result closed), so every window has one row.
func (b *sessionBudget) leave() {
	if b == nil {
		return
	}
	b.stopTimer()
	if !b.since.IsZero() {
		b.auditWindow("closed", time.Since(b.since), slices.Sorted(maps.Keys(b.failing)))
		b.since = time.Time{}
	}
}

// retryC fires when the window's next retry is due; nil while access is confirmed.
func (b *sessionBudget) retryC() <-chan time.Time {
	if b == nil || b.timer == nil || len(b.failing) == 0 {
		return nil
	}
	return b.timer.C
}

// retrying reports whether check is one of the window's unanswered checks: a retry asks only those.
func (b *sessionBudget) retrying(check string) bool {
	if b == nil {
		return false
	}
	_, ok := b.failing[check]
	return ok
}

func (b *sessionBudget) stopTimer() {
	if b != nil && b.timer != nil {
		b.timer.Stop()
	}
}

// auditWindow writes the window's row off the watch, within sessionUnconfirmedAuditWait: a write that waits on a store
// that is down holds up no close. The recorder chain spools a row it cannot write (recordAuditDurably).
func (b *sessionBudget) auditWindow(result string, d time.Duration, checks []string) {
	s, id := b.s, b.id
	data := mustJSON(map[string]any{"surface": id.Surface, "result": result, "unconfirmed_ms": d.Milliseconds(),
		"budget_s": int(b.n / time.Second), "checks": checks})
	outcome := "success"
	if result != "rode_through" {
		outcome = "failure"
	}
	ctx, cancel := context.WithTimeout(b.ctx, sessionUnconfirmedAuditWait)
	go func() {
		defer cancel()
		s.auditSessionUnconfirmed(ctx, id, outcome, data)
	}()
}

// auditSessionUnconfirmed writes one session.unconfirmed row.
func (s *Server) auditSessionUnconfirmed(ctx context.Context, id sessionWatchID, outcome string, data []byte) {
	s.recordAudit(ctx, s.auditEvent(&id.Run, id.Actor, id.Principal, "session.unconfirmed", id.Surface, outcome, data))
}

// sessionBudgetState is the site's budget as this replica last read it, and the tests' overrides.
type sessionBudgetState struct {
	mu     sync.Mutex
	n      time.Duration
	readAt time.Time // zero before the first read that answered
	// test, when set, is every session's budget, and retry, when set, replaces sessionRetryEvery (tests only).
	test  *time.Duration
	retry time.Duration
}

// sessionUnconfirmedBudget is the budget a session whose watch starts now takes: the site config's, read at most
// once per sessionBudgetReadEvery. A read that fails or runs out keeps the last value, or the default before any,
// and never refuses the session.
func (s *Server) sessionUnconfirmedBudget(ctx context.Context) time.Duration {
	st := &s.sessionBudgets
	if st.test != nil {
		return *st.test
	}
	st.mu.Lock()
	n, at := st.n, st.readAt
	st.mu.Unlock()
	if at.IsZero() {
		n = types.SessionBudgetDefaultSec * time.Second
	} else if time.Since(at) < sessionBudgetReadEvery {
		return n
	}
	if s.cfg.Store == nil {
		return n
	}
	rctx, cancel := context.WithTimeout(ctx, sessionBudgetReadWait)
	defer cancel()
	cfg, err := s.cfg.Store.GetSiteConfig(rctx)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: the open-session budget could not be read; keeping the last one", "err", err)
		return n
	}
	n = min(max(cfg.Sessions.UnconfirmedBudget(), 0), types.SessionBudgetMaxSec*time.Second)
	st.mu.Lock()
	st.n, st.readAt = n, time.Now()
	st.mu.Unlock()
	return n
}

func (s *Server) sessionRetry() time.Duration {
	if s.sessionBudgets.retry > 0 {
		return s.sessionBudgets.retry
	}
	return sessionRetryEvery
}

// uiWatchID is whose session a UI relay's window is, for its row.
func uiWatchID(sess uiSession, surface string) sessionWatchID {
	return sessionWatchID{Run: sess.Run, Actor: uiActorType(sess), Principal: sess.Principal, Surface: surface}
}

// tickOrRetry is the channel a watch with one clock waits on: b's retry while a window is open, else its own tick.
func tickOrRetry(tick <-chan time.Time, b *sessionBudget) <-chan time.Time {
	if r := b.retryC(); r != nil {
		return r
	}
	return tick
}
