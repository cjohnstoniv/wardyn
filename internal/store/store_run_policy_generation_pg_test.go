// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests for agent_runs.policy_generations: each revive's policy generation, with the
// run tokens each was enforced under. Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestRunPolicyGeneration_AdvanceAndAbandon: a revive starts generation 2 and moves the run's digest;
// an abandoned one puts the digest back and its number is never reused; RunPolicyStateOf reads the
// generations and the live token lineage in one row read.
func TestRunPolicyGeneration_AdvanceAndAbandon(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	r := newRun(types.RunRunning)
	persistRun(t, ctx, pool, r)
	if err := pg.SetRunPolicyDigest(ctx, r.ID, "d1"); err != nil {
		t.Fatal(err)
	}
	if gen, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, store.PolicySlot{Generation: 1}, "d2", []string{"jti-1", ""}, "jti-2", nil); err != nil || gen != 2 {
		t.Fatalf("advance = %d, %v; want 2", gen, err)
	}
	if _, _, err := pg.RetireRunTokenLineage(ctx, r.ID, "jti-2"); err != nil {
		t.Fatal(err)
	}
	st, found, err := store.RunPolicyStateOf(ctx, pool, r.ID)
	if err != nil || !found {
		t.Fatalf("state: found %v, %v", found, err)
	}
	if st.Digest != "d2" || len(st.Generations) != 2 || st.Stamp(true, "jti-1") != (types.PolicyStamp{Digest: "d1", Generation: 1}) ||
		st.Stamp(true, "jti-2") != (types.PolicyStamp{Digest: "d2", Generation: 2}) {
		t.Errorf("state after advance = %+v", st)
	}
	if got, _ := pg.GetRun(ctx, r.ID); got.PolicyDigest != "d2" {
		t.Errorf("GetRun digest = %q, want the current generation's d2", got.PolicyDigest)
	}
	if err := pg.AbandonRunPolicyGeneration(ctx, r.ID, 2, nil); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := store.RunPolicyStateOf(ctx, pool, r.ID); st.Digest != "d1" || st.Stamp(false, "") != (types.PolicyStamp{Digest: "d1", Generation: 1}) {
		t.Errorf("state after abandon = %+v, want d1 current", st)
	}
	if st, found, err := pg.RunPolicyState(ctx, r.ID); err != nil || !found || st.CurrentGeneration() != 1 {
		t.Errorf("RunPolicyState after abandon = %+v (found %v, err %v), want generation 1 current", st, found, err)
	}
	if gen, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, store.PolicySlot{Generation: 2, Abandoned: true}, "d3", nil, "jti-3", nil); err != nil || gen != 3 {
		t.Errorf("the next advance = %d, %v; want 3", gen, err)
	}
	if _, err := pg.AdvanceRunPolicyGeneration(ctx, uuid.New(), store.PolicySlot{Generation: 1}, "d", nil, "j", nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an absent run = %v, want ErrNotFound", err)
	}
}

// policyRowSnapshot is everything a generation write could touch: the row's policy columns and token
// lineage, the stored config, and the active proxy start.
type policyRowSnapshot struct {
	state  types.RunPolicyState
	sealed string
	start  store.ProxyStart
}

func snapshotPolicyRow(t *testing.T, ctx context.Context, pg store.PG, id uuid.UUID) policyRowSnapshot {
	t.Helper()
	st, _, err := store.RunPolicyStateOf(ctx, pg.Pool, id)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := pg.GetRunProxyConfig(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := pg.ActiveProxyStart(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return policyRowSnapshot{state: st, sealed: string(sealed), start: p}
}

// TestRunPolicyGeneration_AdvanceOnAStaleGenerationChangesNothing: an advance whose expected generation
// is not the newest (a number another revive moved on, an abandoned one's predecessor) or whose run is
// lost is ErrRunPolicyMoved and writes nothing: not the generations, the digest, the token lineage, the
// stored config or the start. The newest number, abandoned ones included, is what lands.
func TestRunPolicyGeneration_AdvanceOnAStaleGenerationChangesNothing(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	r := newRun(types.RunRunning)
	persistRun(t, ctx, pool, r)
	if err := pg.SetRunPolicyDigest(ctx, r.ID, "d1"); err != nil {
		t.Fatal(err)
	}
	if err := pg.PutRunProxyConfig(ctx, r.ID, []byte("sealed-1")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pg.RetireRunTokenLineage(ctx, r.ID, "jti-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.BeginProxyStart(ctx, r.ID, 1, "jti-1", store.ProxyStartExpected); err != nil {
		t.Fatal(err)
	}
	if gen, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, store.PolicySlot{Generation: 1}, "d2", nil, "jti-2", []byte("sealed-2")); err != nil || gen != 2 {
		t.Fatalf("advance = %d, %v; want 2", gen, err)
	}
	if err := pg.AbandonRunPolicyGeneration(ctx, r.ID, 2, []byte("sealed-1")); err != nil {
		t.Fatal(err)
	}
	before := snapshotPolicyRow(t, ctx, pg, r.ID)
	if before.sealed != "sealed-1" || before.state.CurrentGeneration() != 1 {
		t.Fatalf("after the abandon: config %q, current %d; want sealed-1 restored and generation 1 current", before.sealed, before.state.CurrentGeneration())
	}
	for _, expected := range []int{1, 3} { // generation 1 is current again, but 2 is the newest
		if _, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, store.PolicySlot{Generation: expected}, "d3", []string{"jti-1"}, "jti-3", []byte("sealed-3")); !errors.Is(err, store.ErrRunPolicyMoved) {
			t.Fatalf("advance expecting %d = %v, want ErrRunPolicyMoved", expected, err)
		}
		if after := snapshotPolicyRow(t, ctx, pg, r.ID); !reflect.DeepEqual(after, before) {
			t.Fatalf("a refused advance expecting %d changed the row: %+v, was %+v", expected, after, before)
		}
	}
	if _, err := pg.MarkRunLost(ctx, r.ID, types.LostOutage, time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	// The slot as it now reads (2, abandoned), so only the lost mark refuses it.
	if _, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, store.PolicySlot{Generation: 2, Abandoned: true}, "d3", nil, "jti-3", []byte("sealed-3")); !errors.Is(err, store.ErrRunPolicyMoved) {
		t.Fatalf("advance of a lost run = %v, want ErrRunPolicyMoved", err)
	}
	if after := snapshotPolicyRow(t, ctx, pg, r.ID); !reflect.DeepEqual(after, before) {
		t.Fatalf("a refused advance of a lost run changed the row: %+v, was %+v", after, before)
	}
}

// TestRunPolicyGeneration_LateAbandonLeavesTheWinnerAlone: revive A advances to 2 and stores its
// config; B, expecting 2, advances to 3 with its own; A's abandon of 2 then lands late and changes
// nothing: B's generation stays current and B's config stays stored.
func TestRunPolicyGeneration_LateAbandonLeavesTheWinnerAlone(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	r := newRun(types.RunRunning)
	persistRun(t, ctx, pool, r)
	if err := pg.PutRunProxyConfig(ctx, r.ID, []byte("sealed-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, store.PolicySlot{Generation: 1}, "d-a", nil, "jti-a", []byte("sealed-a")); err != nil {
		t.Fatal(err)
	}
	if gen, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, store.PolicySlot{Generation: 2}, "d-b", nil, "jti-b", []byte("sealed-b")); err != nil || gen != 3 {
		t.Fatalf("B's advance = %d, %v; want 3", gen, err)
	}
	before := snapshotPolicyRow(t, ctx, pg, r.ID)
	if err := pg.AbandonRunPolicyGeneration(ctx, r.ID, 2, []byte("sealed-1")); err != nil {
		t.Fatal(err)
	}
	after := snapshotPolicyRow(t, ctx, pg, r.ID)
	if !reflect.DeepEqual(after, before) || after.sealed != "sealed-b" || after.state.CurrentGeneration() != 3 || after.state.Digest != "d-b" {
		t.Errorf("after A's late abandon: %+v, want B's generation 3 and config untouched (%+v)", after, before)
	}
}

// TestRunPolicyGeneration_AdvanceOfARunNotRunningChangesNothing: a run that is not RUNNING (here still
// starting) takes no revive's generation.
func TestRunPolicyGeneration_AdvanceOfARunNotRunningChangesNothing(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	r := newRun(types.RunStarting)
	persistRun(t, ctx, pool, r)
	if _, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, store.PolicySlot{Generation: 1}, "d2", nil, "jti-2", nil); !errors.Is(err, store.ErrRunPolicyMoved) {
		t.Fatalf("advance of a starting run = %v, want ErrRunPolicyMoved", err)
	}
	if st, _, _ := store.RunPolicyStateOf(ctx, pool, r.ID); len(st.Generations) != 0 {
		t.Errorf("generations = %+v, want none", st.Generations)
	}
}

// TestRunPolicyGeneration_RepeatedAbandonChangesNothing: a second abandon of the same generation, with a
// different config to restore, changes neither the generations nor the config the first one restored.
func TestRunPolicyGeneration_RepeatedAbandonChangesNothing(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	r := newRun(types.RunRunning)
	persistRun(t, ctx, pool, r)
	if err := pg.PutRunProxyConfig(ctx, r.ID, []byte("sealed-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, store.PolicySlot{Generation: 1}, "d2", nil, "jti-2", []byte("sealed-2")); err != nil {
		t.Fatal(err)
	}
	if err := pg.AbandonRunPolicyGeneration(ctx, r.ID, 2, []byte("sealed-1")); err != nil {
		t.Fatal(err)
	}
	before := snapshotPolicyRow(t, ctx, pg, r.ID)
	if err := pg.AbandonRunPolicyGeneration(ctx, r.ID, 2, []byte("sealed-other")); err != nil {
		t.Fatalf("a repeated abandon = %v, want no error", err)
	}
	if after := snapshotPolicyRow(t, ctx, pg, r.ID); !reflect.DeepEqual(after, before) {
		t.Errorf("a repeated abandon changed the row: %+v, was %+v", after, before)
	}
}

// failConfigWriteTrigger makes every run_proxy_configs write of the bytes bad fail, as a write the
// database refuses would, until the test ends.
func failConfigWriteTrigger(t *testing.T, ctx context.Context, pg store.PG, bad string) {
	t.Helper()
	name := "test_fail_config_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	for _, q := range []string{
		`CREATE FUNCTION ` + name + `() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.sealed = convert_to('` + bad + `', 'UTF8') THEN RAISE EXCEPTION 'injected config write failure'; END IF;
			RETURN NEW;
		END $$`,
		`CREATE TRIGGER ` + name + ` BEFORE INSERT OR UPDATE ON run_proxy_configs FOR EACH ROW EXECUTE FUNCTION ` + name + `()`,
	} {
		if _, err := pg.Pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = pg.Pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS `+name+` ON run_proxy_configs`)
		_, _ = pg.Pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS `+name+`()`)
	})
}

// TestRunPolicyGeneration_ConfigWriteFailureRollsBackTheGeneration: a config the advance or the abandon
// cannot write rolls its generation change back with it: the advance takes no number, and the abandon
// leaves its generation current with the config it stored.
func TestRunPolicyGeneration_ConfigWriteFailureRollsBackTheGeneration(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	r := newRun(types.RunRunning)
	persistRun(t, ctx, pool, r)
	if err := pg.PutRunProxyConfig(ctx, r.ID, []byte("sealed-1")); err != nil {
		t.Fatal(err)
	}
	failConfigWriteTrigger(t, ctx, pg, "fail-write")
	before := snapshotPolicyRow(t, ctx, pg, r.ID)
	if _, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, store.PolicySlot{Generation: 1}, "d2", nil, "jti-2", []byte("fail-write")); err == nil {
		t.Fatal("an advance whose config write fails must fail")
	}
	if after := snapshotPolicyRow(t, ctx, pg, r.ID); !reflect.DeepEqual(after, before) {
		t.Fatalf("a failed advance changed the row: %+v, was %+v", after, before)
	}
	if _, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, store.PolicySlot{Generation: 1}, "d2", nil, "jti-2", []byte("sealed-2")); err != nil {
		t.Fatal(err)
	}
	before = snapshotPolicyRow(t, ctx, pg, r.ID)
	if err := pg.AbandonRunPolicyGeneration(ctx, r.ID, 2, []byte("fail-write")); err == nil {
		t.Fatal("an abandon whose restore fails must fail")
	}
	after := snapshotPolicyRow(t, ctx, pg, r.ID)
	if !reflect.DeepEqual(after, before) || after.state.CurrentGeneration() != 2 || after.sealed != "sealed-2" {
		t.Errorf("a failed restore changed the row: %+v, want generation 2 current with sealed-2 (%+v)", after, before)
	}
}

// TestRunPolicyGeneration_LostMarkAtAStaleGenerationChangesNothing: a lost mark fenced on a generation
// lands only while that generation is the run's newest, abandoned ones included, so a revive whose lock
// was lost never loses a run a newer revive took over.
func TestRunPolicyGeneration_LostMarkAtAStaleGenerationChangesNothing(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	r := newRun(types.RunRunning)
	persistRun(t, ctx, pool, r)
	if _, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, store.PolicySlot{Generation: 1}, "d-a", nil, "jti-a", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, store.PolicySlot{Generation: 2}, "d-b", nil, "jti-b", nil); err != nil {
		t.Fatal(err)
	}
	if err := pg.AbandonRunPolicyGeneration(ctx, r.ID, 3, nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, stale := range []int{1, 2} {
		if applied, err := pg.MarkRunLostAtGeneration(ctx, r.ID, stale, types.LostOutage, now, 0); err != nil || applied {
			t.Fatalf("lost mark at generation %d = %v, %v; want not applied, no error", stale, applied, err)
		}
	}
	if run, err := pg.GetRun(ctx, r.ID); err != nil || run.LostAt != nil {
		t.Fatalf("after stale lost marks: lost %v, %v; want not lost", run.LostAt, err)
	}
	if applied, err := pg.MarkRunLostAtGeneration(ctx, r.ID, 3, types.LostOutage, now, 0); err != nil || !applied {
		t.Fatalf("lost mark at the newest generation = %v, %v; want applied", applied, err)
	}
	if run, err := pg.GetRun(ctx, r.ID); err != nil || run.LostAt == nil || run.LostReason != types.LostOutage {
		t.Errorf("after the newest generation's lost mark: lost %v %q, %v; want lost (outage)", run.LostAt, run.LostReason, err)
	}
}

// TestRunPolicyGeneration_RestoreLostFateOnlyAtTheNewestGeneration: a lost run's fate, read before a
// revive's claim cleared it, is written back exactly (lost_at, lost_reason, agent_stopped_at) only while
// the revive's generation is still the newest; behind a newer generation it changes nothing.
func TestRunPolicyGeneration_RestoreLostFateOnlyAtTheNewestGeneration(t *testing.T) {
	for _, reason := range []types.LostReason{types.LostReboot, types.LostEnded, types.LostOutage} {
		t.Run(string(reason), func(t *testing.T) {
			pool := runsPGPool(t)
			ctx := context.Background()
			pg := store.NewPG(pool)
			r := newRun(types.RunRunning)
			persistRun(t, ctx, pool, r)
			lostAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
			if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at=$2, lost_reason=$3, agent_stopped_at=$4 WHERE id=$1`,
				r.ID, lostAt, string(reason), lostAt.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			before, err := pg.RunLostFate(ctx, r.ID)
			if err != nil || before.LostAt == nil || before.AgentStoppedAt == nil || before.LostReason != reason {
				t.Fatalf("fate before = %+v, %v; want lost (%s) with its agent stopped", before, err, reason)
			}
			// The claim, as markRunRevived writes it.
			if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at=NULL, lost_reason='', agent_stopped_at=NULL WHERE id=$1`, r.ID); err != nil {
				t.Fatal(err)
			}
			gen, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, store.PolicySlot{Generation: 1}, "d-a", nil, "jti-a", nil)
			if err != nil {
				t.Fatal(err)
			}
			if applied, err := pg.RestoreRunLostFateAtGeneration(ctx, r.ID, gen-1, before); err != nil || applied {
				t.Fatalf("restore at an older generation = %v, %v; want not applied", applied, err)
			}
			if got, _ := pg.RunLostFate(ctx, r.ID); got.LostAt != nil {
				t.Fatalf("after a stale restore: %+v, want not lost", got)
			}
			if applied, err := pg.RestoreRunLostFateAtGeneration(ctx, r.ID, gen, before); err != nil || !applied {
				t.Fatalf("restore at the newest generation = %v, %v; want applied", applied, err)
			}
			if after, err := pg.RunLostFate(ctx, r.ID); err != nil || !reflect.DeepEqual(after, before) {
				t.Errorf("fate after the restore = %+v, %v; want exactly %+v", after, err, before)
			}
		})
	}
}

// TestRunPolicyGeneration_AdvanceAcrossAnAbandonIsMoved: an abandon stores its source config back without
// moving the newest number, so a revive that read the slot before the abandon (and so the abandoned
// revive's config) is refused; one that read it after, abandoned, advances.
func TestRunPolicyGeneration_AdvanceAcrossAnAbandonIsMoved(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	r := newRun(types.RunRunning)
	persistRun(t, ctx, pool, r)
	if err := pg.PutRunProxyConfig(ctx, r.ID, []byte("sealed-1")); err != nil {
		t.Fatal(err)
	}
	gen, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, store.PolicySlot{Generation: 1}, "d-c", nil, "jti-c", []byte("sealed-c"))
	if err != nil {
		t.Fatal(err)
	}
	read := store.PolicySlot{Generation: gen} // A reads C's slot and C's config
	if err := pg.AbandonRunPolicyGeneration(ctx, r.ID, gen, []byte("sealed-1")); err != nil {
		t.Fatal(err)
	}
	before := snapshotPolicyRow(t, ctx, pg, r.ID)
	if _, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, read, "d-a", nil, "jti-a", []byte("sealed-a")); !errors.Is(err, store.ErrRunPolicyMoved) {
		t.Fatalf("advance from the slot read before the abandon = %v, want ErrRunPolicyMoved", err)
	}
	if after := snapshotPolicyRow(t, ctx, pg, r.ID); !reflect.DeepEqual(after, before) || after.sealed != "sealed-1" {
		t.Fatalf("a refused advance changed the row: %+v, was %+v", after, before)
	}
	if got, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, store.PolicySlot{Generation: gen, Abandoned: true}, "d-a", nil, "jti-a", []byte("sealed-a")); err != nil || got != gen+1 {
		t.Errorf("advance from the abandoned slot = %d, %v; want %d", got, err, gen+1)
	}
}

// loseRunPG marks a run lost, as a lost mark leaves the row.
func loseRunPG(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id uuid.UUID, reason types.LostReason) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at=now(), lost_reason=$2 WHERE id=$1`, id, string(reason)); err != nil {
		t.Fatal(err)
	}
}

// TestMarkRunRevived_StartsAgentKeepsTheOwedMarkUntilCleared: a claim that starts the agent leaves
// agent_stopped_at set on the live row, on the stamp it returned, as "agent start still owed"; the row
// reads live (it holds its slot); a clear on a stale generation, on another stamp or on a re-lost row
// leaves it, and a clear at the newest generation with the claim's own stamp ends it. A claim that starts
// no agent clears the mark of a lost row as before.
func TestMarkRunRevived_StartsAgentKeepsTheOwedMarkUntilCleared(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	r := newRun(types.RunRunning)
	persistRun(t, ctx, pool, r)
	mark := func() *time.Time {
		var m *time.Time
		if err := pool.QueryRow(ctx, `SELECT agent_stopped_at FROM agent_runs WHERE id=$1`, r.ID).Scan(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	owed := func(want bool, what string) {
		t.Helper()
		if got, err := pg.RunAgentStopped(ctx, r.ID); err != nil || got != want {
			t.Fatalf("%s: agent start owed = %v, %v; want %v", what, got, err, want)
		}
	}
	gen, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, store.PolicySlot{Generation: 1}, "d2", nil, "jti-2", nil)
	if err != nil {
		t.Fatal(err)
	}
	loseRunPG(t, ctx, pool, r.ID, types.LostReboot)
	stamp, claimed, err := pg.MarkRunRevivedOwingStart(ctx, r.ID, types.LostReboot, nil, 0, store.PolicySlot{Generation: gen})
	if err != nil || !claimed || stamp.IsZero() {
		t.Fatalf("claim that starts the agent = %v, %v, %v; want claimed with a stamp", stamp, claimed, err)
	}
	owed(true, "after the claim")
	if m := mark(); m == nil || !m.Equal(stamp) {
		t.Fatalf("agent_stopped_at = %v, want the claim's own stamp %v", m, stamp)
	}
	if n, err := pg.CountNonTerminalRuns(ctx, uuid.Nil); err != nil || n != 1 {
		t.Errorf("CountNonTerminalRuns = %d, %v; want 1: a run with its start owed reads live and holds its slot", n, err)
	}
	if run, err := pg.GetRun(ctx, r.ID); err != nil || run.LostAt != nil {
		t.Errorf("the run reads lost %v, %v; want live", run.LostAt, err)
	}
	if err := pg.ClearRunAgentStartOwed(ctx, r.ID, gen-1, stamp); err != nil {
		t.Fatal(err)
	}
	owed(true, "after a clear at a stale generation")
	if err := pg.ClearRunAgentStartOwed(ctx, r.ID, gen, stamp.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	owed(true, "after a clear on another stamp")
	loseRunPG(t, ctx, pool, r.ID, types.LostReboot)
	if err := pg.ClearRunAgentStartOwed(ctx, r.ID, gen, stamp); err != nil {
		t.Fatal(err)
	}
	owed(true, "after a clear on a re-lost row")
	if ok, err := pg.MarkRunRevived(ctx, r.ID, types.LostReboot, nil, 0, false); err != nil || !ok {
		t.Fatalf("claim that starts no agent on the lost row = %v, %v; want claimed", ok, err)
	}
	owed(false, "after a claim that starts no agent")

	loseRunPG(t, ctx, pool, r.ID, types.LostReboot)
	stamp, claimed, err = pg.MarkRunRevivedOwingStart(ctx, r.ID, types.LostReboot, nil, 0, store.PolicySlot{Generation: gen})
	if err != nil || !claimed {
		t.Fatalf("second claim = %v, %v", claimed, err)
	}
	if err := pg.ClearRunAgentStartOwed(ctx, r.ID, gen, stamp); err != nil {
		t.Fatal(err)
	}
	owed(false, "after a clear at the newest generation with the claim's stamp")
}

// TestMarkRunRevived_ALaterClaimAtTheSameGenerationKeepsItsOwnMark: A claims and its start lands; the
// run is lost again at the same generation; B claims and owes its own start. A's late clear carries A's
// stamp, so it leaves B's mark; B's clear ends it.
func TestMarkRunRevived_ALaterClaimAtTheSameGenerationKeepsItsOwnMark(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	r := newRun(types.RunRunning)
	persistRun(t, ctx, pool, r)
	gen, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, store.PolicySlot{Generation: 1}, "d2", nil, "jti-2", nil)
	if err != nil {
		t.Fatal(err)
	}
	owed := func(want bool, what string) {
		t.Helper()
		if got, err := pg.RunAgentStopped(ctx, r.ID); err != nil || got != want {
			t.Fatalf("%s: agent start owed = %v, %v; want %v", what, got, err, want)
		}
	}
	loseRunPG(t, ctx, pool, r.ID, types.LostReboot)
	t1, ok, err := pg.MarkRunRevivedOwingStart(ctx, r.ID, types.LostReboot, nil, 0, store.PolicySlot{Generation: gen})
	if err != nil || !ok {
		t.Fatalf("A's claim = %v, %v", ok, err)
	}
	if err := pg.ClearRunAgentStartOwed(ctx, r.ID, gen, t1); err != nil {
		t.Fatal(err)
	}
	owed(false, "after A's start landed")
	loseRunPG(t, ctx, pool, r.ID, types.LostReboot)
	t2, ok, err := pg.MarkRunRevivedOwingStart(ctx, r.ID, types.LostReboot, nil, 0, store.PolicySlot{Generation: gen})
	if err != nil || !ok || t2.Equal(t1) {
		t.Fatalf("B's claim = %v, %v, %v; want claimed on a stamp of its own (A's was %v)", t2, ok, err, t1)
	}
	if err := pg.ClearRunAgentStartOwed(ctx, r.ID, gen, t1); err != nil {
		t.Fatal(err)
	}
	owed(true, "after A's late clear")
	if err := pg.ClearRunAgentStartOwed(ctx, r.ID, gen, t2); err != nil {
		t.Fatal(err)
	}
	owed(false, "after B's clear")
}

// TestMarkRunRevived_AClaimThatStartsNoAgentNeverErasesAnOwedStart: a claim that starts no agent (a live
// restart or an egress edit that read the run before a start-owing claim landed) is refused on a live row
// whose start is owed, and leaves the mark; on a lost row it claims and clears the mark as before.
func TestMarkRunRevived_AClaimThatStartsNoAgentNeverErasesAnOwedStart(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	r := newRun(types.RunRunning)
	persistRun(t, ctx, pool, r)
	loseRunPG(t, ctx, pool, r.ID, types.LostReboot)
	stamp, ok, err := pg.MarkRunRevivedOwingStart(ctx, r.ID, types.LostReboot, nil, 0, store.PolicySlot{})
	if err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	if ok, err := pg.MarkRunRevived(ctx, r.ID, "", nil, 0, false); err != nil || ok {
		t.Fatalf("live claim that starts no agent on an owed start = %v, %v; want refused", ok, err)
	}
	if ok, err := pg.MarkRunRevivedAtSlot(ctx, r.ID, "", nil, 0, false, store.PolicySlot{Generation: 1}); err != nil || ok {
		t.Fatalf("slot-bound live claim that starts no agent on an owed start = %v, %v; want refused", ok, err)
	}
	if got, err := pg.RunAgentStopped(ctx, r.ID); err != nil || !got {
		t.Fatalf("after the refused claims: start owed = %v, %v; want kept", got, err)
	}
	if err := pg.ClearRunAgentStartOwed(ctx, r.ID, 0, stamp); err != nil {
		t.Fatal(err)
	}
	if ok, err := pg.MarkRunRevived(ctx, r.ID, "", nil, 0, false); err != nil || !ok {
		t.Fatalf("live claim that starts no agent once the start landed = %v, %v; want claimed", ok, err)
	}
}

// TestMarkRunRevivedAtSlot_ClaimsOnlyOnTheSlotItRead: the claim lands only while the run's newest policy
// slot (number and abandoned) is the one the revive read; a zero slot binds nothing. Each case runs for
// a claim that starts no agent and for one that does, the two SQL paths.
func TestMarkRunRevivedAtSlot_ClaimsOnlyOnTheSlotItRead(t *testing.T) {
	cases := []struct {
		name      string
		abandoned bool // the run's newest generation (2) is abandoned
		expected  store.PolicySlot
		want      bool
	}{
		{"an older generation", false, store.PolicySlot{Generation: 1}, false},
		{"the newest number, but not abandoned as read", true, store.PolicySlot{Generation: 2}, false},
		{"the newest slot, abandoned", true, store.PolicySlot{Generation: 2, Abandoned: true}, true},
		{"no slot read", false, store.PolicySlot{}, true},
	}
	for _, tc := range cases {
		for _, startsAgent := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: " (no agent)", true: " (agent)"}[startsAgent], func(t *testing.T) {
				pool := runsPGPool(t)
				ctx := context.Background()
				pg := store.NewPG(pool)
				r := newRun(types.RunRunning)
				persistRun(t, ctx, pool, r)
				if _, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, store.PolicySlot{Generation: 1}, "d2", nil, "jti-2", nil); err != nil {
					t.Fatal(err)
				}
				if tc.abandoned {
					if err := pg.AbandonRunPolicyGeneration(ctx, r.ID, 2, nil); err != nil {
						t.Fatal(err)
					}
				}
				loseRunPG(t, ctx, pool, r.ID, types.LostReboot)
				got, err := pg.MarkRunRevivedAtSlot(ctx, r.ID, types.LostReboot, nil, 0, startsAgent, tc.expected)
				if err != nil || got != tc.want {
					t.Fatalf("claim at %+v = %v, %v; want %v", tc.expected, got, err, tc.want)
				}
				if run, err := pg.GetRun(ctx, r.ID); err != nil || (run.LostAt == nil) != tc.want {
					t.Errorf("after the claim lost_at = %v, %v; want it cleared only when claimed (%v)", run.LostAt, err, tc.want)
				}
			})
		}
	}
}
