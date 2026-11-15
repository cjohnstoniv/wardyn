// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// An edit's detached window (editDetached) against a real Postgres run lock. Guarded by WARDYN_TEST_PG: skipped
// cleanly when unset, must PASS when set.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// The caller of an edit stops waiting, which must not end its detached window; the run lock's connection is then
// lost, on its own, and that must end the window with db.ErrLockLost: before the edit's deadline, and in a stop's
// window that runs past it.
func TestEditDetached_LockLostEndsItBeforeAndPastTheEditsDeadline(t *testing.T) {
	old := db.LockWatchInterval
	db.LockWatchInterval = 20 * time.Millisecond
	t.Cleanup(func() { db.LockWatchInterval = old })
	for _, c := range []struct {
		name string
		dl   time.Duration // the edit's deadline, from now
	}{
		{"before the deadline", time.Hour},
		{"in a stop's window past the deadline", -time.Second},
	} {
		pool := throwawayPGPool(t)
		ctx := context.Background()
		k := db.NewLockKey(db.RunOpLockClass, uuid.NewString())
		hctx, release, err := db.NewPGLocker(pool, 2).Lock(ctx, k, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		caller, cancelCaller := context.WithCancel(withEditDeadline(hctx, time.Now().Add(c.dl)))
		win, end := editDetached(caller, killCascadeTimeout)
		cancelCaller()
		select {
		case <-win.Done():
			t.Errorf("%s: the caller's end ended the window: %v", c.name, context.Cause(win))
		case <-time.After(200 * time.Millisecond):
		}
		if _, err := pool.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_locks
			WHERE locktype = 'advisory' AND objsubid = 2 AND classid::bigint = $1 AND objid::bigint = $2
			AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`,
			int64(uint32(k.Class)), int64(uint32(k.Obj))); err != nil {
			t.Fatal(err)
		}
		select {
		case <-win.Done():
		case <-time.After(5 * time.Second):
			t.Errorf("%s: the lock's loss did not end the window", c.name)
		}
		if cause := context.Cause(win); !errors.Is(cause, db.ErrLockLost) {
			t.Errorf("%s: cause = %v, want db.ErrLockLost", c.name, cause)
		}
		end()
		release()
	}
}
