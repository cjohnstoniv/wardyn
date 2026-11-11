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
	if gen, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, 1, "d2", []string{"jti-1", ""}, "jti-2", nil); err != nil || gen != 2 {
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
	if gen, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, 2, "d3", nil, "jti-3", nil); err != nil || gen != 3 {
		t.Errorf("the next advance = %d, %v; want 3", gen, err)
	}
	if _, err := pg.AdvanceRunPolicyGeneration(ctx, uuid.New(), 1, "d", nil, "j", nil); !errors.Is(err, store.ErrNotFound) {
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
	if gen, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, 1, "d2", nil, "jti-2", []byte("sealed-2")); err != nil || gen != 2 {
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
		if _, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, expected, "d3", []string{"jti-1"}, "jti-3", []byte("sealed-3")); !errors.Is(err, store.ErrRunPolicyMoved) {
			t.Fatalf("advance expecting %d = %v, want ErrRunPolicyMoved", expected, err)
		}
		if after := snapshotPolicyRow(t, ctx, pg, r.ID); !reflect.DeepEqual(after, before) {
			t.Fatalf("a refused advance expecting %d changed the row: %+v, was %+v", expected, after, before)
		}
	}
	if _, err := pg.MarkRunLost(ctx, r.ID, types.LostOutage, time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, 2, "d3", nil, "jti-3", []byte("sealed-3")); !errors.Is(err, store.ErrRunPolicyMoved) {
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
	if _, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, 1, "d-a", nil, "jti-a", []byte("sealed-a")); err != nil {
		t.Fatal(err)
	}
	if gen, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, 2, "d-b", nil, "jti-b", []byte("sealed-b")); err != nil || gen != 3 {
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
	if _, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, 1, "d2", nil, "jti-2", nil); !errors.Is(err, store.ErrRunPolicyMoved) {
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
	if _, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, 1, "d2", nil, "jti-2", []byte("sealed-2")); err != nil {
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
	if _, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, 1, "d2", nil, "jti-2", []byte("fail-write")); err == nil {
		t.Fatal("an advance whose config write fails must fail")
	}
	if after := snapshotPolicyRow(t, ctx, pg, r.ID); !reflect.DeepEqual(after, before) {
		t.Fatalf("a failed advance changed the row: %+v, was %+v", after, before)
	}
	if _, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, 1, "d2", nil, "jti-2", []byte("sealed-2")); err != nil {
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
