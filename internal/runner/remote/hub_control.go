// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// Revoke ends the active session after H3 has durably revoked the runner row.
// Stored-row verification on every new connection enforces the durable gate;
// this method alone is intentionally not a substitute for that transaction.
func (h *Hub) Revoke(ctx context.Context, id uuid.UUID) error {
	return h.endWithControl(ctx, id, runnerwire.TypeRevoked, "runner_revoked")
}

// Goaway forces the next connection through full admission, for replica drain
// or an administrative session reset. It does not revoke the registered key.
func (h *Hub) Goaway(ctx context.Context, id uuid.UUID) error {
	return h.endWithControl(ctx, id, runnerwire.TypeGoaway, "fresh_session_required")
}

// GoawayAll sends every session Goaway, as after runners are switched off.
func (h *Hub) GoawayAll(ctx context.Context) error {
	h.mu.Lock()
	ids := slices.Collect(maps.Keys(h.sessions))
	h.mu.Unlock()
	errs := make([]error, 0, len(ids))
	for _, id := range ids {
		errs = append(errs, h.Goaway(ctx, id))
	}
	return errors.Join(errs...)
}

// Online reports whether id's current session reached its own READY and its
// peer is still up. A session in transition holds its slot and reads offline
// rather than making the caller wait for it.
func (h *Hub) Online(id uuid.UUID) bool {
	h.mu.Lock()
	s := h.sessions[id]
	h.mu.Unlock()
	if s == nil || !s.mu.TryLock() {
		return false
	}
	defer s.mu.Unlock()
	return s.peer != nil && s.peer.Online() && s.readyFor(s.id)
}

func (h *Hub) endWithControl(ctx context.Context, id uuid.UUID, kind runnerwire.Type, reason string) error {
	h.mu.Lock()
	s := h.sessions[id]
	h.mu.Unlock()
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.peer == nil {
		return nil
	}
	s.readyID.Store(nil)
	s.link.suspend(s.peer)
	defer s.peer.Close()
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	// The bound closes the link: a send queued behind a stalled write returns
	// only once the link is down.
	defer context.AfterFunc(ctx, s.peer.Close)()
	payload, err := runnerwire.Marshal(runnerwire.Reason{Reason: reason})
	if err != nil {
		return err
	}
	// Best effort: the session is suspended and the deferred Close ends the link either way. A peer
	// inside the resume window may be dead, so its Send can fail with DeadlineExceeded, net.ErrClosed or
	// ErrRunnerOffline, and none of those leaves the runner any less revoked.
	if err := s.peer.Send(ctx, runnerwire.Frame{Type: kind, Payload: payload}); err != nil {
		slog.DebugContext(ctx, "runner control frame not delivered", slog.String("runner", id.String()), slog.String("reason", reason), slog.Any("err", err))
	}
	return nil
}
