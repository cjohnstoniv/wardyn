// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (h *Hub) configureActivity(ctx context.Context, cfg *runnerwire.PeerConfig, row types.Runner, a Admission, session *hubSession, peer func() *runnerwire.Peer) {
	control := cfg.OnControlChecked
	cfg.OnControlChecked = func(frame runnerwire.Frame) error {
		if frame.Type != runnerwire.TypeCurrentActivity {
			return control(frame)
		}
		var activity runnerwire.CurrentActivity
		if err := runnerwire.Unmarshal(frame.Payload, &activity); err != nil {
			return err
		}
		if activity.RunID == uuid.Nil || frame.Seq != 0 {
			return fmt.Errorf("%w: invalid current activity", runnerwire.ErrBadFrame)
		}
		return h.currentActivity(ctx, row, a, session, peer(), activity.RunID)
	}
}

func (h *Hub) currentActivity(ctx context.Context, row types.Runner, a Admission, session *hubSession, peer *runnerwire.Peer, runID uuid.UUID) error {
	// This is the same barrier used by replacement, revocation and expiry.
	// Keep it through the checked DB callback so an old peer cannot commit
	// activity after its connection has been superseded.
	session.mu.Lock()
	defer session.mu.Unlock()
	h.mu.Lock()
	currentSession := !h.closed && h.sessions[row.ID] == session
	h.mu.Unlock()
	if !currentSession || !session.ready.Load() || session.peer != peer || peer == nil || !peer.Online() || a.Activity == nil {
		return runnerwire.ErrNotReady
	}
	current, err := h.cfg.Lookup(ctx, row.ID)
	if err != nil || current.ID != row.ID || current.State != types.RunnerClaimed || current.RevokedAt != nil ||
		current.Owner == "" || current.Owner != row.Owner || string(current.PublicKey) != string(row.PublicKey) || current.OrgURLSHA256 != row.OrgURLSHA256 {
		return runnerwire.ErrAuth
	}
	return a.Activity(ctx, runID)
}
