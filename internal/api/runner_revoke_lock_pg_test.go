// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// FUG-L8: a revoke behind a lock another operation holds on the runner row (the audit spool's ingest
// holds it FOR SHARE) is refused within its lock timeout, 503 lock_unavailable with Retry-After 5, and
// revokes nothing: the runner stays claimed and routed. Once the holder lets go, the retry lands.
func TestPG_RevokeBehindAHeldRunnerLockAnswersARetry(t *testing.T) {
	e := newPlacementEnv(t)
	a := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2)
	e.stream(a, types.CC1, types.CC2)
	ctx := context.Background()
	// The hold is on a pool of its own: this goroutine reads e.pool while it holds the lock
	// (WARDYN_TEST_POOL_GUARD).
	pool, err := pgxpool.New(ctx, e.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck // released below; a no-op then
	// The server ends the holder after 15s idle, so a revoke with no lock timeout of its own lands then
	// and fails the 503 check below instead of hanging the test.
	if _, err := holder.Exec(ctx, `SET LOCAL idle_in_transaction_session_timeout = '15s'`); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT 1 FROM runners WHERE id = $1 FOR SHARE`, a); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	w := e.call(e.person, http.MethodDelete, "/api/v1/me/runners/"+a.String(), "")
	if took := time.Since(start); w.Code != http.StatusServiceUnavailable || wireReason(t, w) != reasonLockUnavailable || w.Header().Get("Retry-After") != "5" {
		t.Fatalf("revoke behind a held lock = %d %s (Retry-After %q) after %s, want 503 %s with Retry-After 5",
			w.Code, w.Body, w.Header().Get("Retry-After"), took, reasonLockUnavailable)
	}
	if got, err := e.pg.GetRunner(ctx, a); err != nil || got.State != types.RunnerClaimed {
		t.Fatalf("after the refused revoke the runner is %s (%v), want claimed", got.State, err)
	}
	e.srv.runnerStreams.subsMu.Lock()
	_, routed := e.srv.runnerStreams.subs[a]
	e.srv.runnerStreams.subsMu.Unlock()
	if !routed {
		t.Error("the refused revoke left the runner unrouted")
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if w := e.call(e.person, http.MethodDelete, "/api/v1/me/runners/"+a.String(), ""); w.Code != http.StatusOK {
		t.Fatalf("the retry once the lock is free = %d %s, want 200", w.Code, w.Body)
	}
}
