// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The bounds on the failure rows the device routes write — auth.fail's pair
// (authFailedLimiter: how fast; auth_failed_coalesce.go: how many of the same
// thing) applied per stream. The anonymous enrolment route is ONE stream,
// charged to the process-wide bucket auth.fail pays. Each enrolled device's
// ingest refusals are a stream of their own, charged to a per-device bucket,
// so one member's laptop spends only its own budget.
package api

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The per-device bucket for device.audit.ingest failure rows. A forwarder
// retrying one refused batch on its backoff is a single streak and pays twice
// in total; this bounds a device that cycles reasons to keep opening new ones.
const (
	ingestFailureRatePerSec = 1.0 / 60
	ingestFailureBurst      = 5.0
	ingestFailureMaxDevices = 4096
)

// failureStreams folds identical consecutive failure rows within a stream into
// the stream's first row plus one summary row carrying the count — the streak
// auth_failed_coalesce.go keeps for auth.fail, once per stream. A different
// key, maxAuthFailedStreak refusals, or AuditCoalesceWindow of quiet closes a
// streak; a streak of one needs no summary, its own row is the record.
type failureStreams struct {
	mu   sync.Mutex
	open map[string]*failureStreak
}

type failureStreak struct {
	key, reason         string
	first               types.AuditEvent
	count               int
	firstSeen, lastSeen time.Time
	timer               *time.Timer
	emit                func(context.Context, *types.AuditEvent)
}

// fold reports whether ev — the row about to be written for stream — is
// absorbed into the stream's open streak, and returns the summary of a streak
// it closed, which the caller emits BEFORE its own row so the trail keeps the
// order things happened in. emit is how a streak the gap timer closes reaches
// the trail; it must accept nil.
func (f *failureStreams) fold(s *Server, stream, key, reason string, ev types.AuditEvent,
	emit func(context.Context, *types.AuditEvent)) (absorbed bool, summary *types.AuditEvent) {
	window := s.cfg.AuditCoalesceWindow
	if window <= 0 {
		return false, nil
	}
	now := s.cfg.Now()
	f.mu.Lock()
	defer f.mu.Unlock()
	if open := f.open[stream]; open != nil && open.key == key {
		open.count++
		open.lastSeen = now
		if open.count >= maxAuthFailedStreak {
			return true, f.closeLocked(s, stream)
		}
		open.timer.Reset(window)
		return true, nil
	}
	summary = f.closeLocked(s, stream)
	fresh := &failureStreak{key: key, reason: reason, first: ev, count: 1, firstSeen: now, lastSeen: now, emit: emit}
	fresh.timer = time.AfterFunc(window, func() {
		f.mu.Lock()
		var closed *types.AuditEvent
		if f.open[stream] == fresh {
			closed = f.closeLocked(s, stream)
		}
		f.mu.Unlock()
		emit(s.cfg.BaseCtx, closed)
	})
	if f.open == nil {
		f.open = map[string]*failureStreak{}
	}
	f.open[stream] = fresh
	return false, summary
}

// closeLocked ends stream's open streak and returns its summary row, or nil
// for no streak or a streak of one. Caller holds f.mu.
func (f *failureStreams) closeLocked(s *Server, stream string) *types.AuditEvent {
	open := f.open[stream]
	if open == nil {
		return nil
	}
	delete(f.open, stream)
	open.timer.Stop()
	if open.count < 2 {
		return nil
	}
	ev := s.auditEvent(open.first.RunID, open.first.ActorType, open.first.Actor, open.first.Action,
		open.first.Target, open.first.Outcome, mustJSON(map[string]any{
			"reason":     open.reason,
			"count":      open.count,
			"first_seen": open.firstSeen.UTC().Format(time.RFC3339),
			"last_seen":  open.lastSeen.UTC().Format(time.RFC3339),
		}))
	ev.SourceIP = open.first.SourceIP
	return &ev
}

// flush closes every open streak and emits its summary — the shutdown path
// FlushAuthFailedStreak runs, for the reason it gives.
func (f *failureStreams) flush(s *Server, ctx context.Context) {
	type pending struct {
		ev   *types.AuditEvent
		emit func(context.Context, *types.AuditEvent)
	}
	var out []pending
	f.mu.Lock()
	for stream, open := range f.open {
		if ev := f.closeLocked(s, stream); ev != nil {
			out = append(out, pending{ev, open.emit})
		}
	}
	f.mu.Unlock()
	for _, p := range out {
		p.emit(ctx, p.ev)
	}
}

// emitEnrolFailure writes one device.enrol failure row (an opening row or a
// streak's summary) if the shared auth-failure bucket has a token — the route
// is anonymous, so an unmetered row would be an unauthenticated write into
// the append-only log. A drop is counted with auth.fail's.
func (s *Server) emitEnrolFailure(ctx context.Context, ev *types.AuditEvent) {
	if ev == nil {
		return
	}
	if !s.authFailedLimiter.allow(s.cfg.Now()) {
		s.metrics.authFailedSuppressedInc()
		return
	}
	s.recordAudit(ctx, *ev)
}

// ingestFailureEmitter is emitEnrolFailure for one device's stream, charged to
// that device's own bucket.
func (s *Server) ingestFailureEmitter(deviceID uuid.UUID) func(context.Context, *types.AuditEvent) {
	return func(ctx context.Context, ev *types.AuditEvent) {
		if ev == nil {
			return
		}
		if !s.ingestFailureLimiter.allow(deviceID.String(), s.cfg.Now()) {
			s.metrics.deviceIngestSuppressedInc()
			return
		}
		s.recordAudit(ctx, *ev)
	}
}
