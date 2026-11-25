// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// presenceInLineAfter is how long a writer may go without input before its next keystroke stamps presence in line.
// A run is paused only after pauseDelayFloor without activity, and a writer's input stamps at least every
// presenceStampEvery, so a run cannot have been paused since an input this recent. Past it, the keystroke waits for
// the stamp, and for any thaw the stamp owes, as every keystroke did before.
const presenceInLineAfter = pauseDelayFloor - presenceStampEvery

// presenceOnInput is a terminal writer's onInput (attach.go, sshgateway_channels.go). It stamps the person's
// presence (markPresent) off the keystroke path, one stamp in flight per holder, so a slow database never holds a
// keystroke. It stamps in line for the holder's first input, for the first after presenceInLineAfter without one,
// and after a stamp that left a thaw waiting for its retry or did not reach the store, so a paused run is thawed before those keystrokes reach it.
func (s *Server) presenceOnInput(ctx context.Context, runID uuid.UUID, actorType types.ActorType, principal string) func() {
	p := &presenceStamper{}
	stamp := func() bool {
		err := s.markPresent(ctx, runID, actorType, principal, "presence")
		return err != nil || s.presenceOwed(runID)
	}
	return func() { p.input(s.cfg.Now(), stamp, s.goBackground) }
}

// presenceOwed reports whether runID's last stamp left work for the next input: a thaw waiting for its retry
// (pauseClocks.thawRetry, read without moving its backoff) or a stamp that did not reach the store (still due).
func (s *Server) presenceOwed(runID uuid.UUID) bool {
	if _, ok := s.cfg.Store.(store.RunPauser); !ok {
		return false
	}
	s.pause.mu.Lock()
	_, thaw := s.pause.thawRetry[runID]
	s.pause.mu.Unlock()
	return thaw || s.pause.stampDue(true, runID, s.cfg.Now())
}

// presenceStamper is one holder's presence stamping.
type presenceStamper struct {
	mu       sync.Mutex
	last     time.Time // the holder's last input
	thawOwed bool      // the last stamp left a thaw or a stamp owed
	inFlight bool      // a stamp off the keystroke path has not returned
}

// input stamps for an input at now: in line (presenceOnInput), else through async unless a stamp is in flight.
func (p *presenceStamper) input(now time.Time, stamp func() bool, async func(func())) {
	p.mu.Lock()
	inLine := p.thawOwed || p.last.IsZero() || now.Sub(p.last) >= presenceInLineAfter
	p.last = now
	if !inLine {
		if p.inFlight {
			p.mu.Unlock()
			return
		}
		p.inFlight = true
	}
	p.mu.Unlock()
	run := func() {
		owed := stamp()
		p.mu.Lock()
		p.thawOwed = owed
		if !inLine {
			p.inFlight = false
		}
		p.mu.Unlock()
	}
	if inLine {
		run()
		return
	}
	async(run)
}
