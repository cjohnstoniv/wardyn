// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
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
	s.ready.Store(false)
	s.link.suspend(s.peer)
	defer s.peer.Close()
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	payload, err := runnerwire.Marshal(runnerwire.Reason{Reason: reason})
	if err != nil {
		return err
	}
	return s.peer.Send(ctx, runnerwire.Frame{Type: kind, Payload: payload})
}
