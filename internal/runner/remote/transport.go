// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"sync"

	"github.com/google/uuid"

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
	mu    sync.Mutex
	peer  *runnerwire.Peer
	h     Handler
	ready bool
	// stated: the attached session's final STATE page reached the org, or the session was resumed.
	// Past judging that page the org refuses no session for a queued action (requireFresh).
	stated bool
	// runner, view and pt route a call to the replica that holds the runner's session while this one holds none,
	// and answer Online from the holder view (UsePeer). Nil keeps every call and answer local.
	runner uuid.UUID
	view   *HolderView
	pt     *PeerTransport
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
	l.ready = true
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

// HeldHere reports whether this replica holds the runner's session, ready.
func (l *Link) HeldHere() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ready && l.peer != nil && l.peer.Online()
}

// Online reports whether the runner's session is ready here or, by the holder view, on another replica.
func (l *Link) Online() bool {
	if l.HeldHere() {
		return true
	}
	l.mu.Lock()
	v, id := l.view, l.runner
	l.mu.Unlock()
	return v != nil && v.Online(id)
}

// UsePeer routes this runner's calls through the replica that holds its session whenever this replica holds
// none, and answers Online from v as well. Call it once, before the Link is used.
func (l *Link) UsePeer(runnerID uuid.UUID, v *HolderView, pt *PeerTransport) {
	l.mu.Lock()
	l.runner, l.view, l.pt = runnerID, v, pt
	l.mu.Unlock()
}

// holder is the holder view's row for the runner, when UsePeer set a view.
func (l *Link) holder() (PeerRoute, bool) {
	l.mu.Lock()
	v, id := l.view, l.runner
	l.mu.Unlock()
	return v.Holder(id)
}

func (l *Link) peerTransport() *PeerTransport {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pt
}

// Call sends one call on this replica's session, or, with none here and UsePeer set, through the holder of the
// runner's session, read at this call.
func (l *Link) Call(ctx context.Context, method string, args, result any, opts ...runnerwire.CallOption) error {
	p := l.current()
	if p == nil || !l.HeldHere() {
		if pt := l.peerTransport(); pt != nil {
			return pt.call(ctx, l.runner, method, args, result, opts...)
		}
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

// session is the calls and streams of one runner session. A byte stream belongs to the session whose
// call opened it: its claim and every control after it (Resize, Wait, Close) go to that session.
type session interface {
	Call(ctx context.Context, method string, args, result any, opts ...runnerwire.CallOption) error
	TakeStream(id uint32) (*runnerwire.Stream, error)
}

// pinSession returns the current session for the life of the streams it opens. Once a reconnect replaces it,
// its calls answer errOffline and never reach the replacement, whose stream ids may repeat the old ones. With no
// session here and UsePeer set, it reads the holder once and pins that replica's session (peerPinned).
func (l *Link) pinSession(ctx context.Context) (session, error) {
	p := l.current()
	if p == nil || !l.HeldHere() {
		if pt := l.peerTransport(); pt != nil {
			if route, ok := pt.route(ctx, l.runner); ok {
				return &peerPinned{pt: pt, runner: l.runner, route: route}, nil
			}
		}
		return nil, errOffline
	}
	return pinnedPeer{l: l, p: p}, nil
}

// pinnedPeer is one peer of l, reachable only while it is still l's current session.
type pinnedPeer struct {
	l *Link
	p *runnerwire.Peer
}

func (s pinnedPeer) Call(ctx context.Context, method string, args, result any, opts ...runnerwire.CallOption) error {
	if s.l.current() != s.p || !s.l.HeldHere() {
		return errOffline
	}
	return s.p.Call(ctx, method, args, result, opts...)
}

func (s pinnedPeer) TakeStream(id uint32) (*runnerwire.Stream, error) {
	if s.l.current() != s.p {
		return nil, errOffline
	}
	return s.p.TakeStream(id)
}

// StreamCount is the byte streams the current session holds (0 when none).
func (l *Link) StreamCount() int {
	if p := l.current(); p != nil {
		return p.StreamCount()
	}
	return 0
}
