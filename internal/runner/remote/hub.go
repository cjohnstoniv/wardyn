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
// Pending, BeginInventory, StatePage and Event are required: missing durable
// reconciliation must not silently admit a runner. Substrate and Link must be
// the same pair.
type Admission struct {
	Substrate *Substrate
	Link      *Link
	Pending   func(context.Context) ([]runnerwire.PendingAction, error)
	// BeginInventory captures the org's candidate set for a fresh session
	// before AUTH_OK and PENDING, fencing every earlier session's inventory.
	// It runs under the runner's session slot; a resumed session skips it.
	BeginInventory func(ctx context.Context, sessionID string) error
	// StatePage durably accepts one bounded, already-ordered page of that
	// session's inventory. It runs on the peer read loop, so it must neither
	// call this runner nor wait on a lock held across a call to it. An error
	// refuses READY and forces a fresh session; the Hub keeps only a cursor.
	StatePage func(ctx context.Context, sessionID string, page runnerwire.StatePage) error
	Event     func(context.Context, runnerwire.Event) error
	// Activity checks current run ownership/lifecycle in its database transaction.
	// Nil refuses live activity; historical EVENT ingestion never calls it.
	Activity func(context.Context, uuid.UUID) error
	// Ready, when set, is told once the session is READY, the first moment the runner accepts a LEASE. It
	// runs on the peer read loop for a fresh session, so it must not block or call this runner.
	Ready func()
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
	// Verified runs once the runner proves possession of its stored key,
	// before any further lookup or admission. An error refuses the connection.
	Verified func(context.Context, uuid.UUID) error
	// Inactive, when set, says a claimed runner may hold no session now (its
	// owner is deactivated), without revoking it. Such a connection is sent
	// GOAWAY (runner_owner_inactive) before admission, and the stored-row
	// recheck ends a live session with GOAWAY (fresh_session_required), so a
	// connection after reactivation is admitted again.
	Inactive func(context.Context, types.Runner) (bool, error)
	// ProxyImage, when set, names the proxy image the organisation runs for a claimed runner (chosen
	// for its reported architecture); AUTH_OK carries it, so the runner follows the organisation's
	// upgrades. "" says nothing.
	ProxyImage func(types.Runner) string
}

type hubSession struct {
	mu    sync.Mutex
	id    string
	peer  *runnerwire.Peer
	link  *Link
	ended time.Time
	// readyID names the session whose own READY was admitted. A late READY
	// from a superseded session stores its own id, never the current one.
	readyID   atomic.Pointer[string]
	timer     *time.Timer
	connected bool
}

// readyFor reports whether id is the current session and reached READY
// itself. Callers hold s.mu, which guards s.id.
func (s *hubSession) readyFor(id string) bool {
	p := s.readyID.Load()
	return id != "" && p != nil && *p == id && id == s.id
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
		return fmt.Errorf("%w: no registered runner by that id, or it could not be read: %s", runnerwire.ErrAuth, readCause(err))
	}
	auth, err := runnerwire.AuthenticateRunner(ctx, conn, row, h.cfg.OrgURL)
	if err != nil {
		return err
	}
	if h.cfg.Verified != nil {
		if err := h.cfg.Verified(ctx, id); err != nil {
			return err
		}
	}
	current, err := h.cfg.Lookup(ctx, id)
	if err != nil || current.ID != id || string(current.PublicKey) != string(row.PublicKey) || current.OrgURLSHA256 != row.OrgURLSHA256 {
		return fmt.Errorf("%w: the registration changed or could not be read while the runner authenticated: %s", runnerwire.ErrAuth, readCause(err))
	}
	if current.ClaimExpired(time.Now()) {
		return fmt.Errorf("%w: the runner's claim has lapsed", runnerwire.ErrAuth)
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
	if h.cfg.Inactive != nil {
		inactive, err := h.cfg.Inactive(ctx, current)
		if err != nil {
			return err
		}
		if inactive {
			_ = runnerwire.WriteControl(ctx, conn, runnerwire.TypeGoaway, runnerwire.Reason{Reason: reasonOwnerInactive})
			return errOwnerInactive
		}
	}
	if h.cfg.Admit == nil {
		return errors.New("runner hub admission is not configured")
	}
	a, err := h.cfg.Admit(ctx, current)
	if err != nil {
		return err
	}
	if a.Substrate == nil || a.Link == nil || a.Substrate.RunnerID() != auth.RunnerID || a.Substrate.t != a.Link ||
		a.Pending == nil || a.BeginInventory == nil || a.StatePage == nil || a.Event == nil {
		return errors.New("runner hub admission is incomplete or bound to another runner")
	}
	a.Substrate.mu.Lock()
	a.Substrate.requireExecutionGeneration = true
	a.Substrate.mu.Unlock()
	return h.serveClaimed(ctx, conn, current, auth, a)
}

// readCause says why a registration read refused: the read's own error, or that the row read no longer
// matches.
func readCause(err error) string {
	if err == nil {
		return "the stored row no longer matches"
	}
	return err.Error()
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
	recheck := time.NewTicker(storedRowRecheck)
	defer recheck.Stop()
	for live := true; live; {
		select {
		case <-peer.Done():
			live = false
		case <-ctx.Done():
			peer.Close()
			live = false
		case <-h.ctx.Done():
			peer.Close()
			live = false
		case <-recheck.C:
			h.recheck(ctx, row.ID)
		}
	}
	h.ended(row.ID, s, peer)
	return peer.Err()
}

// errFreshSessionRequired ends a resumed session that an action was queued for after its PENDING read:
// only a fresh session carries PENDING.
var errFreshSessionRequired = errors.New("runner: an action was queued for this runner; a fresh session carries it")

// storedRowRecheck bounds how long a session outlives a revocation, or its
// owner's deactivation, committed through another replica.
var storedRowRecheck = runnerwire.PingInterval

// reasonOwnerInactive is the GOAWAY reason of a runner whose owner is deactivated.
const reasonOwnerInactive = "runner_owner_inactive"

var errOwnerInactive = errors.New("runner hub: the runner's owner is deactivated")

// recheck ends the session with REVOKED once the stored row says so, and with
// GOAWAY once its owner is deactivated. A failed read is retried at the next
// tick: the store already refuses a revoked runner's writes, so this only
// bounds the session's lifetime.
func (h *Hub) recheck(ctx context.Context, id uuid.UUID) {
	current, err := h.cfg.Lookup(ctx, id)
	if err != nil || current.ID != id {
		return
	}
	if current.State == types.RunnerRevoked {
		_ = h.Revoke(ctx, id)
		return
	}
	if h.cfg.Inactive != nil {
		if inactive, err := h.cfg.Inactive(ctx, current); err == nil && inactive {
			_ = h.Goaway(ctx, id)
		}
	}
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
		return nil, fmt.Errorf("%w: the runner was released, revoked or re-registered while it was being admitted: %s", runnerwire.ErrAuth, readCause(err))
	}
	if s.peer != nil && s.peer.Online() {
		return nil, errors.New("runner already has an active connection")
	}
	cfg, resumed, err := h.resume(ctx, s, auth.Resume)
	if err != nil {
		_ = runnerwire.WriteControl(ctx, conn, runnerwire.TypeGoaway, runnerwire.Reason{Reason: "resume_gap"})
		return nil, err
	}
	if resumed {
		// Only a fresh session carries PENDING. An action queued while the
		// link was down turns this resume into a fresh session that delivers it.
		open, err := a.Pending(ctx)
		if err != nil {
			return nil, err
		}
		if len(open) != 0 {
			cfg, resumed = runnerwire.PeerConfig{}, false
		}
	}
	var pending []runnerwire.PendingAction
	if !resumed {
		if pending, err = h.fresh(ctx, s, a, id); err != nil {
			return nil, err
		}
	}
	ok := runnerwire.AuthOK{SessionID: s.id, Resumed: resumed, PingInterval: runnerwire.PingInterval, LastSeq: cfg.LastReceived}
	if h.cfg.ProxyImage != nil {
		ok.ProxyImage = h.cfg.ProxyImage(current)
	}
	if err := runnerwire.WriteControl(ctx, conn, runnerwire.TypeAuthOK, ok); err != nil {
		return nil, err
	}
	if s.timer != nil {
		s.timer.Stop()
	}
	var peer *runnerwire.Peer
	configureAdmission(ctx, &cfg, resumed, pending, a, s, s.id, func() *runnerwire.Peer { return peer })
	h.configureActivity(ctx, &cfg, row, a, s, func() *runnerwire.Peer { return peer })
	peer = a.Link.AttachSession(conn, cfg)
	if resumed {
		// A resumed session sends no STATE page: it is past the org's page check from its start.
		a.Link.stateFinal(peer)
	}
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
		// An action queued after the PENDING read above is not carried by a resumed session: end it, so
		// the runner's next one is fresh and delivers it.
		if err == nil {
			if open, perr := a.Pending(ctx); perr != nil || len(open) != 0 {
				err = errors.Join(errFreshSessionRequired, perr)
			}
		}
		if err == nil && a.Ready != nil {
			a.Ready()
		}
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
	if request == nil || !s.readyFor(request.SessionID) || s.peer == nil ||
		(!s.ended.IsZero() && time.Since(s.ended) > runnerwire.ResumeWindow) {
		return runnerwire.PeerConfig{}, false, nil
	}
	cfg, err := s.peer.ResumeConfig(ctx)
	if err == nil {
		_, err = cfg.Replay.Since(request.LastSeq)
	}
	return cfg, err == nil, err
}

// fresh replaces the session identity before any durable call, so a refused
// inventory or PENDING read leaves nothing resumable.
func (h *Hub) fresh(ctx context.Context, s *hubSession, a Admission, id uuid.UUID) ([]runnerwire.PendingAction, error) {
	s.id = uuid.NewString()
	s.readyID.Store(nil)
	if err := a.BeginInventory(ctx, s.id); err != nil {
		return nil, err
	}
	pending, err := a.Pending(ctx)
	if err != nil {
		return nil, err
	}
	if s.connected && h.cfg.Disconnected != nil {
		if err := h.cfg.Disconnected(ctx, id); err != nil {
			return nil, err
		}
		s.connected = false
	}
	return pending, nil
}

// configureAdmission binds sessionID by value: a superseded peer's late page
// must reach the store under its own, already fenced, inventory.
func configureAdmission(ctx context.Context, cfg *runnerwire.PeerConfig, resumed bool, actions []runnerwire.PendingAction, a Admission, session *hubSession, sessionID string, peer func() *runnerwire.Peer) {
	state := resumed
	ready := resumed
	var inventory runnerwire.StateInventory
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
			var page runnerwire.StatePage
			if err := runnerwire.Unmarshal(f.Payload, &page); err != nil {
				return err
			}
			next, err := inventory.Next(page)
			if err != nil {
				return err
			}
			// Marked before the final page is judged, not after: a teardown queued while it is judged, past
			// the page's own look at the queue, then ends this session itself (Link.requireFresh).
			if next.Complete && a.Link != nil {
				a.Link.stateFinal(peer())
			}
			if err := a.StatePage(ctx, sessionID, page); err != nil {
				return err
			}
			inventory, state = next, next.Complete
		case runnerwire.TypeReady:
			if !state || ready || len(f.Payload) != 0 {
				return fmt.Errorf("%w: unexpected READY", runnerwire.ErrBadFrame)
			}
			if err := a.Link.Ready(peer()); err != nil {
				return err
			}
			if a.Ready != nil {
				a.Ready()
			}
			ready = true
			session.readyID.Store(&sessionID)
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

// Close drops this replica's connections and timers at daemon shutdown. A session that reported
// Connected reports Disconnected here, each under a short bound, so the runner's disconnect is
// recorded by this replica before it exits rather than never.
func (h *Hub) Close() {
	h.mu.Lock()
	h.closed = true
	sessions := h.sessions
	h.sessions = map[uuid.UUID]*hubSession{}
	h.mu.Unlock()
	for id, s := range sessions {
		s.mu.Lock()
		if s.timer != nil {
			s.timer.Stop()
		}
		if s.link != nil {
			s.link.Close()
		}
		if s.connected && h.cfg.Disconnected != nil {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(h.ctx), time.Second)
			_ = h.cfg.Disconnected(ctx, id)
			cancel()
			s.connected = false
		}
		s.mu.Unlock()
	}
}
