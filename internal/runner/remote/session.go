// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// AttachSession prepares an authenticated session without admitting calls or
// unsolicited streams. Start drives it; Ready admits dispatch after replay or
// the full PENDING/STATE/spool/READY sequence. Attach retains its loopback API.
func (l *Link) AttachSession(conn runnerwire.Conn, cfg runnerwire.PeerConfig) *runnerwire.Peer {
	l.mu.Lock()
	defer l.mu.Unlock()
	cfg.Org = true
	if l.h != nil {
		checked := cfg.OnEventChecked
		h := l.h
		cfg.OnEventChecked = func(ev runnerwire.Event) error {
			if checked != nil {
				if err := checked(ev); err != nil {
					return err
				}
			}
			h.HandleEvent(ev)
			return nil
		}
		cfg.OnOpen = func(s *runnerwire.Stream, o runnerwire.Open) error {
			if !l.Online() {
				return runnerwire.ErrNotReady
			}
			return h.HandleOpen(s, o)
		}
	}
	old := l.peer
	l.peer = runnerwire.NewPeer(conn, cfg)
	l.ready = false
	if old != nil {
		old.Close()
	}
	return l.peer
}

// Ready admits only the exact peer that completed admission, never a stale
// connection's READY racing a newer attachment.
func (l *Link) Ready(peer *runnerwire.Peer) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.peer != peer || !peer.Online() {
		return fmt.Errorf("%w: superseded session", runnerwire.ErrNotReady)
	}
	l.ready = true
	return nil
}

// Close rejects dispatch immediately and ends the active peer.
func (l *Link) Close() {
	l.mu.Lock()
	l.ready = false
	p := l.peer
	l.mu.Unlock()
	if p != nil {
		p.Close()
	}
}

// suspend closes the dispatch gate before a terminal control is written.
func (l *Link) suspend(peer *runnerwire.Peer) {
	l.mu.Lock()
	if l.peer == peer {
		l.ready = false
	}
	l.mu.Unlock()
}

// Send carries LEASE and administrative control on the active session.
func (l *Link) Send(ctx context.Context, frame runnerwire.Frame) error {
	p := l.current()
	if p == nil || !l.Online() {
		return errOffline
	}
	return p.Send(ctx, frame)
}
