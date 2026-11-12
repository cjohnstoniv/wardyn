// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// An end hold is a run's lock held from its terminal write through its tail (holdRunEnd). Each hold has an
// outer deadline, B from when it was taken, carried on its ctx (withHoldDeadline): the tail's floors are capped
// at it (teardownCtxWithin, settleAfterTeardown, the pass back's budget), and past it the holder neither stops
// the sandbox nor records anything. The readers (the containment loop and the tail owner) finish the run from
// there. Each B is the holder's own, and a macOS VM run's gets macOSVMTeardownBudget more (endHoldBudget),
// which its sandbox stop alone can use.
const (
	endHoldSweep     = 60 * time.Second // a sweep's end: reconcile finalize, a lease or kept run's end, a Mac-prep failure
	endHoldDedicated = 3 * time.Minute  // the completion watcher, a person's bring-back and the tail owner
	endHoldReaper    = 2 * time.Minute  // the idle and max-age stop (LockRunEnd)
	// A runner's exit receipt holds for runnerExitTimeout: 30 s, plus a macOS VM's stop.
)

// maxEndHolds is how many end holds one process may hold at once, out of the lock pool's connections, so the
// ends cannot take every connection the other lock users need.
const maxEndHolds = 4

// errHoldPassed is the cause of a ctx done because its end hold's deadline passed.
var errHoldPassed = errors.New("the run's end hold ran out; the background owner finishes it")

// holdDeadlineKey carries an end hold's outer deadline on its ctx.
type holdDeadlineKey struct{}

// withHoldDeadline is ctx carrying dl as its end hold's outer deadline.
func withHoldDeadline(ctx context.Context, dl time.Time) context.Context {
	return context.WithValue(ctx, holdDeadlineKey{}, dl)
}

// holdDeadline is the outer deadline of the end hold ctx is under, if it is under one.
func holdDeadline(ctx context.Context) (time.Time, bool) {
	dl, ok := ctx.Value(holdDeadlineKey{}).(time.Time)
	return dl, ok
}

// holdPassed reports whether ctx is under an end hold whose outer deadline has passed.
func holdPassed(ctx context.Context) bool {
	dl, ok := holdDeadline(ctx)
	return ok && !time.Now().Before(dl)
}

// endHoldSlots is the end-hold semaphore: at most maxEndHolds taken at once. Zero value is ready to use.
type endHoldSlots struct {
	mu sync.Mutex
	n  int
}

// take takes a slot, or reports false when every slot is taken.
func (e *endHoldSlots) take() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.n >= maxEndHolds {
		return false
	}
	e.n++
	return true
}

// release gives a slot back.
func (e *endHoldSlots) release() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.n--
}

// holdRunEnd takes an end hold on run id for b: a slot of the end-hold semaphore, then the run's lock (lockRunOp),
// on a ctx that carries the hold's deadline, now plus b. Every slot taken is a refusal that wraps
// db.ErrLockNoCapacity, so db.LockRefused is true for it and writeLockRefused answers it as busy. A ctx already
// under an end hold re-enters it: no second slot, and the earlier of the two deadlines. unlock releases the lock,
// then the slot.
func (s *Server) holdRunEnd(ctx context.Context, id uuid.UUID, b time.Duration) (context.Context, func(), error) {
	dl := time.Now().Add(b)
	outer, nested := holdDeadline(ctx)
	if nested {
		dl = minTime(dl, outer)
	} else if !s.endHolds.take() {
		return ctx, nil, fmt.Errorf("%w: all %d end holds of this server are taken", db.ErrLockNoCapacity, maxEndHolds)
	}
	lctx, unlock, err := s.lockRunOp(ctx, id)
	if err != nil {
		if !nested {
			s.endHolds.release()
		}
		return ctx, nil, err
	}
	return withHoldDeadline(lctx, dl), func() {
		unlock()
		if !nested {
			s.endHolds.release()
		}
	}, nil
}

// tryHoldRunEnd is holdRunEnd that gives up instead of waiting for the lock (tryLockRunOpHeld), for the sweeps that
// only try. ok false with held true is a busy refusal: the lock found held, or every slot taken (a full semaphore
// is busy, as holdRunEnd's refusal is); held false is a lock that could not be asked.
func (s *Server) tryHoldRunEnd(ctx context.Context, id uuid.UUID, b time.Duration) (context.Context, func(), bool, bool) {
	dl := time.Now().Add(b)
	outer, nested := holdDeadline(ctx)
	if nested {
		dl = minTime(dl, outer)
	} else if !s.endHolds.take() {
		return ctx, nil, false, true
	}
	lctx, unlock, ok, held := s.tryLockRunOpHeld(ctx, id)
	if !ok {
		if !nested {
			s.endHolds.release()
		}
		return ctx, nil, false, held
	}
	return withHoldDeadline(lctx, dl), func() {
		unlock()
		if !nested {
			s.endHolds.release()
		}
	}, true, false
}

// endHoldBudget is base, plus macOSVMTeardownBudget for a macOS VM run.
func (s *Server) endHoldBudget(ctx context.Context, id uuid.UUID, base time.Duration) time.Duration {
	if s.macOSVMRun(ctx, id) {
		return base + macOSVMTeardownBudget
	}
	return base
}
