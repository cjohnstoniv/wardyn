// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// queuedAsks is the in-process locker that counts each run start lock asked for through Lock and through
// LockQueued.
type queuedAsks struct {
	db.Locker
	mu            sync.Mutex
	locks, queued int
}

func (l *queuedAsks) Lock(ctx context.Context, k db.LockKey, wait time.Duration) (context.Context, func(), error) {
	if k.Class == db.RunStartLockClass {
		l.mu.Lock()
		l.locks++
		l.mu.Unlock()
	}
	return l.Locker.Lock(ctx, k, wait)
}

func (l *queuedAsks) LockQueued(ctx context.Context, k db.LockKey, wait time.Duration) (context.Context, func(), error) {
	if k.Class == db.RunStartLockClass {
		l.mu.Lock()
		l.queued++
		l.mu.Unlock()
	}
	return l.Locker.Lock(ctx, k, wait)
}

func (l *queuedAsks) counts() (locks, queued int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.locks, l.queued
}

// TestEndWritersTakeTheQueuedPath: a write that ends a run (FenceRunStarts, and fenceRunsStarts for every run of a
// revoke) asks for the start lock only through LockQueued, which waits in the database's queue so a stream of starts
// cannot starve it; a start (lockStart) asks only through Lock, the fence path.
// Catches: an end-writer on the fence's try-and-poll, which back-to-back starts on its run could keep out past its
// wait.
func TestEndWritersTakeTheQueuedPath(t *testing.T) {
	l := &queuedAsks{Locker: db.NewLocalLocker()}
	s := &Server{}
	s.locks.override = l
	ctx := context.Background()

	s.FenceRunStarts(ctx, uuid.New())()
	if locks, queued := l.counts(); locks != 0 || queued != 1 {
		t.Fatalf("FenceRunStarts asked %d through Lock and %d through LockQueued; want 0 and 1", locks, queued)
	}
	s.fenceRunsStarts(ctx, []types.AgentRun{{ID: uuid.New()}, {ID: uuid.New()}})()
	if locks, queued := l.counts(); locks != 0 || queued != 3 {
		t.Fatalf("after fenceRunsStarts of two runs, %d through Lock and %d through LockQueued; want 0 and 3", locks, queued)
	}
	h, err := s.lockStart(ctx, uuid.New())
	if err != nil {
		t.Fatalf("lockStart: %v", err)
	}
	h.release()
	if locks, queued := l.counts(); locks != 1 || queued != 3 {
		t.Fatalf("after lockStart, %d through Lock and %d through LockQueued; want 1 and 3", locks, queued)
	}
}
