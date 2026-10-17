// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A detach of a remote-hosted runner takes the pool's use-policy target lock first, whether or not it
// refuses a governed pool: a policy write or approval holding that lock finishes before the detach reads
// the pool. Without it the detach is ordered by the pool row alone, which a policy write takes later.
func TestPG_RunnerPoolConcurrentDetachWaitsOnThePolicyTargetLock(t *testing.T) {
	for _, refuseGoverned := range []bool{true, false} {
		e := newPoolEnv(t, false)
		a := e.createPool("A", types.RunnerPoolOrganization)
		if w := e.admin(http.MethodPut, poolPath(a.ID)+"/remote-runners/fake", ""); w.Code != http.StatusOK {
			t.Fatalf("attach = %d %s", w.Code, w.Body)
		}
		ctx := context.Background()
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := store.LockGovernanceTarget(ctx, tx, types.GovernanceTargetRunnerPoolUsePolicy, a.ID.String()); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, _, err := e.pg.RemoveRunnerPoolExecutor(ctx, a.ID, "fake", refuseGoverned)
			done <- err
		}()
		select {
		case err := <-done:
			t.Fatalf("refuseGoverned=%v: the detach returned (%v) while the policy target lock was held", refuseGoverned, err)
		case <-time.After(500 * time.Millisecond):
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("refuseGoverned=%v: the detach after the lock was released = %v", refuseGoverned, err)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("refuseGoverned=%v: the detach did not finish after the lock was released", refuseGoverned)
		}
	}
}
