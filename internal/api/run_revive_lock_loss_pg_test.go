// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A revive's replace context (whileLockHeld) against a real Postgres run lock. Guarded by WARDYN_TEST_PG:
// skipped cleanly when unset, must PASS when set.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// The caller of a revive disconnects, which ends the request context with its own cause for good and
// must not stop the replace; the run lock's connection is then lost, on its own, while the replace
// runs, and that must stop it with db.ErrLockLost.
func TestWhileLockHeld_LockLostAfterTheCallerEndedCancelsTheReplace(t *testing.T) {
	pool := throwawayPGPool(t)
	old := db.LockWatchInterval
	db.LockWatchInterval = 20 * time.Millisecond
	t.Cleanup(func() { db.LockWatchInterval = old })
	ctx := context.Background()
	k := db.NewLockKey(db.RunOpLockClass, uuid.NewString())
	hctx, release, err := db.NewPGLocker(pool, 2).Lock(ctx, k, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	caller, cancelCaller := context.WithCancel(hctx)
	defer cancelCaller()
	replaceCtx, end := whileLockHeld(context.WithoutCancel(caller), caller) // as reviveRun builds it
	defer end()
	cancelCaller()
	select {
	case <-replaceCtx.Done():
		t.Fatalf("the caller's disconnect ended the replace: %v", context.Cause(replaceCtx))
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := pool.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_locks
		WHERE locktype = 'advisory' AND objsubid = 2 AND classid::bigint = $1 AND objid::bigint = $2
		AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`,
		int64(uint32(k.Class)), int64(uint32(k.Obj))); err != nil {
		t.Fatal(err)
	}
	select {
	case <-replaceCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the lock's loss after the caller ended did not stop the replace")
	}
	if cause := context.Cause(replaceCtx); !errors.Is(cause, db.ErrLockLost) {
		t.Errorf("cause = %v, want db.ErrLockLost", cause)
	}
}
