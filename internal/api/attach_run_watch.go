// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Every open attach socket repeats the run's half of its check (attachUnitCheck): the run row and the profile's
// deny_interactive. That half answers the same for every socket on the run, so this replica reads it once per
// attachAuthorityEvery for the run (watchAttachRun) and hands the read to each socket, which runs its own
// credential's half beside it. A socket decides at the shared read's tick, which comes no later than its own timer
// would have; a refusal in the read ends every socket on the run at that tick. The read's lookup bound is the check's
// bound too: a socket's check on a shared read ends when the read's bound would have, so one bound covers the run's
// half and the credential's half together, as one bound covered both before the read was shared.

// attachRunState is one read of a run's half of the attach check.
type attachRunState struct {
	at      time.Time // when the read began (monotonic); its lookup bound ends attachAuthorityLookupBound later
	run     types.AgentRun
	gone    bool  // the run row is gone (store.ErrNotFound)
	runErr  error // the run could not be read
	denied  bool  // its profile denies interactive sessions, or nothing satisfies its composition
	unitErr error // its profile could not be read (unitReadFailed)
}

// readAttachRunState reads runID's half of the attach check.
func (s *Server) readAttachRunState(ctx context.Context, runID uuid.UUID) attachRunState {
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if errors.Is(err, store.ErrNotFound) {
		return attachRunState{gone: true}
	}
	if err != nil {
		return attachRunState{runErr: err}
	}
	st := attachRunState{run: run}
	p, err := s.interactiveDeniedBy(ctx, run)
	switch {
	case err != nil && unitReadFailed(err):
		st.unitErr = err
	case err != nil || p != nil:
		st.denied = true
	}
	return st
}

// verdict is attachUnitCheck's answer from st, in its order: the run, the entry rule, the exemption, the profile.
func (st attachRunState) verdict(entry func(types.AgentRun) error, exempt func(types.AgentRun) bool) error {
	switch {
	case st.gone:
		return errAttachAuthorityEnded
	case st.runErr != nil:
		return unconfirmed(st.runErr)
	}
	if entry != nil {
		if err := entry(st.run); err != nil {
			return err
		}
	}
	switch {
	case exempt(st.run):
		return nil
	case st.unitErr != nil:
		return unconfirmed(st.unitErr)
	case st.denied:
		return errAttachAuthorityEnded
	}
	return nil
}

type attachRunStateKey struct{}

// sharedCheck decides what a socket does with the shared read st, taken at now. fresh is when the socket's latest read
// of its own began (zero if none). It returns the context the socket's check runs in and whether it runs at all:
//   - st began before that fresh read and refuses nothing: the check is skipped, for a stale OK must not close a window
//     the fresh read left open (a stale refusal still ends the socket: that is the safe side);
//   - st's lookup bound has passed (the socket was busy with a retry): the read is dropped and the check reads afresh;
//   - otherwise the check runs on st.
func (s *Server) sharedCheck(ctx context.Context, runID uuid.UUID, st attachRunState, fresh, now time.Time) (context.Context, bool) {
	switch {
	case st.at.Before(fresh) && !st.gone && !st.denied:
		return ctx, false
	case !now.Before(st.at.Add(s.attachAuthorityLookupBound())):
		return ctx, true
	}
	return withAttachRunState(ctx, runID, st), true
}

// attachRunStateFor is a shared read and the run it was read for.
type attachRunStateFor struct {
	runID uuid.UUID
	st    attachRunState
}

// withAttachRunState carries st, read for runID, to the attachUnitCheck a socket's check makes under the result.
func withAttachRunState(ctx context.Context, runID uuid.UUID, st attachRunState) context.Context {
	return context.WithValue(ctx, attachRunStateKey{}, attachRunStateFor{runID: runID, st: st})
}

// attachRunStateFrom is the shared read ctx carries for runID, if any.
func attachRunStateFrom(ctx context.Context, runID uuid.UUID) (attachRunState, bool) {
	v, ok := ctx.Value(attachRunStateKey{}).(attachRunStateFor)
	if !ok || v.runID != runID {
		return attachRunState{}, false
	}
	return v.st, true
}

// attachRunWatchState is this replica's shared run reads, one per run with an open socket. Zero value is ready.
type attachRunWatchState struct {
	mu      sync.Mutex
	watches map[uuid.UUID]*attachRunWatch
}

// attachRunWatch is one run's shared read: the sockets it feeds and how to stop it.
type attachRunWatch struct {
	subs map[chan attachRunState]struct{}
	stop context.CancelFunc
}

// watchAttachRun subscribes to runID's shared read, starting it for the run's first socket. Each read is sent on
// the returned channel, the newest replacing one not yet taken. leave unsubscribes, and the read stops with the
// run's last socket; calling leave again does nothing.
func (s *Server) watchAttachRun(runID uuid.UUID) (<-chan attachRunState, func()) {
	ws := &s.attachRuns
	ch := make(chan attachRunState, 1)
	ws.mu.Lock()
	defer ws.mu.Unlock()
	w := ws.watches[runID]
	if w == nil {
		// Not ended by the server's shutdown: its sockets stay open through the drain and stay checked, and the
		// read ends with the run's last socket.
		ctx, stop := context.WithCancel(context.WithoutCancel(s.cfg.BaseCtx))
		w = &attachRunWatch{subs: map[chan attachRunState]struct{}{}, stop: stop}
		if ws.watches == nil {
			ws.watches = map[uuid.UUID]*attachRunWatch{}
		}
		ws.watches[runID] = w
		go s.runAttachWatch(ctx, runID, w)
	}
	w.subs[ch] = struct{}{}
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			ws.mu.Lock()
			defer ws.mu.Unlock()
			delete(w.subs, ch)
			if len(w.subs) == 0 {
				w.stop()
				delete(ws.watches, runID)
			}
		})
	}
}

// runAttachWatch reads runID's half of the attach check every attachAuthorityEvery, each read under the lookup
// bound, and sends it to every socket subscribed to w until ctx ends.
func (s *Server) runAttachWatch(ctx context.Context, runID uuid.UUID, w *attachRunWatch) {
	t := time.NewTicker(s.attachAuthorityEvery())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		at := time.Now()
		lctx, cancel := context.WithDeadline(ctx, at.Add(s.attachAuthorityLookupBound()))
		st := s.readAttachRunState(lctx, runID)
		st.at = at
		cancel()
		s.attachRuns.mu.Lock()
		for ch := range w.subs {
			select { // only this loop sends, so after the drain the send never blocks
			case <-ch:
			default:
			}
			ch <- st
		}
		s.attachRuns.mu.Unlock()
	}
}
