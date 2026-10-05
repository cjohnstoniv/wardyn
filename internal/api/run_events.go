// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Per-run lifecycle events (#1144): GET /api/v1/runs/{id}/events, a
// text/event-stream of the closed client.RunEvent vocabulary, fed by the state
// moves dispatch and the terminal paths already make (casRunState, the
// dispatcher's OnWaiting, the idle reaper's call into this package). The feed
// is an in-memory ring per run — resumable with Last-Event-ID within the
// daemon's lifetime, no table behind it. Each replica announces the events it
// emits to the others over NOTIFY (live_bus.go), so a reader on any replica
// follows a run another dispatches; a notice is a hint, and the keepalive
// re-read of the store ends a stream whose notice was lost. Event ids count
// within one replica's ring, so a Last-Event-ID is not portable across replicas.
package api

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

const (
	// runEventsBeat spaces the keepalive comment, which also keeps a proxy's
	// inter-read timeout (nginx proxy_read_timeout) from cutting an idle stream,
	// and the store re-read that ends a stream whose run ended on a path that
	// does not emit (a lease end, or a run that ended before a restart).
	runEventsBeat = 15 * time.Second
	// runEventsHold bounds one connection. Authentication runs once per
	// request, so a stream held forever would keep serving a session revoked
	// after it opened; closing it makes the client reconnect (Last-Event-ID)
	// through the middleware again.
	runEventsHold = 5 * time.Minute
	// runEventsRetention keeps an ended run's ring for a reconnecting reader.
	// After it, a reader gets the synthesized ended event alone.
	runEventsRetention = 10 * time.Minute
	// maxRunEventStreams bounds one principal's concurrent streams (#1407).
	// Each is a goroutine, a held connection and a store read per beat, and a
	// portal serves every viewer of a person through that person's delegated
	// token, so without it one credential — or a reconnect loop gone wrong —
	// holds connections without bound. 32 leaves room for a person following
	// a wide fan-out of their own runs from a portal and the CLI at once. The
	// admin token is one principal: its callers share one 32.
	maxRunEventStreams = 32
)

// runEventHub holds every run's ring. Zero value is ready to use.
// ponytail: a run whose end reaches no emitter and no reader keeps its ring
// (a few small events) for the daemon's lifetime.
type runEventHub struct {
	mu    sync.Mutex
	rings map[uuid.UUID]*runEventRing
	// beat and hold override runEventsBeat and runEventsHold for one server,
	// so a test need not wait real seconds; zero means the constant.
	beat, hold time.Duration
	// streams counts each principal's open streams (maxRunEventStreams).
	streams map[runEventStreamer]int
	// publish, when set, sends the events this replica itself emits to the others
	// (live_bus.go). Events taken from another replica are appended with append, not emit.
	publish func(runID uuid.UUID, evs []client.RunEvent)
}

// emit appends evs to runID's ring and tells the other replicas.
func (h *runEventHub) emit(runID uuid.UUID, evs ...client.RunEvent) {
	h.append(runID, evs...)
	if h.publish != nil {
		h.publish(runID, evs)
	}
}

// runEventStreamer is who a stream counts against: the actor type with the
// name, so no principal string shares another kind of caller's slots.
type runEventStreamer struct {
	actor types.ActorType
	name  string
}

// openStream takes one of who's stream slots, returning its release and
// whether one was free.
func (h *runEventHub) openStream(who runEventStreamer) (func(), bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.streams[who] >= maxRunEventStreams {
		return nil, false
	}
	if h.streams == nil {
		h.streams = map[runEventStreamer]int{}
	}
	h.streams[who]++
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.streams[who]--; h.streams[who] == 0 {
			delete(h.streams, who)
		}
	}, true
}

type runEventRing struct {
	events []client.RunEvent
	wake   chan struct{} // closed and replaced on every append
}

func (r *runEventRing) ended() bool {
	return len(r.events) > 0 && r.events[len(r.events)-1].Type == client.RunEventEnded
}

// ringLocked returns runID's ring, creating it. Caller holds h.mu.
func (h *runEventHub) ringLocked(runID uuid.UUID) *runEventRing {
	if h.rings == nil {
		h.rings = map[uuid.UUID]*runEventRing{}
	}
	r := h.rings[runID]
	if r == nil {
		r = &runEventRing{wake: make(chan struct{})}
		h.rings[runID] = r
	}
	return r
}

// append adds evs to runID's ring unless the ring has ended: once ended, a
// ring is closed, which is also what makes a KILLED->KILLED re-kill silent.
func (h *runEventHub) append(runID uuid.UUID, evs ...client.RunEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.appendLocked(runID, h.ringLocked(runID), evs...)
}

func (h *runEventHub) appendLocked(runID uuid.UUID, r *runEventRing, evs ...client.RunEvent) {
	if r.ended() {
		return
	}
	now := time.Now().UTC()
	for _, ev := range evs {
		ev.ID = uint64(len(r.events)) + 1
		ev.At = now
		r.events = append(r.events, ev)
	}
	close(r.wake)
	r.wake = make(chan struct{})
	if r.ended() {
		time.AfterFunc(runEventsRetention, func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.rings[runID] == r {
				delete(h.rings, runID)
			}
		})
	}
}

// moved maps one won state CAS onto the vocabulary. casRunState is the one
// place every CAS is issued, so this is the one hook for all of them.
func (h *runEventHub) moved(runID uuid.UUID, from, to types.RunState) {
	switch {
	case from.IsTerminal():
		return
	case to == types.RunStarting:
		h.emit(runID, client.RunEvent{Type: client.RunEventProvisioning})
	case to == types.RunRunning && from == types.RunStarting:
		h.emit(runID, client.RunEvent{Type: client.RunEventReady})
	case to == types.RunFailed:
		h.emit(runID, client.RunEvent{Type: client.RunEventFailed, Reason: failedPhase(from)},
			client.RunEvent{Type: client.RunEventEnded, State: to})
	case to.IsTerminal():
		h.emit(runID, client.RunEvent{Type: client.RunEventEnded, State: to})
	}
}

// failedPhase is a FAILED event's machine reason: the state it failed from.
// The free-text failure hint is deliberately not it — it can carry a
// substrate's error text, and this feed carries no log content.
func failedPhase(from types.RunState) string {
	switch from {
	case types.RunPending:
		return client.RunFailedNotStarted
	case types.RunStarting:
		return client.RunFailedStartFailed
	default:
		return client.RunFailedRunFailed
	}
}

// idleStopped records the idle reaper's RUNNING->STOPPED win.
func (h *runEventHub) idleStopped(runID uuid.UUID) {
	h.emit(runID, client.RunEvent{Type: client.RunEventIdleStopped},
		client.RunEvent{Type: client.RunEventEnded, State: types.RunStopped})
}

// onWaiting wraps dispatch's OnWaiting so the first image pull the substrate
// reports becomes one pulling event; every other waiting reason is detail the
// status line already carries, not a lifecycle step.
func (h *runEventHub) onWaiting(runID uuid.UUID, next func(string)) func(string) {
	return func(detail string) {
		if statusDetailReason(detail) == "Pulling" {
			h.mu.Lock()
			r := h.ringLocked(runID)
			pulling := len(r.events) > 0 && r.events[len(r.events)-1].Type == client.RunEventProvisioning
			if pulling {
				h.appendLocked(runID, r, client.RunEvent{Type: client.RunEventPulling})
			}
			h.mu.Unlock()
			if pulling && h.publish != nil {
				h.publish(runID, []client.RunEvent{{Type: client.RunEventPulling}})
			}
		}
		if next != nil {
			next(detail)
		}
	}
}

// settle ends a ring whose run the store already reports terminal. The store
// can say FAILED before the CAS's own emit lands (a reader opening in that
// window, or a keepalive re-read), and moved's failed+ended would then meet a
// closed ring — so settle writes the failed event itself, with the phase the
// ring's last event implies. An empty ring (a previous daemon's run) has no
// phase to infer, and gets ended alone.
func (h *runEventHub) settle(runID uuid.UUID, state types.RunState) {
	if !state.IsTerminal() {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.ringLocked(runID)
	evs := []client.RunEvent{{Type: client.RunEventEnded, State: state}}
	if state == types.RunFailed && len(r.events) > 0 {
		from := types.RunStarting
		if r.events[len(r.events)-1].Type == client.RunEventReady {
			from = types.RunRunning
		}
		evs = append([]client.RunEvent{{Type: client.RunEventFailed, Reason: failedPhase(from)}}, evs...)
	}
	h.appendLocked(runID, r, evs...)
}

// since returns runID's events after id `after`, the channel the next append
// closes, and whether the ring has ended. An `after` beyond the ring came from
// a previous daemon (or a pruned ring): ids only grow within one, so the whole
// ring is new to that reader.
func (h *runEventHub) since(runID uuid.UUID, after uint64) ([]client.RunEvent, <-chan struct{}, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.ringLocked(runID)
	if after > uint64(len(r.events)) {
		after = 0
	}
	evs := r.events[after:]
	if len(evs) == 0 && r.ended() {
		// A synthesized ring (a previous daemon's, or one rebuilt after pruning)
		// is shorter than the one the reader followed, so `after` can land on its
		// last id. Re-sending the terminal event is idempotent; sending nothing
		// leaves the reader reconnecting forever.
		evs = r.events[len(r.events)-1:]
	}
	return append([]client.RunEvent(nil), evs...), r.wake, r.ended()
}

// handleRunEvents serves GET /api/v1/runs/{id}/events. Owner-or-admin
// (getRunAuthorized): a caller who cannot read the run gets the same 404
// GET /runs/{id} answers. A portal's delegated token reads its person's runs
// (delegationAllowed). One principal holds at most maxRunEventStreams at a
// time; the next is refused 422 event_stream_cap. The stream ends after the
// ended event, after runEventsHold, or when the client or the daemon goes away.
func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunAuthorized(w, r, id)
	if !ok {
		return
	}
	// After the read gate, so a non-reader gets the 404 and never a slot.
	actor, principal := actorFromRequest(r)
	release, ok := s.runEvents.openStream(runEventStreamer{actor, principal})
	if !ok {
		s.refuse(w, r, authz.Deny(authz.ReasonEventStreamCap, id.String(),
			fmt.Sprintf("too many open event streams (max %d) — close one and retry", maxRunEventStreams)).OnRun(id))
		return
	}
	defer release()
	// A malformed Last-Event-ID replays from the start rather than refusing:
	// it is a resume hint, and a full replay is never wrong.
	after, _ := strconv.ParseUint(r.Header.Get("Last-Event-ID"), 10, 64)
	openedAt := s.cfg.Now().UTC()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	daemon := r.Context()
	if s.cfg.BaseCtx != nil {
		daemon = s.cfg.BaseCtx
	}
	hold := time.NewTimer(cmp.Or(s.runEvents.hold, runEventsHold))
	defer hold.Stop()
	beat := time.NewTicker(cmp.Or(s.runEvents.beat, runEventsBeat))
	defer beat.Stop()

	s.runEvents.settle(id, run.State)
	for {
		evs, wake, ended := s.runEvents.since(id, after)
		// No write deadline (the server sets none, boot_serve.go), so a peer that
		// stops reading must never fill the socket buffer: one hold is at most
		// hold/beat keepalives plus the closed vocabulary's few events — a few
		// KiB — after which the hold timer ends the handler anyway.
		for _, ev := range evs {
			data, _ := json.Marshal(ev)
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.ID, ev.Type, data); err != nil {
				return
			}
			after = ev.ID
		}
		if err := rc.Flush(); err != nil || ended {
			return
		}
		select {
		case <-wake:
		case <-beat.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil || s.sessionRevokedSince(r, principal, openedAt) || s.delegationEndedSince(r) {
				return
			}
			if cur, err := s.cfg.Store.GetRun(r.Context(), id); err == nil {
				s.runEvents.settle(id, cur.State)
			}
		case <-hold.C:
			return
		case <-r.Context().Done():
			return
		case <-daemon.Done():
			return
		}
	}
}

// sessionRevokedSince reports whether the caller's session was revoked after
// openedAt. Authentication ran once, when the stream opened; this cuts a
// revoked session off at the next keepalive instead of at the hold. An
// unanswerable check ends the stream too — the reconnect re-authenticates.
// The admin token is not a session, so nothing revokes it here.
func (s *Server) sessionRevokedSince(r *http.Request, principal string, openedAt time.Time) bool {
	if s.cfg.SessionRevocations == nil {
		return false
	}
	// The admin token authenticated as the system with no device behind it; a
	// device keeps the check (a revoke-all still ends its stream).
	if actorType, _ := actorFromRequest(r); actorType == types.ActorSystem {
		if _, isDevice := deviceFromContext(r.Context()); !isDevice {
			return false
		}
	}
	revoked, err := s.cfg.SessionRevocations.IsSessionRevoked(r.Context(), principal, oidcEmailFromContext(r.Context()), openedAt)
	return err != nil || revoked
}

// delegationEndedSince reports whether a delegated stream's token is no longer
// live: its ten-minute TTL passed or its portal was revoked since the stream
// opened. It asks the store the question delegatedTokenAuth asked at open
// (GetDelegatedTokenByRaw: unexpired and portal not revoked), and anything but
// a live answer ends the stream — a lookup failure and a store without the
// portal capability included — so the reconnect re-authenticates. A request
// that is not delegated has nothing to check.
func (s *Server) delegationEndedSince(r *http.Request) bool {
	if _, delegated := audit.DelegationFrom(r.Context()); !delegated {
		return false
	}
	ds, ok := s.cfg.Store.(store.DelegateStore)
	if !ok {
		return true
	}
	tok, ok := bearerToken(r)
	if !ok {
		return true
	}
	_, err := ds.GetDelegatedTokenByRaw(r.Context(), tok, s.cfg.Now().UTC())
	return err != nil
}
