// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// countingLocker counts the locks asked of next: of class alone when class is set.
type countingLocker struct {
	next  db.Locker
	class int32
	n     atomic.Int32
}

func (l *countingLocker) count(k db.LockKey) {
	if l.class == 0 || k.Class == l.class {
		l.n.Add(1)
	}
}

func (l *countingLocker) Lock(ctx context.Context, k db.LockKey, wait time.Duration) (context.Context, func(), error) {
	l.count(k)
	return l.next.Lock(ctx, k, wait)
}

func (l *countingLocker) TryLock(ctx context.Context, k db.LockKey) (context.Context, func(), bool, error) {
	l.count(k)
	return l.next.TryLock(ctx, k)
}

// A run with no pass back to run ends while the lock pool is exhausted: its end publishes and revokes, and
// asks for no run lock; its start fence, refused, is written past (FenceRunStarts). A run whose pass back may run
// still waits for the run lock, and publishes nothing without it.
func TestEnd_TakesTheLockOnlyWhenAPassBackMayRun(t *testing.T) {
	for name, tc := range map[string]struct {
		folder bool
		ends   bool
	}{
		"no folder":           {folder: false, ends: true},
		"a pass back may run": {folder: true, ends: false},
	} {
		t.Run(name, func(t *testing.T) {
			s, cs, _, run, _, _ := computerFolderFixture(t, types.ComputerFolderBoth)
			if !s.holdForLocalFolder(context.Background(), run, "sbx") {
				t.Fatal(cs.FailureHint())
			}
			if !tc.folder {
				cs.mu.Lock()
				cs.folder = nil
				cs.mu.Unlock()
			}
			locks := &countingLocker{next: exhaustedPool(), class: db.RunOpLockClass}
			s.locks.override = locks
			s.reconcileFinalize(context.Background(), run.ID, types.RunFailed, "sbx", "test end")
			revoked := slices.Contains(s.cfg.Broker.(*fakeBroker).revoked, run.ID)
			if ended := cs.State() == types.RunFailed; ended != tc.ends || revoked != tc.ends {
				t.Errorf("state %s, revoked %v; want ended and revoked %v", cs.State(), revoked, tc.ends)
			}
			if n := locks.n.Load(); (n == 0) != tc.ends {
				t.Errorf("%d run locks asked; want none exactly when the run has no pass back to run", n)
			}
		})
	}
}
