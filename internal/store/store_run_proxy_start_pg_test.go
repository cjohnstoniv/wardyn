// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_RunProxyStarts pins migration 0274 through the readback surface: a report is held until the
// driver's hash is in, equal hashes enforce, a late answer for a superseded generation and a token
// outside the active start's lineage change nothing, a renewed lineage (current and previous jti) is
// accepted, and a passed deadline fails the start. The proxy address round-trips through the run read.
func TestPG_RunProxyStarts(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	run := newRun(types.RunRunning)
	persistRun(t, ctx, pool, run)
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	if _, err := pg.BeginProxyStart(ctx, run.ID, 1, "J0", store.ProxyStartExpected); err != nil {
		t.Fatalf("BeginProxyStart: %v", err)
	}
	ch, err := pg.ReportProxyStart(ctx, run.ID, "J0", sha)
	if err != nil || !ch.Active || ch.Start.State != types.ProxyStartPending || ch.Settled != "" {
		t.Fatalf("early report = %+v, %v; want held pending", ch, err)
	}
	ch, err = pg.AnswerProxyStart(ctx, run.ID, 1, sha, time.Minute)
	if err != nil || !ch.Active || ch.Settled != types.ProxyStartEnforced || ch.Start.ConfirmedAt == nil {
		t.Fatalf("answer = %+v, %v; want enforced", ch, err)
	}

	// Generation 2: a late answer for generation 1 is fenced; a report on J0 is stale.
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET token_jti = 'F', token_prev_jti = NULL WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.BeginProxyStart(ctx, run.ID, 2, "F", store.ProxyStartExpected); err != nil {
		t.Fatal(err)
	}
	if ch, _ := pg.AnswerProxyStart(ctx, run.ID, 1, sha, time.Minute); ch.Active {
		t.Fatal("a late answer for a superseded generation landed")
	}
	if ch, _ := pg.ReportProxyStart(ctx, run.ID, "J0", sha); ch.Active {
		t.Fatal("a report on the previous start's token landed")
	}
	// F renews to R1, then R1 to R2 (stamped, maybe never delivered): R1 is the previous jti, accepted.
	if ok, err := pg.StampRunTokenRenewed(ctx, run.ID, "R1", "F"); !ok || err != nil {
		t.Fatalf("renew F->R1: %v %v", ok, err)
	}
	if ok, err := pg.StampRunTokenRenewed(ctx, run.ID, "R2", "R1"); !ok || err != nil {
		t.Fatalf("renew R1->R2: %v %v", ok, err)
	}
	if ch, err := pg.ReportProxyStart(ctx, run.ID, "R1", sha); err != nil || !ch.Active {
		t.Fatalf("report on R1 = %+v, %v; want accepted", ch, err)
	}
	ch, err = pg.AnswerProxyStart(ctx, run.ID, 2, sha, -time.Second) // its deadline already passed
	if err != nil || ch.Settled != types.ProxyStartFailed || ch.Reason != store.ProxyStartDeadline {
		t.Fatalf("answer past the deadline = %+v, %v; want failed deadline", ch, err)
	}

	// Generation 3: no hash fails at once; an abandoned generation 4 leaves 3 active.
	if _, err := pg.BeginProxyStart(ctx, run.ID, 3, "G", store.ProxyStartExpected); err != nil {
		t.Fatal(err)
	}
	if ch, _ := pg.AnswerProxyStart(ctx, run.ID, 3, "", time.Minute); ch.Settled != types.ProxyStartFailed || ch.Reason != store.ProxyStartHashMissing {
		t.Fatalf("answer without a hash = %+v; want failed hash_missing", ch)
	}
	if _, err := pg.BeginProxyStart(ctx, run.ID, 4, "H", store.ProxyStartExpected); err != nil {
		t.Fatal(err)
	}
	if err := pg.AbandonProxyStart(ctx, run.ID, 4); err != nil {
		t.Fatal(err)
	}
	if p, ok, err := pg.ActiveProxyStart(ctx, run.ID); err != nil || !ok || p.Generation != 3 {
		t.Fatalf("active start = %+v %v %v; want generation 3", p, ok, err)
	}

	if err := pg.SetRunProxyAddr(ctx, run.ID, "172.30.0.2"); err != nil {
		t.Fatal(err)
	}
	got, err := pg.GetRun(ctx, run.ID)
	if err != nil || got.ProxyAddr != "172.30.0.2" {
		t.Fatalf("run proxy address = %q, %v; want 172.30.0.2", got.ProxyAddr, err)
	}
	other := newRun(types.RunRunning)
	persistRun(t, ctx, pool, other)
	if got, _ := pg.GetRun(ctx, other.ID); got.ProxyAddr != "" {
		t.Errorf("a run with no address reads %q, want empty", got.ProxyAddr)
	}
}
