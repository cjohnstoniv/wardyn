// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"sync"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// Transport is how a Substrate reaches its runner: one session at a time,
// replaced on reconnect. Link is the implementation over a runnerwire.Conn, the
// in-memory loopback and the WebSocket alike.
type Transport interface {
	// Online is true while an authenticated session is up.
	Online() bool
	Call(ctx context.Context, method string, args, result any, opts ...runnerwire.CallOption) error
	// TakeStream claims a byte stream the runner opened for an in-flight call.
	TakeStream(id uint32) (*runnerwire.Stream, error)
	// SetHandler is where the transport delivers the runner's events and
	// unsolicited streams. New calls it once.
	SetHandler(h Handler)
}

// Handler receives what a runner sends on its own.
type Handler interface {
	HandleEvent(ev runnerwire.Event)
	HandleOpen(s *runnerwire.Stream, o runnerwire.Open) error
}

// Link is the Transport over runnerwire.Conns. Attach starts a session on a
// connection whose handshake has already run; a later Attach replaces it.
type Link struct {
	mu   sync.Mutex
	peer *runnerwire.Peer
	h    Handler
}

var _ Transport = (*Link)(nil)

func (l *Link) SetHandler(h Handler) {
	l.mu.Lock()
	l.h = h
	l.mu.Unlock()
}

// Attach runs the org side of a session over conn until the link ends, and
// returns its peer so the caller can send session-level frames and watch Done.
func (l *Link) Attach(ctx context.Context, conn runnerwire.Conn) *runnerwire.Peer {
	l.mu.Lock()
	h := l.h
	old := l.peer
	cfg := runnerwire.PeerConfig{Org: true}
	if h != nil {
		cfg.OnEvent, cfg.OnOpen = h.HandleEvent, h.HandleOpen
	}
	p := runnerwire.NewPeer(conn, cfg)
	l.peer = p
	l.mu.Unlock()
	if old != nil {
		old.Close()
	}
	go func() { _ = p.Run(ctx) }()
	return p
}

func (l *Link) current() *runnerwire.Peer {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.peer
}

func (l *Link) Online() bool {
	p := l.current()
	return p != nil && p.Online()
}

func (l *Link) Call(ctx context.Context, method string, args, result any, opts ...runnerwire.CallOption) error {
	p := l.current()
	if p == nil {
		return errOffline
	}
	return p.Call(ctx, method, args, result, opts...)
}

func (l *Link) TakeStream(id uint32) (*runnerwire.Stream, error) {
	p := l.current()
	if p == nil {
		return nil, errOffline
	}
	return p.TakeStream(id)
}

// StreamCount is the byte streams the current session holds (0 when none).
func (l *Link) StreamCount() int {
	if p := l.current(); p != nil {
		return p.StreamCount()
	}
	return 0
}
