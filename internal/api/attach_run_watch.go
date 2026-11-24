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
// would have; a refusal in the read ends every socket on the run at that tick.

// attachRunState is one read of a run's half of the attach check.
type attachRunState struct {
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
		ctx, stop := context.WithCancel(s.cfg.BaseCtx)
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
		lctx, cancel := context.WithTimeout(ctx, s.attachAuthorityLookupBound())
		st := s.readAttachRunState(lctx, runID)
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
