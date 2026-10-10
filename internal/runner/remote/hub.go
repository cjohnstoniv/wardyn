// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Admission is supplied by the org's durable routing/reconciliation layer.
// Pending, State and Event are required: missing durable reconciliation must
// not silently admit a runner. Substrate and Link must be the same pair.
type Admission struct {
	Substrate *Substrate
	Link      *Link
	Pending   func(context.Context) ([]runnerwire.PendingAction, error)
	State     func(context.Context, []runnerwire.RunState) error
	Event     func(context.Context, runnerwire.Event) error
	// Activity checks current run ownership/lifecycle in its database transaction.
	// Nil refuses live activity; historical EVENT ingestion never calls it.
	Activity func(context.Context, uuid.UUID) error
}

// HubConfig binds proof of possession to stored identity and org admission.
type HubConfig struct {
	OrgURL string
	Lookup func(context.Context, uuid.UUID) (types.Runner, error)
	Admit  func(context.Context, types.Runner) (Admission, error)
	// Connected and Disconnected are audit/lifecycle callbacks. A resumed
	// session emits neither; Disconnected waits for the resume window.
	Connected    func(context.Context, uuid.UUID) error
	Disconnected func(context.Context, uuid.UUID) error
}

type hubSession struct {
	mu        sync.Mutex
	id        string
	peer      *runnerwire.Peer
	link      *Link
	ended     time.Time
	ready     atomic.Bool
	timer     *time.Timer
	connected bool
}

// Hub retains only authenticated sessions, for the bounded resume window.
// Connections and callbacks belong to ctx, the daemon lifetime.
type Hub struct {
	ctx      context.Context
	cfg      HubConfig
	mu       sync.Mutex
	sessions map[uuid.UUID]*hubSession
	closed   bool
}

// NewHub does not enable dispatch when Admit is absent.
func NewHub(ctx context.Context, cfg HubConfig) *Hub {
	h := &Hub{ctx: ctx, cfg: cfg, sessions: map[uuid.UUID]*hubSession{}}
	context.AfterFunc(ctx, h.Close)
	return h
}

// Serve authenticates the route's runner before resolving or attaching its
// substrate. A reconnect always checks the stored key and claim state anew.
func (h *Hub) Serve(ctx context.Context, conn runnerwire.Conn, id uuid.UUID) error {
	defer conn.Close()
	if federation.CheckOrgURL(h.cfg.OrgURL) != nil || h.cfg.Lookup == nil {
		return errors.New("runner hub requires the public organisation URL and identity store")
	}
	row, err := h.cfg.Lookup(ctx, id)
	if err != nil || row.ID != id {
		return runnerwire.ErrAuth
	}
	auth, err := runnerwire.AuthenticateRunner(ctx, conn, row, h.cfg.OrgURL)
	if err != nil {
		return err
	}
	current, err := h.cfg.Lookup(ctx, id)
	if err != nil || current.ID != id || string(current.PublicKey) != string(row.PublicKey) || current.OrgURLSHA256 != row.OrgURLSHA256 {
		return runnerwire.ErrAuth
	}
	if current.ClaimExpired(time.Now()) {
		return runnerwire.ErrAuth
	}
	if current.State == types.RunnerRevoked {
		_ = runnerwire.WriteControl(ctx, conn, runnerwire.TypeRevoked, runnerwire.Reason{Reason: "runner_revoked"})
		return runnerwire.ErrRevoked
	}
	if current.State != types.RunnerClaimed {
		// Proof of possession succeeds, but no Link is attached and no run data
		// is sent. A subsequent claim takes effect on a fresh connection.
		ok := runnerwire.AuthOK{SessionID: uuid.NewString(), PingInterval: runnerwire.PingInterval}
		if err := runnerwire.WriteControl(ctx, conn, runnerwire.TypeAuthOK, ok); err != nil {
			return err
		}
		return runnerwire.WriteControl(ctx, conn, runnerwire.TypeGoaway, runnerwire.Reason{Reason: "runner_unclaimed"})
	}
	if h.cfg.Admit == nil {
		return errors.New("runner hub admission is not configured")
	}
	a, err := h.cfg.Admit(ctx, current)
	if err != nil {
		return err
	}
	if a.Substrate == nil || a.Link == nil || a.Substrate.RunnerID() != auth.RunnerID || a.Substrate.t != a.Link ||
		a.Pending == nil || a.State == nil || a.Event == nil {
		return errors.New("runner hub admission is incomplete or bound to another runner")
	}
	return h.serveClaimed(ctx, conn, current, auth, a)
}

func (h *Hub) lockSession(id uuid.UUID) (*hubSession, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.ctx.Err() != nil {
		return nil, runnerwire.ErrLinkDown
	}
	if h.sessions[id] == nil {
		h.sessions[id] = &hubSession{}
	}
	s := h.sessions[id]
	// TryLock cannot wait while holding the map lock: expiry takes these
	// locks in the other order. Acquiring both closes the lookup/expiry gap.
	if !s.mu.TryLock() {
		return nil, errors.New("runner connection is already being established")
	}
	return s, nil
}

func (h *Hub) serveClaimed(ctx context.Context, conn runnerwire.Conn, row types.Runner, auth runnerwire.Auth, a Admission) error {
	s, err := h.lockSession(row.ID)
	if err != nil {
		return err
	}
	peer, err := h.start(ctx, conn, row, auth, a, s)
	s.mu.Unlock()
	if err != nil {
		if peer != nil {
			h.ended(row.ID, s, peer)
		} else {
			h.dropEmpty(row.ID, s)
		}
		return err
	}
	select {
	case <-peer.Done():
	case <-ctx.Done():
		peer.Close()
	case <-h.ctx.Done():
		peer.Close()
	}
	h.ended(row.ID, s, peer)
	return peer.Err()
}

func (h *Hub) dropEmpty(id uuid.UUID, s *hubSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.peer != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sessions[id] == s {
		delete(h.sessions, id)
	}
}

// start holds the runner's slot while authenticating a connection transition.
func (h *Hub) start(ctx context.Context, conn runnerwire.Conn, row types.Runner, auth runnerwire.Auth, a Admission, s *hubSession) (*runnerwire.Peer, error) {
	id := row.ID
	// Admission may wait on durable routing. Re-read under the session lock
	// so a revocation that found no attached peer cannot leave a stale row
	// poised to attach afterwards.
	current, err := h.cfg.Lookup(ctx, id)
	if err != nil || current.ID != id || current.State != types.RunnerClaimed ||
		string(current.PublicKey) != string(row.PublicKey) || current.OrgURLSHA256 != row.OrgURLSHA256 {
		return nil, runnerwire.ErrAuth
	}
	if s.peer != nil && s.peer.Online() {
		return nil, errors.New("runner already has an active connection")
	}
	cfg, resumed, err := h.resume(ctx, s, auth.Resume)
	if err != nil {
		_ = runnerwire.WriteControl(ctx, conn, runnerwire.TypeGoaway, runnerwire.Reason{Reason: "resume_gap"})
		return nil, err
	}
	var pending []runnerwire.PendingAction
	if !resumed {
		pending, err = a.Pending(ctx)
		if err != nil {
			return nil, err
		}
		if s.connected && h.cfg.Disconnected != nil {
			if err := h.cfg.Disconnected(ctx, id); err != nil {
				return nil, err
			}
			s.connected = false
		}
		s.id = uuid.NewString()
		s.ready.Store(false)
	}
	ok := runnerwire.AuthOK{SessionID: s.id, Resumed: resumed, PingInterval: runnerwire.PingInterval, LastSeq: cfg.LastReceived}
	if err := runnerwire.WriteControl(ctx, conn, runnerwire.TypeAuthOK, ok); err != nil {
		return nil, err
	}
	if s.timer != nil {
		s.timer.Stop()
	}
	var peer *runnerwire.Peer
	configureAdmission(ctx, &cfg, resumed, pending, a, s, func() *runnerwire.Peer { return peer })
	h.configureActivity(ctx, &cfg, row, a, s, func() *runnerwire.Peer { return peer })
	peer = a.Link.AttachSession(conn, cfg)
	s.peer, s.link, s.ended = peer, a.Link, time.Time{}
	last := uint64(0)
	if resumed {
		last = auth.Resume.LastSeq
	}
	if err := peer.Start(h.ctx, last); err != nil {
		peer.Close()
		return peer, err
	}
	if resumed {
		err = a.Link.Ready(peer)
	} else {
		if h.cfg.Connected != nil {
			err = h.cfg.Connected(ctx, id)
			s.connected = err == nil
		}
		if err == nil {
			var payload []byte
			payload, err = runnerwire.Marshal(pending)
			if err == nil {
				err = peer.Send(ctx, runnerwire.Frame{Type: runnerwire.TypePending, Payload: payload})
			}
		}
	}
	if err != nil {
		peer.Close()
		return peer, err
	}
	return peer, nil
}

func (h *Hub) resume(ctx context.Context, s *hubSession, request *runnerwire.Resume) (runnerwire.PeerConfig, bool, error) {
	if request == nil || request.SessionID != s.id || s.peer == nil || !s.ready.Load() ||
		(!s.ended.IsZero() && time.Since(s.ended) > runnerwire.ResumeWindow) {
		return runnerwire.PeerConfig{}, false, nil
	}
	cfg, err := s.peer.ResumeConfig(ctx)
	if err == nil {
		_, err = cfg.Replay.Since(request.LastSeq)
	}
	return cfg, err == nil, err
}

func configureAdmission(ctx context.Context, cfg *runnerwire.PeerConfig, resumed bool, actions []runnerwire.PendingAction, a Admission, session *hubSession, peer func() *runnerwire.Peer) {
	state := resumed
	ready := resumed
	pending := make(map[uuid.UUID]bool, len(actions))
	for _, action := range actions {
		pending[action.ID] = true
	}
	cfg.OnControlChecked = func(f runnerwire.Frame) error {
		switch f.Type {
		case runnerwire.TypeState:
			if state || ready || len(pending) != 0 {
				return fmt.Errorf("%w: unexpected STATE", runnerwire.ErrBadFrame)
			}
			var runs []runnerwire.RunState
			if err := runnerwire.Unmarshal(f.Payload, &runs); err != nil {
				return err
			}
			if err := a.State(ctx, runs); err != nil {
				return err
			}
			state = true
		case runnerwire.TypeReady:
			if !state || ready || len(f.Payload) != 0 {
				return fmt.Errorf("%w: unexpected READY", runnerwire.ErrBadFrame)
			}
			if err := a.Link.Ready(peer()); err != nil {
				return err
			}
			ready = true
			session.ready.Store(true)
		default:
			return fmt.Errorf("%w: unexpected %s", runnerwire.ErrBadFrame, f.Type)
		}
		return nil
	}
	cfg.OnEventChecked = func(ev runnerwire.Event) error {
		if !state {
			var result runnerwire.ActionResult
			if ev.Kind != runnerwire.EventActionResult || runnerwire.Unmarshal(ev.Data, &result) != nil || !pending[result.ID] {
				return fmt.Errorf("%w: expected pending action result before STATE", runnerwire.ErrBadFrame)
			}
			if err := a.Event(ctx, ev); err != nil {
				return err
			}
			delete(pending, result.ID)
			return nil
		}
		return a.Event(ctx, ev)
	}
}

func (h *Hub) ended(id uuid.UUID, s *hubSession, peer *runnerwire.Peer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.peer != peer {
		return
	}
	h.mu.Lock()
	current := !h.closed && h.sessions[id] == s
	h.mu.Unlock()
	if !current {
		return
	}
	s.ended = time.Now()
	s.timer = time.AfterFunc(runnerwire.ResumeWindow, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.peer != peer || peer.Online() {
			return
		}
		h.mu.Lock()
		if h.sessions[id] == s {
			delete(h.sessions, id)
		}
		h.mu.Unlock()
		if s.connected && h.cfg.Disconnected != nil {
			_ = h.cfg.Disconnected(h.ctx, id)
			s.connected = false
		}
	})
}

// Close drops this replica's connections and timers at daemon shutdown.
func (h *Hub) Close() {
	h.mu.Lock()
	h.closed = true
	sessions := h.sessions
	h.sessions = map[uuid.UUID]*hubSession{}
	h.mu.Unlock()
	for _, s := range sessions {
		s.mu.Lock()
		if s.timer != nil {
			s.timer.Stop()
		}
		if s.link != nil {
			s.link.Close()
		}
		s.mu.Unlock()
	}
}
