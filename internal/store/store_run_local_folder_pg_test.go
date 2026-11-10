// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests for agent_runs.local_folder and its seed (migration
// 0195). Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"encoding/json"
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
	if st, ok, err := pg.BumpRunLocalFolderGen(ctx, null, store.FolderStatusGuard{Gen: 0}); !ok || err != nil || st.Gen != 1 || st.Back != nil {
		t.Fatalf("bump of a NULL record = %+v %v %v, want gen 1 and nothing else", st, ok, err)
	}

	legacy := newFolderRun()
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET local_folder_status = '{"back":{"at":"2026-11-01T00:00:00Z","files":4,"bytes":9,"refused":[]}}'::jsonb WHERE id = $1`, legacy); err != nil {
		t.Fatal(err)
	}
	if st, err := pg.RunLocalFolderStatus(ctx, legacy); err != nil || st.Gen != 0 || st.Back == nil {
		t.Fatalf("a gen-less record reads %+v %v, want gen 0 with its back", st, err)
	}
	if ok, err := pg.SetRunLocalFolderStatusIf(ctx, legacy, store.FolderStatusGuard{Gen: 1}, types.RunLocalFolderStatus{}); ok || err != nil {
		t.Fatalf("a write at gen 1 on a gen-less record landed: %v %v", ok, err)
	}
	st, ok, err := pg.BumpRunLocalFolderGen(ctx, legacy, store.FolderStatusGuard{Gen: 0})
	if !ok || err != nil || st.Gen != 1 || st.Back == nil || st.Back.Files != 4 {
		t.Fatalf("bump of a gen-less record = %+v %v %v, want gen 1 with its back kept", st, ok, err)
	}
	if _, ok, _ := pg.BumpRunLocalFolderGen(ctx, legacy, store.FolderStatusGuard{Gen: 0}); ok {
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

// giveUpRun is a run in state with a sandbox and a computer folder, its record status ("" for NULL), and its
// end ended ago on the database's clock.
func giveUpRun(t *testing.T, pool *pgxpool.Pool, state types.RunState, status string, ended time.Duration) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	runner := uuid.New()
	r := newRun(state)
	r.LocalFolder = &types.RunLocalFolder{Name: "my-app", RemoteDir: "/home/agent/work", RunnerID: &runner, Path: "/Users/alice/code/my-app", Direction: types.ComputerFolderBoth}
	if _, err := store.NewPG(pool).CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id = $1`, r.ID) })
	var raw any
	if status != "" {
		raw = status
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET sandbox_ref = 'sbx', local_folder_status = $2::jsonb,
		ended_at = now() - make_interval(secs => $3), updated_at = now() - make_interval(secs => $3) WHERE id = $1`,
		r.ID, raw, ended.Seconds()); err != nil {
		t.Fatal(err)
	}
	return r.ID
}

// giveUpRows counts run id's given-up audit rows.
func giveUpRows(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE run_id=$1 AND action='run.folder.transfer'`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

var giveUpEvent = types.AuditEvent{ActorType: types.ActorSystem, Actor: "wardynd", Action: "run.folder.transfer", Target: "sbx", Outcome: "failure",
	Data: json.RawMessage(`{"given_up":true}`)}

// A give-up lands at any generation, once, with one audit row: it records back with its time, drops resume
// and moves the generation on, so the owner that was resuming the pass cannot settle it afterwards (here a
// D1 give-up while a later resume passes); a second give-up writes nothing, and a NULL or gen-less record is
// given up like any other.
func TestPG_FolderGiveUp_EndsAResumeOnceAndFencesItsOwner(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	givenUp := types.FolderPassRecord{Error: "given up", Refused: []types.FolderRefusal{}}
	d1 := store.FolderGiveUpWhen{Cap: 24 * time.Hour, D1: true, Unsupported: true}

	id := giveUpRun(t, pool, types.RunStopped, `{"gen":2,"settled":2,"resume":{"stalled":0,"unsynced":3}}`, 2*time.Hour)
	owned, ok, err := pg.BumpRunLocalFolderGen(ctx, id, store.FolderStatusGuard{Gen: 2})
	if !ok || err != nil || owned.Gen != 3 {
		t.Fatalf("bump = %+v %v %v", owned, ok, err)
	}
	gen, ok, err := pg.GiveUpRunLocalFolder(ctx, id, d1, givenUp, giveUpEvent, prepareSpoolTest)
	if !ok || err != nil || gen != 4 {
		t.Fatalf("give-up = %d %v %v, want gen 4", gen, ok, err)
	}
	owned.Back, owned.Resume, owned.Settled = &types.FolderPassRecord{Files: 3, Refused: []types.FolderRefusal{}}, nil, 3
	if ok, err := pg.SetRunLocalFolderStatusIf(ctx, id, store.FolderStatusGuard{Gen: 3}, owned); ok || err != nil {
		t.Fatalf("the old owner's settle landed after the give-up: %v %v", ok, err)
	}
	got, err := pg.RunLocalFolderStatus(ctx, id)
	if err != nil || got.Gen != 4 || got.Resume != nil || got.Back == nil || got.Back.Error != "given up" || got.Back.GivenUpAt == nil || got.Settled != 2 {
		t.Fatalf("read back %+v %v, want gen 4, the given-up back with its time, no resume, settled kept", got, err)
	}
	if _, ok, err := pg.GiveUpRunLocalFolder(ctx, id, d1, givenUp, giveUpEvent, prepareSpoolTest); ok || err != nil {
		t.Errorf("a second give-up landed: %v %v", ok, err)
	}
	if n := giveUpRows(t, pool, id); n != 1 {
		t.Errorf("%d given-up rows after two give-ups, want 1", n)
	}

	past := store.FolderGiveUpWhen{Cap: time.Hour}
	for name, status := range map[string]string{"null": "", "gen-less": `{"seed":{"at":"2026-11-01T00:00:00Z","files":1,"bytes":1,"refused":[]}}`} {
		id := giveUpRun(t, pool, types.RunStopped, status, 2*time.Hour)
		if gen, ok, err := pg.GiveUpRunLocalFolder(ctx, id, past, givenUp, giveUpEvent, prepareSpoolTest); !ok || err != nil || gen != 1 {
			t.Errorf("%s: give-up = %d %v %v, want gen 1", name, gen, ok, err)
		}
		if got, err := pg.RunLocalFolderStatus(ctx, id); err != nil || got.Back == nil || got.Gen != 1 || (status != "" && got.Seed == nil) {
			t.Errorf("%s: read back %+v %v, want the given-up back at gen 1 with the rest kept", name, got, err)
		}
		if n := giveUpRows(t, pool, id); n != 1 {
			t.Errorf("%s: %d given-up rows, want 1", name, n)
		}
	}
	if _, _, err := pg.GiveUpRunLocalFolder(ctx, id, past, givenUp, giveUpEvent, nil); err == nil {
		t.Error("a give-up with no audit row was let run")
	}
}

// A give-up never lands on a live run, one with no sandbox, nor before its time: the cap since its end, or
// for D1 the hold since its containment was due with the substrate's refusal or a containment error old
// enough. One that does not land writes no row and leaves the record as it was.
func TestPG_FolderGiveUp_NeverOnALiveRunOrBeforeItsTime(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	back := types.FolderPassRecord{Error: "given up", Refused: []types.FolderRefusal{}}
	const resume = `{"gen":1,"resume":{"stalled":0,"unsynced":2}}`
	for name, tc := range map[string]struct {
		state      types.RunState
		ended      time.Duration
		noSandbox  bool
		containErr time.Duration // ago; 0 = none
		created    time.Duration // ago; 0 = now
		when       store.FolderGiveUpWhen
		want       bool
	}{
		"running, past the cap":        {state: types.RunRunning, ended: 2 * time.Hour, when: store.FolderGiveUpWhen{Cap: time.Hour}},
		"running, D1":                  {state: types.RunRunning, ended: 2 * time.Hour, when: store.FolderGiveUpWhen{Cap: 24 * time.Hour, D1: true, Unsupported: true}},
		"no sandbox, past the cap":     {state: types.RunStopped, ended: 2 * time.Hour, noSandbox: true, when: store.FolderGiveUpWhen{Cap: time.Hour}},
		"terminal, before the cap":     {state: types.RunStopped, ended: time.Minute, when: store.FolderGiveUpWhen{Cap: time.Hour}},
		"terminal, D1 before the hold": {state: types.RunStopped, ended: time.Minute, when: store.FolderGiveUpWhen{Cap: 24 * time.Hour, D1: true, Hold: time.Hour, Unsupported: true}},
		"terminal, D1 young error":     {state: types.RunStopped, ended: 2 * time.Hour, containErr: time.Minute, when: store.FolderGiveUpWhen{Cap: 24 * time.Hour, D1: true, ContainErrAge: time.Hour}},
		"terminal, D1 no refusal":      {state: types.RunStopped, ended: 2 * time.Hour, when: store.FolderGiveUpWhen{Cap: 24 * time.Hour, D1: true}},
		"terminal, D1 old error":       {state: types.RunStopped, ended: 2 * time.Hour, containErr: 2 * time.Hour, when: store.FolderGiveUpWhen{Cap: 24 * time.Hour, D1: true, ContainErrAge: time.Hour}, want: true},
		"terminal, D1 refused stop":    {state: types.RunStopped, ended: 2 * time.Hour, when: store.FolderGiveUpWhen{Cap: 24 * time.Hour, D1: true, Hold: time.Hour, Unsupported: true}, want: true},
		"terminal, D1 due by max age":  {state: types.RunStopped, ended: time.Minute, created: 3 * time.Hour, when: store.FolderGiveUpWhen{Cap: 24 * time.Hour, D1: true, Hold: time.Hour, Unsupported: true, MaxAge: time.Hour}, want: true},
		"terminal, past the cap":       {state: types.RunStopped, ended: 2 * time.Hour, when: store.FolderGiveUpWhen{Cap: time.Hour}, want: true},
	} {
		id := giveUpRun(t, pool, tc.state, resume, tc.ended)
		if tc.noSandbox {
			if _, err := pool.Exec(ctx, `UPDATE agent_runs SET sandbox_ref = '' WHERE id = $1`, id); err != nil {
				t.Fatal(err)
			}
		}
		if tc.created > 0 {
			if _, err := pool.Exec(ctx, `UPDATE agent_runs SET created_at = now() - make_interval(secs => $2) WHERE id = $1`, id, tc.created.Seconds()); err != nil {
				t.Fatal(err)
			}
		}
		if tc.containErr > 0 {
			if _, err := pool.Exec(ctx, `UPDATE agent_runs SET containment_error = 'proxy down', containment_error_at = now() - make_interval(secs => $2) WHERE id = $1`, id, tc.containErr.Seconds()); err != nil {
				t.Fatal(err)
			}
		}
		if _, ok, err := pg.GiveUpRunLocalFolder(ctx, id, tc.when, back, giveUpEvent, prepareSpoolTest); ok != tc.want || err != nil {
			t.Errorf("%s: applied %v %v, want %v", name, ok, err, tc.want)
		}
		if n := giveUpRows(t, pool, id); (n == 1) != tc.want || n > 1 {
			t.Errorf("%s: %d given-up rows, want %v", name, n, tc.want)
		}
		if got, _ := pg.RunLocalFolderStatus(ctx, id); !tc.want && (got.Resume == nil || got.Back != nil || got.Gen != 1) {
			t.Errorf("%s: a give-up that did not apply changed the record: %+v", name, got)
		}
	}
}

// ContainmentDue and the give-up's SQL agree on when a run's containment was due.
func TestPG_ContainmentDue_GoAndSQLAgree(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	end := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	grace := time.Hour
	for name, tc := range map[string]struct {
		set    string
		maxAge time.Duration
		want   time.Time
	}{
		"an ordinary end":        {set: `lost_at = NULL`, want: end},
		"lost with reason ended": {set: `lost_at = $2::timestamptz - interval '3 hours', lost_reason = 'ended'`, want: end.Add(-2 * time.Hour)},
		"lost with a lease end":  {set: `lost_at = $2::timestamptz - interval '1 hour', lost_reason = 'outage', ends_at = $2::timestamptz - interval '5 hours'`, want: end.Add(-4 * time.Hour)},
		"max age":                {set: `created_at = $2::timestamptz - interval '10 hours', admitted_at = NULL`, maxAge: 2 * time.Hour, want: end.Add(-8 * time.Hour)},
		"max age 0":              {set: `created_at = $2::timestamptz - interval '10 hours', admitted_at = NULL`, want: end},
	} {
		id := giveUpRun(t, pool, types.RunStopped, "", 0)
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET ended_at = $2, updated_at = $2, `+tc.set+` WHERE id = $1`, id, end); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		run, err := pg.GetRun(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		var inSQL time.Time
		if err := pool.QueryRow(ctx, `SELECT `+store.ContainmentDueSQL()+` FROM agent_runs WHERE id = $3`, grace.Seconds(), store.MaxAgeSecs(tc.maxAge), id).Scan(&inSQL); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if inGo := store.ContainmentDue(run, grace, tc.maxAge); !inGo.Equal(tc.want) || !inSQL.Equal(tc.want) {
			t.Errorf("%s: Go %s, SQL %s, want %s", name, inGo, inSQL, tc.want)
		}
	}
}
