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
	"net"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// authFailedKey is what makes two auth.failed rows "identical" for coalescing:
// the boundary that refused, its bounded reason, the request path, and the peer
// IP.
//
// THE PEER IP, WITHOUT THE EPHEMERAL PORT (V1-r2-lensS #3). r.RemoteAddr is
// host:port, and the port is a fresh number on every TCP connection — so keying
// on it made the whole fold a no-op for exactly the clients that matter: a
// scanner, an ingress, or any HTTP client without keep-alive opens a connection
// per request, every request lands under its own key, and nothing coalesces. The
// drip B5 was built for (one sidecar, one long-lived connection) folded; the
// flood did not.
//
// sourceIP is in the key because on a single-tenant or loopback deployment it
// genuinely separates principals — but it is NOT the defence here and
// THREAT-MODEL.md says so: behind a Kubernetes ingress or load balancer every
// client shares one peer address (RealIP is deliberately not installed, see
// routes.go), so a credential-stuffing burst arrives under ONE key. What keeps
// such a burst from collapsing into a single row is the pair of bounds below —
// the window and the max count — plus the rate limiter every summary emit is
// charged to (recordAuthFailedSummary) — not this field.
type authFailedKey struct {
	actor, reason, target, sourceIP string
}

// peerIP strips the ephemeral port off a net/http RemoteAddr, leaving the bare
// peer address (IPv6 loses its brackets, which is what SplitHostPort does). A
// RemoteAddr with no port — a test double, a non-TCP listener — is returned
// unchanged; this is a coalescing key, so an unparseable value only has to be
// consistent.
func peerIP(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// maxAuthFailedStreak caps one summary row's count. Past it the streak closes and
// a new one starts, so a tight loop produces a row every 1000 refusals rather
// than one row whenever it happens to stop — an upper bound on how much volume a
// single row is allowed to stand for.
const maxAuthFailedStreak = 1000

// authFailedStreak is the ONE open run of identical consecutive refusals.
// Consecutive, so there is at most one: any row with a different key closes it.
type authFailedStreak struct {
	key authFailedKey
	// sourceAddr is the opening refusal's full RemoteAddr (host:port). The KEY
	// holds the port-less peer IP; the summary ROW carries this, so source_ip
	// keeps the host:port shape every other audit row in the package uses and a
	// streak's summary stays readable next to the row that opened it.
	sourceAddr string
	count      int
	firstSeen  time.Time
	lastSeen   time.Time
	timer      *time.Timer
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
func (s *Server) coalesceAuthFailed(actor, reason, target, remoteAddr string) (bool, *types.AuditEvent) {
	window := s.cfg.AuditCoalesceWindow
	if window <= 0 {
		return false, nil // disabled: every refusal is its own row, exactly as before
	}
	key := authFailedKey{actor: actor, reason: reason, target: target, sourceIP: peerIP(remoteAddr)}
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
	fresh := &authFailedStreak{key: key, sourceAddr: remoteAddr, count: 1, firstSeen: now, lastSeen: now}
	// A streak closed by the timer has no request in flight, so its summary is
	// recorded under BaseCtx (daemon lifetime) — a cancelled request context
	// would drop the row on the floor.
	fresh.timer = time.AfterFunc(window, func() {
		s.authFailedStreakMu.Lock()
		ev := s.closeAuthFailedStreakLockedIf(key)
		s.authFailedStreakMu.Unlock()
		s.recordAuthFailedSummary(s.cfg.BaseCtx, ev)
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
// its streak with a row that is already recorded. So does a shutdown that lands
// mid-flood with the rate limiter empty: this row pays the limiter like every
// other summary (recordAuthFailedSummary), and a refused one is counted in the
// suppressed series instead — the audit trail's bound outranks a count.
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
	s.recordAuthFailedSummary(context.WithoutCancel(base), ev)
}

// recordAuthFailedSummary is the ONE way a closing streak's summary row reaches
// the trail, and it is CHARGED TO THE SAME RATE LIMITER a first row pays
// (V1-r2-lensS #1). Before this the summary went straight to recordAudit: the
// coalescer closes a streak on every KEY CHANGE and summarises any streak of 2 or
// more, so an unauthenticated client alternating two paths on one connection
// produced one unmetered row per two requests — 400 refusals at a single frozen
// instant recorded 204 rows against the limiter's ceiling of 5. A structural
// bound that ADDS an unbounded emit path is not a bound.
//
// A refused summary is DROPPED and counted in the same suppressed series every
// other dropped auth.failed row is (the folded ones, the rate-limited ones): the
// count it carried is lost, the refusals themselves are not — each one either
// opened its own recorded row or was already counted as suppressed.
//
// Nothing changes for the drip this instrument exists for: a real streak is one
// key, so it pays the limiter twice in total (its opening row and its summary)
// however many refusals it folds.
func (s *Server) recordAuthFailedSummary(ctx context.Context, ev *types.AuditEvent) {
	if ev == nil {
		return
	}
	if !s.authFailedLimiter.allow(s.cfg.Now()) {
		s.metrics.authFailedSuppressedInc()
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.recordAudit(ctx, *ev)
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
	ev.SourceIP = open.sourceAddr
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
