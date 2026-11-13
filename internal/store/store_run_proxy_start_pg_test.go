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

// TestPG_RunProxyStarts pins migration 0275 through the readback surface: a report is held until the
// driver's hash is in, equal hashes enforce, a late answer for a superseded generation and a token
// outside the active start's lineage change nothing, a renewed lineage (current and previous jti) is
// accepted, and a passed deadline fails the start. A failed start's stop is retried until it is
// recorded contained. The proxy address round-trips through the run read.
func TestPG_RunProxyStarts(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	run := newRun(types.RunRunning)
	persistRun(t, ctx, pool, run)
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	if _, err := pg.BeginProxyStart(ctx, run.ID, 1, "J0", store.ProxyStartExpected, false); err != nil {
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
	if _, err := pg.BeginProxyStart(ctx, run.ID, 2, "F", store.ProxyStartExpected, false); err != nil {
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
	if _, err := pg.BeginProxyStart(ctx, run.ID, 3, "G", store.ProxyStartExpected, false); err != nil {
		t.Fatal(err)
	}
	if ch, _ := pg.AnswerProxyStart(ctx, run.ID, 3, "", time.Minute); ch.Settled != types.ProxyStartFailed || ch.Reason != store.ProxyStartHashMissing {
		t.Fatalf("answer without a hash = %+v; want failed hash_missing", ch)
	}
	if _, err := pg.BeginProxyStart(ctx, run.ID, 4, "H", store.ProxyStartExpected, false); err != nil {
		t.Fatal(err)
	}
	if err := pg.AbandonProxyStart(ctx, run.ID, 4); err != nil {
		t.Fatal(err)
	}
	if p, ok, err := pg.ActiveProxyStart(ctx, run.ID); err != nil || !ok || p.Generation != 3 {
		t.Fatalf("active start = %+v %v %v; want generation 3", p, ok, err)
	}

	// Containment: only the active failed start is retried; a failed stop keeps it there, a confirmed
	// one takes it out and stays.
	overdue := func() []int {
		starts, err := pg.OverdueProxyStarts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var gens []int
		for _, p := range starts {
			if p.RunID == run.ID {
				gens = append(gens, p.Generation)
			}
		}
		return gens
	}
	if gens := overdue(); len(gens) != 1 || gens[0] != 3 {
		t.Fatalf("uncontained starts = %v, want [3] (generation 2 is no longer active)", gens)
	}
	if err := pg.RecordProxyContainment(ctx, run.ID, 3, "daemon down"); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := pg.ActiveProxyStart(ctx, run.ID); p.ContainedAt != nil || p.ContainmentError != "daemon down" || len(overdue()) != 1 {
		t.Fatalf("after a failed stop: %+v; want unresolved and still retried", p)
	}
	for _, e := range []string{"", "a later failure"} {
		if err := pg.RecordProxyContainment(ctx, run.ID, 3, e); err != nil {
			t.Fatal(err)
		}
	}
	if p, _, _ := pg.ActiveProxyStart(ctx, run.ID); p.ContainedAt == nil || p.ContainmentError != "" || len(overdue()) != 0 {
		t.Fatalf("after a confirmed stop: %+v; want contained, kept, and no longer retried", p)
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

// TestPG_ProxyStartInPlace pins migration 0278: a start begun in place reads back in place, through the
// begin and through ActiveProxyStart, and a begin again under the same generation resets the flag with
// the rest of the row.
func TestPG_ProxyStartInPlace(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	run := newRun(types.RunRunning)
	persistRun(t, ctx, pool, run)

	p, err := pg.BeginProxyStart(ctx, run.ID, 1, "J-in-place", store.ProxyStartExpected, true)
	if err != nil || !p.InPlace {
		t.Fatalf("begin in place = %+v, %v; want InPlace", p, err)
	}
	if a, found, err := pg.ActiveProxyStart(ctx, run.ID); err != nil || !found || !a.InPlace {
		t.Fatalf("active = %+v, %v, %v; want the in-place start", a, found, err)
	}
	if p, err = pg.BeginProxyStart(ctx, run.ID, 1, "J-replace", store.ProxyStartExpected, false); err != nil || p.InPlace {
		t.Fatalf("begin again by a replace = %+v, %v; want InPlace false", p, err)
	}
	if a, found, err := pg.ActiveProxyStart(ctx, run.ID); err != nil || !found || a.InPlace || a.JTI != "J-replace" {
		t.Fatalf("active after the reset = %+v, %v, %v; want J-replace, not in place", a, found, err)
	}
}

// TestPG_Migration0278InPlace pins migration 0278 itself. Up: in_place is a NOT NULL boolean defaulting to
// false. Default and down: a start written the way a release before 0278 writes it (no in_place) lands and
// reads as not in place, so an older binary on the migrated schema keeps working; this repo has no down
// migrations, and going back is a database restore (docs/OPERATIONS.md's migration list says so).
func TestPG_Migration0278InPlace(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	var typ, nullable string
	var def *string
	if err := pool.QueryRow(ctx, `SELECT data_type, is_nullable, column_default FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'run_proxy_starts' AND column_name = 'in_place'`).Scan(&typ, &nullable, &def); err != nil {
		t.Fatalf("read the in_place column: %v", err)
	}
	if typ != "boolean" || nullable != "NO" || def == nil || *def != "false" {
		t.Fatalf("in_place = %s, nullable %s, default %v; want boolean NOT NULL DEFAULT false", typ, nullable, def)
	}
	run := newRun(types.RunRunning)
	persistRun(t, ctx, pool, run)
	if _, err := pool.Exec(ctx, `INSERT INTO run_proxy_starts (run_id, generation, jti, mode, started_at, state)
		VALUES ($1, 1, 'J-old', 'expected', now(), 'pending')`, run.ID); err != nil {
		t.Fatalf("a start written without in_place, as before 0278: %v", err)
	}
	if a, found, err := pg.ActiveProxyStart(ctx, run.ID); err != nil || !found || a.InPlace || a.JTI != "J-old" {
		t.Fatalf("active = %+v, %v, %v; want J-old, not in place", a, found, err)
	}
}
