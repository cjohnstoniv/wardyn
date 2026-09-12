// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The SECOND bound on the auth.failed emit. http.go's authFailedLimiter caps the
// RATE (~1 row/sec, burst 5); this file folds a slow, permanent DRIP — the
// failure mode that actually emptied a deployment's audit window: one row a
// minute from a single sidecar retrying a renew the control plane would never
// grant, forever, under a limiter it never once tripped. Split out of http.go
// along that seam — the limiter answers "how fast", this answers "how many of the
// same thing" — and because http.go was at its size ceiling.

package api

import (
	"context"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// authFailedKey is what makes two auth.failed rows "identical" for coalescing:
// the boundary that refused, its bounded reason, the request path, and the TCP
// peer. SourceIP is in the key because on a single-tenant or loopback deployment
// it genuinely separates principals — but it is NOT the defence here and
// THREAT-MODEL.md says so: behind a Kubernetes ingress or load balancer every
// client shares one peer address (RealIP is deliberately not installed, see
// routes.go), so a credential-stuffing burst arrives under ONE key. What keeps
// such a burst from collapsing into a single row is the pair of bounds below —
// the window and the max count — not this field.
type authFailedKey struct {
	actor, reason, target, sourceIP string
}

// maxAuthFailedStreak caps one summary row's count. Past it the streak closes and
// a new one starts, so a tight loop produces a row every 1000 refusals rather
// than one row whenever it happens to stop — an upper bound on how much volume a
// single row is allowed to stand for.
const maxAuthFailedStreak = 1000

// authFailedStreak is the ONE open run of identical consecutive refusals.
// Consecutive, so there is at most one: any row with a different key closes it.
type authFailedStreak struct {
	key       authFailedKey
	count     int
	firstSeen time.Time
	lastSeen  time.Time
	timer     *time.Timer
}

// coalesceAuthFailed folds identical consecutive refusals together.
//
// It returns (absorbed, summary):
//   - absorbed=true means the caller must NOT emit this row; it was folded into
//     the open streak.
//   - summary non-nil is a streak that just closed and whose count must be
//     recorded — a NEW row, never an update of the first one. That is not a
//     stylistic choice: audit_events is append-only, UPDATE/DELETE are blocked by
//     trigger, and every row is hash-chained to its predecessor (0047), so
//     "update the first row's count" is not an operation this table has.
//
// The window is the maximum GAP between two consecutive identical rows, not a
// cap on a streak's total duration: the motivating flood was one row a minute
// forever, and a 5m gap window folds exactly that while leaving a handful of
// genuinely spaced-out failures individually visible.
func (s *Server) coalesceAuthFailed(actor, reason, target, sourceIP string) (bool, *types.AuditEvent) {
	window := s.cfg.AuditCoalesceWindow
	if window <= 0 {
		return false, nil // disabled: every refusal is its own row, exactly as before
	}
	key := authFailedKey{actor: actor, reason: reason, target: target, sourceIP: sourceIP}
	now := s.cfg.Now()

	s.authFailedStreakMu.Lock()
	defer s.authFailedStreakMu.Unlock()
	open := s.authFailedStreak
	if open != nil && open.key == key {
		open.count++
		open.lastSeen = now
		if open.count >= maxAuthFailedStreak {
			return true, s.closeAuthFailedStreakLocked()
		}
		if open.timer != nil {
			open.timer.Reset(window)
		}
		return true, nil
	}
	// A different key (or none open): close what was open, then start a streak
	// whose FIRST row the caller goes on to emit normally.
	summary := s.closeAuthFailedStreakLocked()
	fresh := &authFailedStreak{key: key, count: 1, firstSeen: now, lastSeen: now}
	// A streak closed by the timer has no request in flight, so its summary is
	// recorded under BaseCtx (daemon lifetime) — a cancelled request context
	// would drop the row on the floor.
	fresh.timer = time.AfterFunc(window, func() {
		s.authFailedStreakMu.Lock()
		ev := s.closeAuthFailedStreakLockedIf(key)
		s.authFailedStreakMu.Unlock()
		if ev != nil {
			s.recordAudit(s.cfg.BaseCtx, *ev)
		}
	})
	s.authFailedStreak = fresh
	return false, summary
}

// FlushAuthFailedStreak closes whatever streak is still open and records its
// summary row. Called at daemon shutdown (cmd/wardynd's serveAndShutdown, after
// httpSrv.Shutdown has stopped accepting requests and before the audit sinks are
// closed), mirroring the proxy's flushPrivateIPMemo at the same point in its own
// stop — and for the same reason: a streak closes only on a different key, the
// 1000-cap, or the 5m gap timer, so a restart during a steady drip lost up to 999
// refusals' COUNT. The opening row is already in the trail, so what was lost was
// only the volume — during exactly the rollout this instrument exists to measure.
//
// The timer could not have saved it either: a streak's AfterFunc records under
// BaseCtx, and by shutdown BaseCtx is already cancelled. So this records under
// context.WithoutCancel(BaseCtx) — the daemon's values (the audit recorder reads
// them), none of its cancellation.
//
// CEILING, stated rather than implied, same as the memo's: this is the ORDERLY
// stop. A SIGKILL, an OOM kill or a pod deleted out from under the process drops
// the open streak's count — never the refusals themselves, each of which opened
// its streak with a row that is already recorded.
func (s *Server) FlushAuthFailedStreak() {
	s.authFailedStreakMu.Lock()
	ev := s.closeAuthFailedStreakLocked()
	s.authFailedStreakMu.Unlock()
	if ev == nil {
		return
	}
	base := s.cfg.BaseCtx
	if base == nil {
		base = context.Background()
	}
	s.recordAudit(context.WithoutCancel(base), *ev)
}

// closeAuthFailedStreakLocked closes the open streak and returns the summary row
// to record, or nil when there is nothing to summarize (no streak, or a streak of
// exactly one row — which the trail already carries verbatim). Caller holds the
// lock.
func (s *Server) closeAuthFailedStreakLocked() *types.AuditEvent {
	open := s.authFailedStreak
	s.authFailedStreak = nil
	if open == nil {
		return nil
	}
	if open.timer != nil {
		open.timer.Stop()
	}
	if open.count < 2 {
		return nil
	}
	ev := s.auditEvent(nil, types.ActorSystem, open.key.actor, "auth.failed", open.key.target,
		"failure", mustJSON(map[string]any{
			"reason":     open.key.reason,
			"count":      open.count,
			"first_seen": open.firstSeen.UTC().Format(time.RFC3339),
			"last_seen":  open.lastSeen.UTC().Format(time.RFC3339),
		}))
	ev.SourceIP = open.key.sourceIP
	return &ev
}

// closeAuthFailedStreakLockedIf closes the open streak only if it is still the
// one the caller means — the timer fires against a key that a newer refusal may
// already have replaced. Caller holds the lock.
func (s *Server) closeAuthFailedStreakLockedIf(key authFailedKey) *types.AuditEvent {
	if s.authFailedStreak == nil || s.authFailedStreak.key != key {
		return nil
	}
	return s.closeAuthFailedStreakLocked()
}
