// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// HolderView is a snapshot of every fresh holder row, by runner id, so every replica gives one answer to
// whether a runner is online. A failed refresh empties it: an outage reads as offline elsewhere, never as a stale
// online. It answers "online" only; a call still reads the runner's row itself, so a moved session is a refusal,
// never a misroute.
type HolderView struct {
	self string
	read func(ctx context.Context) (map[uuid.UUID]PeerRoute, error)
	// OnRefresh, when set, runs after each successful refresh with the runners whose fresh row names another
	// replica, on the refresh's goroutine and with no lock held.
	OnRefresh func(ctx context.Context, elsewhere []uuid.UUID)

	mu   sync.Mutex
	rows map[uuid.UUID]PeerRoute
}

// NewHolderView is replica self's view over read, which returns every holder row seen in the last 30 seconds.
func NewHolderView(self string, read func(ctx context.Context) (map[uuid.UUID]PeerRoute, error)) *HolderView {
	return &HolderView{self: self, read: read}
}

// Refresh reads the rows again; on an error the view holds none.
func (v *HolderView) Refresh(ctx context.Context) error {
	rows, err := v.read(ctx)
	if err != nil {
		rows = nil
	}
	v.mu.Lock()
	v.rows = rows
	v.mu.Unlock()
	if err != nil {
		return err
	}
	if v.OnRefresh != nil {
		var elsewhere []uuid.UUID
		for id, r := range rows {
			if r.ReplicaID != v.self {
				elsewhere = append(elsewhere, id)
			}
		}
		v.OnRefresh(ctx, elsewhere)
	}
	return nil
}

// Online reports a fresh row for the runner that names another replica.
func (v *HolderView) Online(runnerID uuid.UUID) bool {
	r, ok := v.Holder(runnerID)
	return ok && r.ReplicaID != v.self
}

// Holder is the runner's fresh row as of the last refresh, whichever replica it names.
func (v *HolderView) Holder(runnerID uuid.UUID) (PeerRoute, bool) {
	if v == nil {
		return PeerRoute{}, false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	r, ok := v.rows[runnerID]
	return r, ok
}

// PeerCaps is the holder's answer to the door method caps: its Substrate's Caps.
type PeerCaps struct {
	Caps runnerwire.Caps `json:"caps"`
	OK   bool            `json:"ok"`
}

// peerCapsFor is how long a non-holder keeps the holder's caps answer for one session.
var peerCapsFor = 10 * time.Second

// peerCapsRead is one kept caps answer: the session it was read for and when.
type peerCapsRead struct {
	session uuid.UUID
	at      time.Time
	caps    runnerwire.Caps
	ok      bool
}

// holderCaps is the runner's caps as the holder of session route answers them, kept for peerCapsFor. No lock is
// held across the read.
func (s *Substrate) holderCaps(l *Link, route PeerRoute) (runnerwire.Caps, bool) {
	pt := l.peerTransport()
	now := pt.now()
	s.mu.Lock()
	kept := s.peerCaps
	s.mu.Unlock()
	if kept.session == route.SessionID && now.Sub(kept.at) < peerCapsFor {
		return kept.caps, kept.ok
	}
	ctx, cancel := context.WithTimeout(context.Background(), peerLookupTimeout)
	defer cancel()
	var res PeerCaps
	if err := pt.CallHolder(ctx, l.runner, PeerMethodCaps, nil, &res); err != nil {
		return runnerwire.Caps{}, false
	}
	s.mu.Lock()
	s.peerCaps = peerCapsRead{session: route.SessionID, at: now, caps: res.Caps, ok: res.OK}
	s.mu.Unlock()
	return res.Caps, res.OK
}

// peerRefreshEvery is how often the hub refreshes the holder view; it touches the holder rows of the sessions
// whose link is up on every second tick.
var peerRefreshEvery = 5 * time.Second

// claimHolder records this replica as the holder of the runner's new session. A failed claim is retried by the
// next touch, which claims again for a session with no row.
func (h *Hub) claimHolder(ctx context.Context, runnerID uuid.UUID, session string) {
	sid, err := uuid.Parse(session)
	if h.cfg.Peer == nil || err != nil {
		return
	}
	_, _ = h.cfg.Peer.Claim(ctx, runnerID, sid)
}

// dropHolder deletes the ended session's row, only while it still names that session.
func (h *Hub) dropHolder(runnerID uuid.UUID, session string) {
	sid, err := uuid.Parse(session)
	if h.cfg.Peer == nil || err != nil {
		return
	}
	_ = h.cfg.Peer.Drop(h.ctx, runnerID, sid)
}

// peerLoop touches the holder rows of every session whose link is up, and claims again for one the touch did not
// return (another replica's drop, or a restore deleted its row); the claim is still gated on the inventory session.
// A session in its resume window with no link is not touched, so its row ages out of every route. Every tick also
// refreshes the holder view, the first one at once.
func (h *Hub) peerLoop() {
	t := time.NewTicker(peerRefreshEvery)
	defer t.Stop()
	h.refreshView()
	for tick := 1; ; tick++ {
		select {
		case <-h.ctx.Done():
			return
		case <-t.C:
			if tick%2 == 0 {
				h.touchHolders(h.ctx)
			}
			h.refreshView()
		}
	}
}

func (h *Hub) refreshView() {
	ctx, cancel := context.WithTimeout(h.ctx, peerRefreshEvery)
	defer cancel()
	h.cfg.Peer.Refresh(ctx)
}

func (h *Hub) touchHolders(ctx context.Context) {
	held := h.heldSessions()
	if len(held) == 0 {
		return
	}
	sessions := make([]uuid.UUID, 0, len(held))
	for sid := range held {
		sessions = append(sessions, sid)
	}
	tctx, cancel := context.WithTimeout(ctx, 2*peerRefreshEvery)
	defer cancel()
	touched, err := h.cfg.Peer.Touch(tctx, sessions)
	if err != nil {
		return
	}
	for _, sid := range touched {
		delete(held, sid)
	}
	for sid, runnerID := range held {
		_, _ = h.cfg.Peer.Claim(tctx, runnerID, sid)
	}
}
