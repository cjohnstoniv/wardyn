// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests for agent_runs.local_folder and its seed (migration
// 0195). Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_RunLocalFolder_SeedIsRecordedOnce(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	r := newRun(types.RunRunning)
	r.LocalFolder = &types.RunLocalFolder{Name: "my-app", RemoteDir: "/home/agent/work"}
	if _, err := pg.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	plain := newRun(types.RunRunning)
	if _, err := pg.CreateRun(ctx, plain); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id IN ($1, $2)`, r.ID, plain.ID)
	})

	folder, seed, err := pg.RunLocalFolderSeed(ctx, r.ID)
	if err != nil || folder == nil || *folder != *r.LocalFolder || seed.SeededAt != nil || seed.Error != "" {
		t.Fatalf("read back %+v %+v %v", folder, seed, err)
	}
	if ok, err := pg.SetRunLocalFolderSeed(ctx, r.ID, ""); !ok || err != nil {
		t.Fatalf("first seed: %v %v", ok, err)
	}
	if ok, err := pg.SetRunLocalFolderSeed(ctx, r.ID, "late failure"); ok || err != nil {
		t.Errorf("a second outcome was recorded: %v %v", ok, err)
	}
	if _, seed, _ := pg.RunLocalFolderSeed(ctx, r.ID); seed.SeededAt == nil || seed.Error != "" {
		t.Errorf("seed = %+v, want the first outcome only", seed)
	}

	if folder, _, err := pg.RunLocalFolderSeed(ctx, plain.ID); folder != nil || err != nil {
		t.Errorf("a run with no local folder read back %+v %v", folder, err)
	}
	if ok, _ := pg.SetRunLocalFolderSeed(ctx, plain.ID, ""); ok {
		t.Error("a run with no local folder took a seed")
	}
}

func TestPG_RunLocalFolder_AComputerFolderKeepsItsRunnerAndItsPasses(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	runner := uuid.New()
	r := newRun(types.RunRunning)
	r.LocalFolder = &types.RunLocalFolder{Name: "my-app", RemoteDir: "/home/agent/work", RunnerID: &runner, Path: "/Users/alice/code/my-app", Direction: types.ComputerFolderBoth}
	if _, err := pg.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	plain := newRun(types.RunRunning)
	if _, err := pg.CreateRun(ctx, plain); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id IN ($1, $2)`, r.ID, plain.ID)
	})

	folder, _, err := pg.RunLocalFolderSeed(ctx, r.ID)
	if err != nil || folder == nil || folder.RunnerID == nil || *folder.RunnerID != runner || folder.Path != r.LocalFolder.Path || folder.Direction != types.ComputerFolderBoth {
		t.Fatalf("read back %+v %v", folder, err)
	}
	if st, err := pg.RunLocalFolderStatus(ctx, r.ID); err != nil || st.Seed != nil || st.Back != nil {
		t.Fatalf("a fresh run's passes = %+v %v, want none", st, err)
	}
	at := time.Now().UTC().Truncate(time.Millisecond)
	want := types.RunLocalFolderStatus{
		Seed: &types.FolderPassRecord{At: at, Files: 2, Bytes: 18, Refused: []types.FolderRefusal{{Path: "link", Reason: "symlink"}}},
		Back: &types.FolderPassRecord{At: at, Files: 1, Bytes: 5, Refused: []types.FolderRefusal{}, Error: ""},
	}
	if ok, err := pg.SetRunLocalFolderStatusIf(ctx, r.ID, store.FolderStatusGuard{}, want); !ok || err != nil {
		t.Fatalf("set: %v %v", ok, err)
	}
	got, err := pg.RunLocalFolderStatus(ctx, r.ID)
	if err != nil || got.Seed == nil || got.Back == nil || !got.Seed.At.Equal(at) || got.Seed.Files != 2 || len(got.Seed.Refused) != 1 || got.Back.Files != 1 {
		t.Fatalf("read back %+v %+v %v", got.Seed, got.Back, err)
	}
	if ok, _ := pg.SetRunLocalFolderStatusIf(ctx, plain.ID, store.FolderStatusGuard{}, want); ok {
		t.Error("a run with no local folder took a pass record")
	}
}

// A record's generation is 0 when the column is NULL or its JSON has no gen: the bump moves either to 1 and
// keeps every other field, a write at 0 lands on a gen-less record, and a write at a stale generation does not.
func TestPG_FolderGen_NullAndLegacyRows(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	runner := uuid.New()
	newFolderRun := func() uuid.UUID {
		r := newRun(types.RunRunning)
		r.LocalFolder = &types.RunLocalFolder{Name: "my-app", RemoteDir: "/home/agent/work", RunnerID: &runner, Path: "/Users/alice/code/my-app", Direction: types.ComputerFolderBoth}
		if _, err := pg.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id = $1`, r.ID) })
		return r.ID
	}

	null := newFolderRun()
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET local_folder_status = NULL WHERE id = $1`, null); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	if st, ok, err := pg.BumpRunLocalFolderGen(ctx, null, store.FolderStatusGuard{Gen: 0}, at); !ok || err != nil || st.Gen != 1 || !st.GenAt.Equal(at) || st.Back != nil {
		t.Fatalf("bump of a NULL record = %+v %v %v, want gen 1 stamped %v and nothing else", st, ok, err, at)
	}

	legacy := newFolderRun()
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET local_folder_status = jsonb_build_object('back', jsonb_build_object('at', now(), 'files', 4, 'bytes', 9, 'refused', '[]'::jsonb)) WHERE id = $1`, legacy); err != nil {
		t.Fatal(err)
	}
	if st, err := pg.RunLocalFolderStatus(ctx, legacy); err != nil || st.Gen != 0 || st.Back == nil {
		t.Fatalf("a gen-less record reads %+v %v, want gen 0 with its back", st, err)
	}
	if ok, err := pg.SetRunLocalFolderStatusIf(ctx, legacy, store.FolderStatusGuard{Gen: 1}, types.RunLocalFolderStatus{}); ok || err != nil {
		t.Fatalf("a write at gen 1 on a gen-less record landed: %v %v", ok, err)
	}
	st, ok, err := pg.BumpRunLocalFolderGen(ctx, legacy, store.FolderStatusGuard{Gen: 0}, at)
	if !ok || err != nil || st.Gen != 1 || st.Back == nil || st.Back.Files != 4 {
		t.Fatalf("bump of a gen-less record = %+v %v %v, want gen 1 with its back kept", st, ok, err)
	}
	if _, ok, _ := pg.BumpRunLocalFolderGen(ctx, legacy, store.FolderStatusGuard{Gen: 0}, at); ok {
		t.Error("a second bump from gen 0 landed")
	}
	st.Settled = 1
	if ok, err := pg.SetRunLocalFolderStatusIf(ctx, legacy, store.FolderStatusGuard{Gen: 1}, st); !ok || err != nil {
		t.Fatalf("a write at the owned gen: %v %v", ok, err)
	}
	if got, err := pg.RunLocalFolderStatus(ctx, legacy); err != nil || got.Gen != 1 || got.Settled != 1 || got.Back == nil {
		t.Errorf("read back %+v %v, want gen 1, settled 1, back kept", got, err)
	}

	gen0 := newFolderRun()
	if ok, err := pg.SetRunLocalFolderStatusIf(ctx, gen0, store.FolderStatusGuard{Gen: 0}, types.RunLocalFolderStatus{Seed: &types.FolderPassRecord{Files: 1, Refused: []types.FolderRefusal{}}}); !ok || err != nil {
		t.Fatalf("a write at gen 0 on a fresh run: %v %v", ok, err)
	}
}

// giveUpRunOpts is a run as the give-up statement reads it. The *Ago fields are seconds before the database's
// now(), nil for NULL.
type giveUpRunOpts struct {
	state       types.RunState
	endedAgo    *float64
	updatedAgo  float64
	createdAgo  float64
	lifetimeAgo *float64
	errorAgo    *float64
	ref         string
	folder      bool
	status      *types.RunLocalFolderStatus
}

func secs(d time.Duration) *float64 { v := d.Seconds(); return &v }

// newGiveUpRun stores a run as o says: with a sandbox "sb-1" and a both-ways folder unless o says otherwise.
func newGiveUpRun(t *testing.T, pool *pgxpool.Pool, o giveUpRunOpts) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	runner := uuid.New()
	r := newRun(types.RunRunning)
	if o.folder {
		r.LocalFolder = &types.RunLocalFolder{Name: "my-app", RemoteDir: "/home/agent/work", RunnerID: &runner, Path: "/Users/alice/code/my-app", Direction: types.ComputerFolderBoth}
	}
	if _, err := store.NewPG(pool).CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id = $1`, r.ID) })
	var status []byte
	if o.status != nil {
		var err error
		if status, err = json.Marshal(o.status); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET state = $2,
		ended_at = CASE WHEN $3::double precision IS NULL THEN NULL ELSE now() - make_interval(secs => $3) END,
		updated_at = now() - make_interval(secs => $4),
		created_at = now() - make_interval(secs => $5),
		lifetime_cap_at = CASE WHEN $6::double precision IS NULL THEN NULL ELSE now() - make_interval(secs => $6) END,
		containment_error_at = CASE WHEN $7::double precision IS NULL THEN NULL ELSE now() - make_interval(secs => $7) END,
		sandbox_ref = $8, local_folder_status = $9::jsonb
		WHERE id = $1`, r.ID, string(o.state), o.endedAgo, o.updatedAgo, o.createdAgo, o.lifetimeAgo, o.errorAgo, o.ref, status); err != nil {
		t.Fatal(err)
	}
	return r.ID
}

// ownedRecord is a record an owner holds at gen: a Resume of three counted changes.
func ownedRecord(gen int) *types.RunLocalFolderStatus {
	return &types.RunLocalFolderStatus{
		Gen:    gen,
		Resume: &types.FolderResume{Stalled: 1, Unsynced: 3, Counted: true, Paths: []string{"a.txt", "b/c.txt"}},
		Back:   &types.FolderPassRecord{At: time.Now().UTC(), Files: 1, Refused: []types.FolderRefusal{}, Error: "cut short"},
	}
}

func givenUpBack(n int) types.FolderPassRecord {
	at := time.Now().UTC().Truncate(time.Microsecond)
	return types.FolderPassRecord{At: at, Refused: []types.FolderRefusal{}, Error: "given up", GivenUpAt: &at, GivenUpCount: &n, GivenUpPaths: []string{"a.txt"}}
}

const (
	giveUpCap = 7 * 24 * time.Hour
	giveUpTau = 30 * time.Second
)

// endedLongAgo is a stopped run past the keep, holding its sandbox with an owner at gen 7.
func endedLongAgo() giveUpRunOpts {
	return giveUpRunOpts{state: types.RunStopped, endedAgo: secs(giveUpCap + time.Hour), ref: "sb-1", folder: true, status: ownedRecord(7)}
}

// The give-up moves the generation on and drops the Resume in one statement, so the owner that held the old
// generation can neither settle nor resume.
func TestPG_GiveUpAdvancesGenAndFencesALiveOwner(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	id := newGiveUpRun(t, pool, endedLongAgo())

	out, err := pg.GiveUpRunLocalFolder(ctx, id, store.GiveUpParams{Back: givenUpBack(3), Cap: giveUpCap})
	if err != nil || !out.Applied || out.Gen != 8 {
		t.Fatalf("give-up = %+v %v, want applied at gen 8", out, err)
	}
	ownerA := store.FolderStatusGuard{Gen: 7}
	if ok, err := pg.SetRunLocalFolderStatusIf(ctx, id, ownerA, types.RunLocalFolderStatus{Settled: 7, Back: &types.FolderPassRecord{At: time.Now(), Refused: []types.FolderRefusal{}}}); ok || err != nil {
		t.Errorf("the old owner's settle landed: %v %v", ok, err)
	}
	if ok, err := pg.SetRunLocalFolderStatusIf(ctx, id, ownerA, *ownedRecord(7)); ok || err != nil {
		t.Errorf("the old owner's Resume landed: %v %v", ok, err)
	}
	if _, ok, err := pg.BumpRunLocalFolderGen(ctx, id, ownerA, time.Now()); ok || err != nil {
		t.Errorf("the old owner's bump landed: %v %v", ok, err)
	}
	got, err := pg.RunLocalFolderStatus(ctx, id)
	if err != nil || got.Gen != 8 || got.Resume != nil || got.Back == nil || got.Back.GivenUpAt == nil || got.Back.GivenUpCount == nil || *got.Back.GivenUpCount != 3 {
		t.Fatalf("record = %+v %v, want gen 8, no Resume, the given-up Back", got, err)
	}
}

// Nothing is given up before its due: a run one second short of the cap, a run still live, and a D1 caller
// before the containment due or with no failed cut. The same run gives up once each condition holds.
func TestPG_GiveUpNeverFiresEarly(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	const grace = time.Hour
	d1 := func(unsupported bool) store.GiveUpParams {
		return store.GiveUpParams{Back: givenUpBack(3), Cap: giveUpCap, D1: true, Lag: store.ContainmentLag, Unsupported: unsupported, Tau: giveUpTau}
	}
	ended := func(ago time.Duration, mutate func(*giveUpRunOpts)) giveUpRunOpts {
		o := endedLongAgo()
		o.endedAgo = secs(ago)
		if mutate != nil {
			mutate(&o)
		}
		return o
	}
	for name, c := range map[string]struct {
		o       giveUpRunOpts
		p       store.GiveUpParams
		applied bool
	}{
		"cap minus a second":            {ended(grace-time.Second, nil), store.GiveUpParams{Back: givenUpBack(3), Cap: grace}, false},
		"cap plus a second":             {ended(grace+time.Second, nil), store.GiveUpParams{Back: givenUpBack(3), Cap: grace}, true},
		"a running run":                 {ended(grace+time.Hour, func(o *giveUpRunOpts) { o.state = types.RunRunning }), store.GiveUpParams{Back: givenUpBack(3), Cap: grace}, false},
		"no sandbox":                    {ended(grace+time.Hour, func(o *giveUpRunOpts) { o.ref = "" }), store.GiveUpParams{Back: givenUpBack(3), Cap: grace}, false},
		"no folder":                     {ended(grace+time.Hour, func(o *giveUpRunOpts) { o.folder = false }), store.GiveUpParams{Back: givenUpBack(3), Cap: grace}, false},
		"d1 before the containment due": {ended(store.ContainmentLag-5*time.Second, nil), d1(true), false},
		"d1 unsupported past the due":   {ended(store.ContainmentLag+5*time.Second, nil), d1(true), true},
		"d1 with no failed cut":         {ended(store.ContainmentLag+5*time.Second, nil), d1(false), false},
		"d1 failure younger than tau":   {ended(store.ContainmentLag+5*time.Second, func(o *giveUpRunOpts) { o.errorAgo = secs(giveUpTau / 2) }), d1(false), false},
		"d1 failure older than tau":     {ended(store.ContainmentLag+5*time.Second, func(o *giveUpRunOpts) { o.errorAgo = secs(giveUpTau * 2) }), d1(false), true},
		"not d1 past the lag":           {ended(store.ContainmentLag+5*time.Second, func(o *giveUpRunOpts) { o.errorAgo = secs(giveUpTau * 2) }), store.GiveUpParams{Back: givenUpBack(3), Cap: giveUpCap, Lag: store.ContainmentLag, Unsupported: true}, false},
	} {
		id := newGiveUpRun(t, pool, c.o)
		out, err := pg.GiveUpRunLocalFolder(ctx, id, c.p)
		if err != nil || out.Applied != c.applied {
			t.Errorf("%s: give-up = %+v %v, want applied=%v", name, out, err, c.applied)
		}
		if !c.applied {
			if st, _ := pg.RunLocalFolderStatus(ctx, id); st.Gen != 7 || st.Resume == nil {
				t.Errorf("%s: a refused give-up changed the record: %+v", name, st)
			}
		}
	}
}

// A run's lifetime cap bounds a live run: one that ended before its keep is up is not given up because its cap has passed.
func TestPG_GiveUpIgnoresLifetimeCap(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	o := endedLongAgo()
	o.createdAgo, o.lifetimeAgo = (10 * time.Hour).Seconds(), secs(9*time.Hour)
	o.endedAgo = secs(90 * time.Minute)
	id := newGiveUpRun(t, pool, o)
	if out, err := pg.GiveUpRunLocalFolder(ctx, id, store.GiveUpParams{Back: givenUpBack(3), Cap: 2 * time.Hour}); err != nil || out.Applied {
		t.Fatalf("give-up 90 min after the end under a 2 h keep = %+v %v, want not applied though the lifetime cap passed hours ago", out, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET ended_at = now() - interval '121 minutes' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if out, err := pg.GiveUpRunLocalFolder(ctx, id, store.GiveUpParams{Back: givenUpBack(3), Cap: 2 * time.Hour}); err != nil || !out.Applied {
		t.Fatalf("give-up past the keep = %+v %v, want applied", out, err)
	}
}

// Two replicas giving up the same run at once: one statement applies.
func TestPG_GiveUpOnceAcrossReplicas(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	id := newGiveUpRun(t, pool, endedLongAgo())
	var wg sync.WaitGroup
	var mu sync.Mutex
	applied, gens := 0, []int{}
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := pg.GiveUpRunLocalFolder(ctx, id, store.GiveUpParams{Back: givenUpBack(3), Cap: giveUpCap})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("give-up: %v", err)
			}
			if out.Applied {
				applied++
				gens = append(gens, out.Gen)
			}
		}()
	}
	wg.Wait()
	if applied != 1 || len(gens) != 1 || gens[0] != 8 {
		t.Fatalf("applied %d times at gens %v, want once at 8", applied, gens)
	}
	if st, _ := pg.RunLocalFolderStatus(ctx, id); st.Gen != 8 {
		t.Errorf("record gen = %d, want 8 (one move)", st.Gen)
	}
}

// A give-up that applies to nothing says so, and writes nothing: a record already final, an unknown run.
func TestPG_GiveUpReportsNotApplied(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	p := store.GiveUpParams{Back: givenUpBack(9), Cap: giveUpCap}

	done := endedLongAgo()
	done.status = &types.RunLocalFolderStatus{Gen: 4, Settled: 4, Back: &types.FolderPassRecord{At: time.Now().UTC(), Files: 2, Refused: []types.FolderRefusal{}}}
	id := newGiveUpRun(t, pool, done)
	if out, err := pg.GiveUpRunLocalFolder(ctx, id, p); err != nil || out.Applied || out.Gen != 0 {
		t.Errorf("give-up of a record already final = %+v %v, want the zero outcome", out, err)
	}
	if st, _ := pg.RunLocalFolderStatus(ctx, id); st.Gen != 4 || st.Back == nil || st.Back.GivenUpAt != nil || st.Back.Files != 2 {
		t.Errorf("a record already final was changed: %+v", st)
	}

	given := endedLongAgo()
	given.status = &types.RunLocalFolderStatus{Gen: 8, Back: &types.FolderPassRecord{At: time.Now().UTC(), Refused: []types.FolderRefusal{}, GivenUpAt: &done.status.Back.At}}
	id = newGiveUpRun(t, pool, given)
	if out, err := pg.GiveUpRunLocalFolder(ctx, id, p); err != nil || out.Applied {
		t.Errorf("a second give-up = %+v %v, want not applied", out, err)
	}

	if out, err := pg.GiveUpRunLocalFolder(ctx, uuid.New(), p); err != nil || out.Applied {
		t.Errorf("give-up of an unknown run = %+v %v, want not applied and no error", out, err)
	}

	never := endedLongAgo()
	never.status = nil
	id = newGiveUpRun(t, pool, never)
	if out, err := pg.GiveUpRunLocalFolder(ctx, id, p); err != nil || !out.Applied || out.Gen != 1 {
		t.Errorf("give-up of a run with no record = %+v %v, want applied at gen 1", out, err)
	}
}

// The statement's D1 due is the containment due: COALESCE(ended_at, updated_at) + ContainmentLag, the arithmetic of
// ContainmentDueSQL, over its value table.
func TestPG_GiveUpDueMatchesContainmentDue(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	for name, c := range map[string]struct {
		endedAgo, updatedAgo *float64
	}{
		"ended before the due":             {endedAgo: secs(store.ContainmentLag + 5*time.Second)},
		"ended inside the lag":             {endedAgo: secs(store.ContainmentLag - 5*time.Second)},
		"no end, updated before the due":   {updatedAgo: secs(store.ContainmentLag + 5*time.Second)},
		"no end, updated inside the lag":   {updatedAgo: secs(store.ContainmentLag - 5*time.Second)},
		"ended inside, updated long ago":   {endedAgo: secs(store.ContainmentLag - 5*time.Second), updatedAgo: secs(24 * time.Hour)},
		"ended long ago, updated just now": {endedAgo: secs(24 * time.Hour), updatedAgo: secs(0)},
	} {
		o := endedLongAgo()
		o.endedAgo = c.endedAgo
		if c.updatedAgo != nil {
			o.updatedAgo = *c.updatedAgo
		}
		id := newGiveUpRun(t, pool, o)
		var dueSQL bool
		if err := pool.QueryRow(ctx, `SELECT `+store.ContainmentDueSQL+` <= now() FROM agent_runs WHERE id = $1`, id).Scan(&dueSQL); err != nil {
			t.Fatal(err)
		}
		out, err := pg.GiveUpRunLocalFolder(ctx, id, store.GiveUpParams{Back: givenUpBack(3), Cap: giveUpCap, D1: true, Lag: store.ContainmentLag, Unsupported: true})
		if err != nil || out.Applied != dueSQL {
			t.Errorf("%s: the statement applied=%v %v, ContainmentDueSQL says due=%v", name, out.Applied, err, dueSQL)
		}
	}
}

// MarkGivenUpAudited writes at the record's generation only, and changes only that field.
func TestPG_MarkGivenUpAuditedAtGenOnly(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	id := newGiveUpRun(t, pool, endedLongAgo())
	out, err := pg.GiveUpRunLocalFolder(ctx, id, store.GiveUpParams{Back: givenUpBack(3), Cap: giveUpCap})
	if err != nil || !out.Applied {
		t.Fatalf("give-up: %+v %v", out, err)
	}
	if ok, err := pg.MarkGivenUpAudited(ctx, id, out.Gen-1); ok || err != nil {
		t.Errorf("a mark at the old generation landed: %v %v", ok, err)
	}
	if st, _ := pg.RunLocalFolderStatus(ctx, id); st.Back.GivenUpAudited {
		t.Error("the record is marked after a mark at the wrong generation")
	}
	if ok, err := pg.MarkGivenUpAudited(ctx, id, out.Gen); !ok || err != nil {
		t.Fatalf("the mark at the record's generation: %v %v", ok, err)
	}
	st, err := pg.RunLocalFolderStatus(ctx, id)
	if err != nil || st.Gen != out.Gen || !st.Back.GivenUpAudited || st.Back.GivenUpAt == nil || st.Back.GivenUpCount == nil || *st.Back.GivenUpCount != 3 || len(st.Back.GivenUpPaths) != 1 || st.Back.Error != "given up" {
		t.Errorf("record after the mark = %+v %v, want gen %d, audited, every other field kept", st.Back, err, out.Gen)
	}

	plain := endedLongAgo()
	plain.status = &types.RunLocalFolderStatus{Gen: 2, Back: &types.FolderPassRecord{At: time.Now().UTC(), Refused: []types.FolderRefusal{}}}
	other := newGiveUpRun(t, pool, plain)
	if ok, err := pg.MarkGivenUpAudited(ctx, other, 2); ok || err != nil {
		t.Errorf("a record that was not given up took a mark: %v %v", ok, err)
	}
}

// ListGivenUpUnaudited lists terminal given-up records without an accepted audit row, by id, with a cursor that wraps.
func TestPG_ListGivenUpUnaudited(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	given := func(audited *bool) *types.RunLocalFolderStatus {
		back := givenUpBack(2)
		if audited != nil {
			back.GivenUpAudited = *audited
		}
		return &types.RunLocalFolderStatus{Gen: 8, Back: &back}
	}
	yes, no := true, false
	want := map[uuid.UUID]bool{}
	for _, c := range []struct {
		name   string
		o      func(*giveUpRunOpts)
		listed bool
	}{
		{"given up, flag absent, with a ref", func(o *giveUpRunOpts) { o.status = given(nil) }, true},
		{"given up, flag false, no ref", func(o *giveUpRunOpts) { o.status, o.ref = given(&no), "" }, true},
		{"audited", func(o *giveUpRunOpts) { o.status = given(&yes) }, false},
		{"not given up", func(o *giveUpRunOpts) { o.status = ownedRecord(3) }, false},
		{"not terminal", func(o *giveUpRunOpts) { o.status, o.state = given(nil), types.RunRunning }, false},
	} {
		o := endedLongAgo()
		c.o(&o)
		id := newGiveUpRun(t, pool, o)
		want[id] = c.listed
	}
	var foreign int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_runs WHERE (local_folder_status->'back'->>'given_up_at') IS NOT NULL
		AND COALESCE((local_folder_status->'back'->>'given_up_audited')::boolean, false) = false AND state = ANY($1) AND NOT (id::text = ANY($2))`,
		terminalNames, idStrings(idsWhere(want, true))).Scan(&foreign); err != nil {
		t.Fatal(err)
	}
	if foreign > 0 {
		t.Skipf("%d given-up records of other tests are in the table; the page boundaries are not exact", foreign)
	}
	ids, cur, err := pg.ListGivenUpUnaudited(ctx, nil, 50)
	if err != nil || cur != nil {
		t.Fatalf("list = %v %v %v", ids, cur, err)
	}
	for _, id := range ids {
		if listed, mine := want[id]; mine && !listed {
			t.Errorf("run %s listed, want it left out", id)
		}
	}
	got := map[uuid.UUID]bool{}
	for _, id := range ids {
		got[id] = true
	}
	for id, listed := range want {
		if listed && !got[id] {
			t.Errorf("run %s not listed", id)
		}
	}

	// 51 rows in all, 50 a page: the 50 lowest ids with a cursor, then the last with none, then the first page again.
	for len(idsWhere(want, true)) < 51 {
		o := endedLongAgo()
		o.status = given(nil)
		want[newGiveUpRun(t, pool, o)] = true
	}
	all := idsWhere(want, true)
	sortUUIDs(all)
	p1, c1, err := pg.ListGivenUpUnaudited(ctx, nil, 50)
	if err != nil || len(p1) != 50 || c1 == nil || *c1 != all[49] || p1[0] != all[0] || p1[49] != all[49] {
		t.Fatalf("page 1 = %d rows, cursor %v, %v; want the 50 lowest ids and the 50th as the cursor", len(p1), c1, err)
	}
	p2, c2, err := pg.ListGivenUpUnaudited(ctx, c1, 50)
	if err != nil || len(p2) != 1 || p2[0] != all[50] || c2 != nil {
		t.Fatalf("page 2 = %v, cursor %v, %v; want only the 51st id and no cursor (the cursor row is not listed twice)", p2, c2, err)
	}
	p3, c3, err := pg.ListGivenUpUnaudited(ctx, c2, 50)
	if err != nil || len(p3) != 50 || c3 == nil || p3[0] != all[0] {
		t.Fatalf("page 3 (after nil) = %d rows from %v, %v; want page 1 again", len(p3), c3, err)
	}
}

var terminalNames = []string{"STOPPED", "COMPLETED", "FAILED", "KILLED", "ARCHIVED"}

// idsWhere is the ids of m whose value is v.
func idsWhere(m map[uuid.UUID]bool, v bool) []uuid.UUID {
	var out []uuid.UUID
	for id, b := range m {
		if b == v {
			out = append(out, id)
		}
	}
	return out
}

func idStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

// sortUUIDs orders ids as Postgres orders uuids: by bytes.
func sortUUIDs(ids []uuid.UUID) {
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
}
