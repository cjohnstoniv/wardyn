// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// maskFences runs one fence loop per run on this replica, however many in-flight consumers the run has (#4069):
// each beat one read of the run's coverage, and on the first miss every consumer still open is ended.
type maskFences struct {
	mu   sync.Mutex
	runs map[uuid.UUID]*runMaskFence
	// read replaces maskCovered (tests only).
	read func(ctx context.Context, runID uuid.UUID) bool
}

type runMaskFence struct {
	subs map[*maskFenceSub]struct{}
	stop context.CancelFunc
}

type maskFenceSub struct {
	ctx    context.Context
	end    func()
	fenced atomic.Bool
}

// joinMaskFence adds sub to runID's loop, starting the loop for the run's first consumer.
func (s *Server) joinMaskFence(runID uuid.UUID, sub *maskFenceSub) {
	f := &s.maskFences
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runs == nil {
		f.runs = map[uuid.UUID]*runMaskFence{}
	}
	r := f.runs[runID]
	if r == nil {
		parent := context.Background() // a Server literal in a test (maskDoorServer) has no BaseCtx; New sets it
		if s.cfg.BaseCtx != nil {
			// Not cancelled with BaseCtx: at shutdown a consumer can stream until its own context ends, and it
			// stays fenced until then. Only the run's last consumer leaving, or the first miss, stops the loop.
			parent = context.WithoutCancel(s.cfg.BaseCtx)
		}
		ctx, stop := context.WithCancel(parent)
		r = &runMaskFence{subs: map[*maskFenceSub]struct{}{}, stop: stop}
		f.runs[runID] = r
		go s.runMaskFence(ctx, runID, r)
	}
	r.subs[sub] = struct{}{}
	context.AfterFunc(sub.ctx, func() { s.leaveMaskFence(runID, r, sub) })
}

// leaveMaskFence drops sub; the run's last consumer to leave stops its loop.
func (s *Server) leaveMaskFence(runID uuid.UUID, r *runMaskFence, sub *maskFenceSub) {
	f := &s.maskFences
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(r.subs, sub)
	if len(r.subs) == 0 && f.runs[runID] == r {
		delete(f.runs, runID)
		r.stop()
	}
}

func (s *Server) runMaskFence(ctx context.Context, runID uuid.UUID, r *runMaskFence) {
	read := s.maskFences.read
	if read == nil {
		read = s.maskCovered
	}
	t := time.NewTicker(s.maskCheckPeriod())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if ctx.Err() != nil { // stopped while this beat was due
				return
			}
			if read(ctx, runID) || ctx.Err() != nil {
				continue
			}
			f := &s.maskFences
			f.mu.Lock()
			subs := make([]*maskFenceSub, 0, len(r.subs))
			for sub := range r.subs {
				subs = append(subs, sub)
			}
			if f.runs[runID] == r {
				delete(f.runs, runID) // a consumer that joins from now starts a fresh loop
			}
			f.mu.Unlock()
			r.stop()
			for _, sub := range subs {
				if sub.ctx.Err() == nil {
					sub.fenced.Store(true)
					sub.end()
				}
			}
			return
		}
	}
}
