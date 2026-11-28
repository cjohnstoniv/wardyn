// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runnerPeering is the replica-to-replica routing of registered runners' calls under high availability: the
// peer client, the holder view every online answer reads, and the hub's way to the holder record. One per Server,
// nil without RunnerPeer or a holder store.
type runnerPeering struct {
	cfg   remote.PeerConfig
	self  string
	st    store.RunnerHolderStore
	trans *remote.PeerTransport
	view  *remote.HolderView

	mu   sync.Mutex
	seen map[uuid.UUID]bool // the runners held here or in the view at routeHeldElsewhere's last call
}

func (s *Server) runnerPeering() *runnerPeering {
	s.peering.once.Do(func() {
		st, ok := s.cfg.Store.(store.RunnerHolderStore)
		if s.cfg.RunnerPeer == nil || !ok {
			return
		}
		p := &runnerPeering{cfg: *s.cfg.RunnerPeer, self: s.replicaName(), st: st}
		p.trans = remote.NewPeerTransport(p.cfg, p.self, func(ctx context.Context, runnerID uuid.UUID) (remote.PeerRoute, bool, error) {
			r, ok, err := st.RunnerSessionHolder(ctx, runnerID)
			return remote.PeerRoute{ReplicaID: r.ReplicaID, PeerURL: r.PeerURL, SessionID: r.SessionID}, ok, err
		})
		p.view = remote.NewHolderView(p.self, func(ctx context.Context) (map[uuid.UUID]remote.PeerRoute, error) {
			rows, err := st.RunnerSessionHolders(ctx)
			if err != nil {
				return nil, err
			}
			out := make(map[uuid.UUID]remote.PeerRoute, len(rows))
			for _, r := range rows {
				out[r.RunnerID] = remote.PeerRoute{ReplicaID: r.ReplicaID, PeerURL: r.PeerURL, SessionID: r.SessionID}
			}
			return out, nil
		})
		p.view.OnRefresh = s.routeHeldElsewhere
		s.peering.p = p
	})
	return s.peering.p
}

// Claim, Touch and Drop are the hub's remote.PeerLoop, under this replica's id and address.
func (p *runnerPeering) Claim(ctx context.Context, runnerID, sessionID uuid.UUID) (bool, error) {
	return p.st.ClaimRunnerSessionHolder(ctx, runnerID, sessionID, p.self, p.cfg.URL)
}

func (p *runnerPeering) Touch(ctx context.Context, sessions []uuid.UUID) ([]uuid.UUID, error) {
	return p.st.TouchRunnerSessionHolders(ctx, p.self, sessions)
}

func (p *runnerPeering) Drop(ctx context.Context, runnerID, sessionID uuid.UUID) error {
	return p.st.DropRunnerSessionHolder(ctx, runnerID, sessionID)
}

// Refresh reads the holder view again and, after a successful read, records this process's lease owner name
// against this replica's id and address, so a run's watcher lease names a peer address.
func (p *runnerPeering) Refresh(ctx context.Context) {
	if p.view.Refresh(ctx) == nil {
		_ = p.st.TouchRunnerPeerReplica(ctx, watcherOwner, p.self, p.cfg.URL)
	}
}

var _ remote.PeerLoop = (*runnerPeering)(nil)

// routeHeldElsewhere is the holder view's OnRefresh. It makes this replica's endpoint for each claimed runner whose
// session another replica holds, so a runner claimed after this replica booted is routed here within one refresh
// of its admission anywhere. It removes the endpoint of a runner that left both this replica's held sessions and
// the view since the last call and is no longer claimed. No lock is held across a store read or an endpoint change.
func (s *Server) routeHeldElsewhere(ctx context.Context, elsewhere []uuid.UUID) {
	p := s.runnerPeering()
	st, ok := s.cfg.Store.(store.RunnerStore)
	if p == nil || !ok {
		return
	}
	now := map[uuid.UUID]bool{}
	for _, id := range s.heldRunnerSessions() {
		now[id] = true
	}
	for _, id := range elsewhere {
		now[id] = true
		s.runnerStreams.subsMu.Lock()
		_, routed := s.runnerStreams.subs[id]
		s.runnerStreams.subsMu.Unlock()
		if routed {
			continue
		}
		if rn, err := st.GetRunner(ctx, id); err != nil || rn.State != types.RunnerClaimed {
			continue
		}
		s.runnerEndpoint(id)
		// A revoke that committed between the read and the endpoint has already unrouted the runner: undo it.
		if rn, err := st.GetRunner(ctx, id); errors.Is(err, store.ErrNotFound) || (err == nil && rn.State != types.RunnerClaimed) {
			s.unrouteRunner(id)
		}
	}
	p.mu.Lock()
	prev := p.seen
	p.seen = now
	p.mu.Unlock()
	var retry []uuid.UUID
	for id := range prev {
		if now[id] {
			continue
		}
		switch rn, err := st.GetRunner(ctx, id); {
		case errors.Is(err, store.ErrNotFound) || (err == nil && rn.State != types.RunnerClaimed):
			s.unrouteRunner(id)
		case err != nil:
			retry = append(retry, id) // read again at the next call
		}
	}
	if len(retry) > 0 {
		p.mu.Lock()
		for _, id := range retry {
			p.seen[id] = true
		}
		p.mu.Unlock()
	}
}

// peerLeaseArgs is the door method lease's argument: the run and the execution generation its lease carries.
type peerLeaseArgs struct {
	RunID      uuid.UUID `json:"run_id"`
	Generation uuid.UUID `json:"generation"`
}

// peerHolderAnswer answers a door method the holder serves from its own memory, for runnerID: caps from its own
// Substrate, sending no frame to the runner, and lease by its own leaseSendable and send, on its own clock. The
// lease send is this replica's only: it is never passed on to another replica.
func (s *Server) peerHolderAnswer(runnerID uuid.UUID, method string) func(ctx context.Context, raw json.RawMessage) (any, error) {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		if method == remote.PeerMethodCaps {
			c, ok := s.runnerEndpoint(runnerID).sub.Caps()
			return remote.PeerCaps{Caps: c, OK: ok}, nil
		}
		var a peerLeaseArgs
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
		st, err := s.runnerExecutionStore()
		if err != nil {
			return nil, err
		}
		run, err := st.GetRun(ctx, a.RunID)
		if err != nil {
			return nil, err
		}
		if run.RunnerID == nil || *run.RunnerID != runnerID {
			return nil, errRunnerNotReady // a pin names one runner: a lease for another's run is never sent on it
		}
		renewed, err := s.leaseSendable(ctx, st, run)
		if err != nil {
			return nil, err
		}
		return nil, s.sendRunnerLeaseHere(ctx, run, a.Generation, renewed)
	}
}

// peerState is the Server's peering value and the door's refusal counts.
type peerState struct {
	once sync.Once
	p    *runnerPeering

	mu      sync.Mutex
	refused map[string]int       // per class, since that class's last Warn
	warned  map[string]time.Time // per class, when it was last logged
}

// Refusal classes of the runner peer door.
const (
	peerRefusedMissingField   = "missing_field"
	peerRefusedBadMAC         = "bad_mac"
	peerRefusedWrongReplica   = "wrong_replica"
	peerRefusedExpired        = "expired"
	peerRefusedNotLiveSession = "not_live_session"
	peerRefusedNoSink         = "no_sink"
	peerRefusedWrongMethod    = "wrong_method"
)

// peerRefusalWarnEvery is the most often one class's refusals are logged.
const peerRefusalWarnEvery = time.Minute

// notePeerRefusal counts a refusal and logs at most one Warn per class a minute, with the count since the last line.
// Only the class is logged: no header value (not even the claimed sender, which a bad mac leaves unauthenticated),
// mac or frame byte.
func (s *Server) notePeerRefusal(ctx context.Context, class string) {
	now := s.cfg.Now()
	s.peering.mu.Lock()
	if s.peering.refused == nil {
		s.peering.refused, s.peering.warned = map[string]int{}, map[string]time.Time{}
	}
	s.peering.refused[class]++
	last, seen := s.peering.warned[class]
	if seen && now.Sub(last) < peerRefusalWarnEvery {
		s.peering.mu.Unlock()
		return
	}
	n := s.peering.refused[class]
	s.peering.refused[class], s.peering.warned[class] = 0, now
	s.peering.mu.Unlock()
	slog.WarnContext(ctx, "wardynd: runner peer request refused", slog.String("class", class), slog.Int("refused", n))
}

// mountRunnerPeerRoute mounts the runner peer door, GET /api/v1/internal/runner-peer, only under high availability
// with the internal listener, the only listener that then serves the internal prefix: the door is never on the
// console listener. It takes no auth middleware, since its credential is its own Wardyn-Peer-* headers, and it is
// listed in internalRoutesWithoutAuth.
func (s *Server) mountRunnerPeerRoute(r chi.Router) {
	if !s.cfg.HA || s.cfg.ControlPlaneCAPEM == "" {
		return
	}
	r.Get("/internal/runner-peer", s.handleRunnerPeer)
}

// handleRunnerPeer is the holder's end of a pin another replica made on this replica's session for a runner. It
// refuses, before any frame reaches a runner: a missing field (403, no class); a bad mac (403, before which no class
// is told); then, with the class in Wardyn-Peer-Refused, a token for another replica, an exp in the past or more
// than 30 seconds ahead, a target that does not fit the method, and a runner or session this replica does not hold
// now. A first call of another method closes the WebSocket.
func (s *Server) handleRunnerPeer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tok, mac, err := remote.ReadPeerToken(r.Header)
	if err != nil {
		s.notePeerRefusal(ctx, peerRefusedMissingField)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	p := s.runnerPeering()
	if p == nil || len(p.cfg.Key) == 0 || !tok.Verify(p.cfg.Key, mac) {
		s.notePeerRefusal(ctx, peerRefusedBadMAC)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	refuse := func(class string) {
		s.notePeerRefusal(ctx, class)
		w.Header().Set(remote.HeaderPeerRefused, class)
		w.WriteHeader(http.StatusForbidden)
	}
	now := s.cfg.Now()
	exp := time.Unix(tok.Exp, 0)
	switch {
	case tok.To != p.self:
		refuse(peerRefusedWrongReplica)
		return
	case !exp.After(now) || exp.Sub(now) > remote.PeerTokenTTL:
		refuse(peerRefusedExpired)
		return
	case tok.Method == remote.PeerMethodOutput && tok.Target == uuid.Nil,
		tok.Method != remote.PeerMethodOutput && tok.Target != uuid.Nil,
		tok.Method == "" || tok.Method == remote.PeerMethodStream:
		// output names its run; every other method names none, and a pin always opens with a call.
		refuse(peerRefusedWrongMethod)
		return
	case tok.Method == remote.PeerMethodOutput:
		refuse(peerRefusedNoSink) // this replica serves no forwarded output
		return
	}
	st, ok := s.cfg.Store.(store.RunnerStore)
	if !ok {
		refuse(peerRefusedNotLiveSession)
		return
	}
	hub := s.runnerHub(st)
	if !hub.HoldsSession(tok.Runner, tok.Session) {
		refuse(peerRefusedNotLiveSession)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer c.CloseNow() //nolint:errcheck // the peer may already have closed it
	if tok.Method == remote.PeerMethodCaps || tok.Method == remote.PeerMethodLease {
		_ = hub.ServeHolderMethod(ctx, runnerwire.NewWebSocket(c), tok.Runner, tok.Session, tok.Method, s.peerHolderAnswer(tok.Runner, tok.Method), func(class string) {
			s.notePeerRefusal(ctx, class)
		})
		return
	}
	_ = hub.ServePeer(ctx, runnerwire.NewWebSocket(c), tok.Runner, tok.Session, tok.Method, func(class string) {
		s.notePeerRefusal(ctx, class)
	})
}
