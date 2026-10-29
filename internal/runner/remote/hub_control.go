// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// Revoke ends the active session after H3 has durably revoked the runner row.
// Stored-row verification on every new connection enforces the durable gate;
// this method alone is intentionally not a substitute for that transaction.
func (h *Hub) Revoke(ctx context.Context, id uuid.UUID) error {
	return h.endWithControl(ctx, id, runnerwire.TypeRevoked, "runner_revoked", nil)
}

// Goaway forces the next connection through full admission, for replica drain
// or an administrative session reset. It does not revoke the registered key.
func (h *Hub) Goaway(ctx context.Context, id uuid.UUID) error {
	return h.endWithControl(ctx, id, runnerwire.TypeGoaway, "fresh_session_required", nil)
}

// GoawayAll sends every session Goaway, as after runners are switched off.
func (h *Hub) GoawayAll(ctx context.Context) error {
	h.mu.Lock()
	ids := slices.Collect(maps.Keys(h.sessions))
	h.mu.Unlock()
	// Concurrently: each Goaway may wait out its own 1 s bound on a stalled link, and the caller
	// (the runners settings write) waits for all of them.
	errs := make([]error, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Go(func() { errs[i] = h.Goaway(ctx, id) })
	}
	wg.Wait()
	return errors.Join(errs...)
}

// Online reports whether id's current session reached its own READY and its
// peer is still up. A session in transition holds its slot and reads offline
// rather than making the caller wait for it. A current-activity check also
// holds the slot, across a store round trip, but moves nothing: the peer it
// verified is read from activityPeer, so a connected runner stays online.
func (h *Hub) Online(id uuid.UUID) bool {
	h.mu.Lock()
	s := h.sessions[id]
	h.mu.Unlock()
	if s == nil {
		return false
	}
	if !s.mu.TryLock() {
		p := s.activityPeer.Load()
		return p != nil && p.Online() && s.readyID.Load() != nil
	}
	defer s.mu.Unlock()
	return s.peer != nil && s.peer.Online() && s.readyFor(s.id)
}

// endWithControl ends id's session with a control frame. With only set, it ends the session only while it still
// serves that peer: a caller that decided on an older session's state must not end the one that replaced it.
func (h *Hub) endWithControl(ctx context.Context, id uuid.UUID, kind runnerwire.Type, reason string, only *runnerwire.Peer) error {
	h.mu.Lock()
	s := h.sessions[id]
	h.mu.Unlock()
	if s == nil {
		return nil
	}
	if !s.mu.TryLock() {
		if only == nil {
			// An admission holds the slot, perhaps stalled on a durable call or a dead link: end it now rather
			// than after its own timeouts.
			s.activityPeer.Store(nil) // a pending revoke reads offline, as it did before an activity check could hold the slot
			if abort := s.abort.Load(); abort != nil {
				(*abort)()
			}
		}
		// With only set, the holder is a reconnect that replaces the session this call was made for: wait
		// for it instead of cancelling it.
		s.mu.Lock()
	}
	defer s.mu.Unlock()
	if s.peer == nil || (only != nil && s.peer != only) {
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
