// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Launch intents (migration 0171) against a real Postgres. Guarded by
// WARDYN_TEST_PG; each test works in its own throwaway database.
package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func intentRun(by, intent string) types.AgentRun {
	r := newRun(types.RunPending)
	r.CreatedBy = by
	r.LaunchIntent = json.RawMessage(`{"api_version":"wardyn/v1","kind":"Run","intent":` + intent + `}`)
	return r
}

func TestPG_LaunchIntentWrittenWithItsRun(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	older := intentRun("alice", `{"agent":"claude-code","task":"one"}`)
	older.Title, older.CreatedAt = "first", older.CreatedAt.Add(-time.Minute)
	a, err := st.CreateRun(ctx, older) // no identity guard on ctx: still one transaction
	if err != nil || a.LaunchIntent == nil {
		t.Fatalf("CreateRun = %v kept %v", err, a.LaunchIntent != nil)
	}
	b, err := st.CreateRunUnderCap(ctx, intentRun("alice", `{"agent":"claude-code","shape":"agent_task","task":"two"}`), 10)
	if err != nil || b.LaunchIntent == nil {
		t.Fatalf("CreateRunUnderCap = %v kept %v", err, b.LaunchIntent != nil)
	}
	plain, err := st.CreateRun(ctx, newRun(types.RunPending))
	if err != nil || plain.LaunchIntent != nil {
		t.Fatalf("a run with no intent = %v %s", err, plain.LaunchIntent)
	}
	if _, err := st.CreateRun(ctx, intentRun("bob", `{"agent":"claude-code"}`)); err != nil {
		t.Fatal(err)
	}

	list, err := st.ListLaunchIntents(ctx, "alice", 50)
	if err != nil || len(list) != 2 || list[0].RunID != b.ID || list[1].RunID != a.ID {
		t.Fatalf("alice's list = %+v %v, want two, newest first", list, err)
	}
	if list[0].Shape != "agent_task" || list[1].Title != "first" || list[0].Document != nil {
		t.Errorf("summaries = %+v, want the shape, the run's title and no document", list)
	}
	if one, _ := st.ListLaunchIntents(ctx, "alice", 1); len(one) != 1 || one[0].RunID != b.ID {
		t.Errorf("limit 1 = %+v", one)
	}
	got, err := st.GetLaunchIntent(ctx, a.ID, "alice")
	var doc struct {
		Intent struct{ Task string } `json:"intent"`
	}
	if err != nil || json.Unmarshal(got.Document, &doc) != nil || doc.Intent.Task != "one" {
		t.Fatalf("GetLaunchIntent = %+v %v", got, err)
	}
	for name, id := range map[string]struct {
		run uuid.UUID
		by  string
	}{"another person's run": {a.ID, "bob"}, "a run that kept none": {plain.ID, plain.CreatedBy}} {
		if _, err := st.GetLaunchIntent(ctx, id.run, id.by); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s: %v, want ErrNotFound", name, err)
		}
	}

	if n, err := st.DeleteLaunchIntentsCreatedBy(ctx, "alice"); err != nil || n != 2 {
		t.Fatalf("erase alice = %d %v, want 2", n, err)
	}
	if left, _ := st.ListLaunchIntents(ctx, "alice", 50); len(left) != 0 {
		t.Errorf("alice keeps %d after the erase", len(left))
	}
	if bobs, _ := st.ListLaunchIntents(ctx, "bob", 50); len(bobs) != 1 {
		t.Errorf("the erase of alice took bob's: %d left", len(bobs))
	}
}

// D25-C: a launch that began before an erasure of its creator's intents never
// commits one after the erasure commits. The launches are held inside their own
// transaction by the deployment cap's lock, which they take after the person lock.
func TestPG_LaunchIntentEraseLinearizesWithLaunches(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	capHold, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = capHold.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := capHold.Exec(ctx, `SELECT pg_advisory_xact_lock($1, 0)`, db.RunCapLockClass); err != nil {
		t.Fatal(err)
	}
	type created struct {
		run types.AgentRun
		err error
	}
	launch := func(r types.AgentRun) chan created {
		ch := make(chan created, 1)
		go func() { got, err := st.CreateRunUnderCap(ctx, r, 10); ch <- created{got, err} }()
		return ch
	}

	before := intentRun("alice", `{"agent":"claude-code","task":"before"}`)
	beforeDone := launch(before)
	waitAdvisoryWaiters(ctx, t, capHold, 1) // holds alice's lock, waits on the cap
	erased := make(chan int, 1)
	go func() {
		n, err := st.DeleteLaunchIntentsCreatedBy(ctx, "alice")
		if err != nil {
			t.Error(err)
		}
		erased <- n
	}()
	waitAdvisoryWaiters(ctx, t, capHold, 2) // the erase waits for that launch
	// The uncapped path (no WARDYN_MAX_CONCURRENT_RUNS) meets the queued erase too: it starts at once, with no intent.
	// Through a store on a pool of its own: this goroutine holds capHold, and a second acquire from its pool trips the nested-acquire guard.
	other, err := pgxpool.New(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	uncapped, err := store.NewPG(other).CreateRun(ctx, intentRun("alice", `{"agent":"claude-code","task":"uncapped"}`))
	if err != nil || uncapped.LaunchIntent != nil {
		t.Fatalf("CreateRun beside a queued erase = %v, kept %v; want started with no intent", err, uncapped.LaunchIntent != nil)
	}
	during := intentRun("alice", `{"agent":"claude-code","task":"during"}`)
	duringDone := launch(during)
	waitAdvisoryWaiters(ctx, t, capHold, 3) // met the queued erase, now waits on the cap
	if err := capHold.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	b, d, n := <-beforeDone, <-duringDone, <-erased
	if b.err != nil || d.err != nil {
		t.Fatalf("launches: %v / %v", b.err, d.err)
	}
	if b.run.LaunchIntent == nil || n != 1 {
		t.Errorf("the launch ahead of the erase kept %v and the erase removed %d, want kept then removed", b.run.LaunchIntent != nil, n)
	}
	if d.run.LaunchIntent != nil {
		t.Error("the launch that met the erase kept an intent")
	}
	for name, run := range map[string]types.AgentRun{"before": before, "during": during, "uncapped, during": uncapped} {
		if _, err := st.GetLaunchIntent(ctx, run.ID, "alice"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("the launch %s the erase still has an intent after it: %v", name, err)
		}
		if _, err := st.GetRun(ctx, run.ID); err != nil {
			t.Errorf("the launch %s the erase did not start: %v", name, err)
		}
	}
}

// The same linearization on the queued path: CreateQueuedRun takes the person lock before it waits for
// the pool row, so a queued launch that began before an erase keeps its intent until the erase removes
// it, and one that meets the queued erase starts with none.
func TestPG_LaunchIntentEraseLinearizesWithQueuedLaunches(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p := queuePool(t, st)
	poolHold, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = poolHold.Rollback(context.WithoutCancel(ctx)) }()
	var locked uuid.UUID
	if err := poolHold.QueryRow(ctx, `SELECT id FROM runner_pools WHERE id = $1 FOR NO KEY UPDATE`, p.ID).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	// waitOnPool returns once n sessions wait on that row lock, through the connection this test already holds.
	waitOnPool := func(n int) {
		t.Helper()
		for {
			var waiting int
			if err := poolHold.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype IN ('transactionid', 'tuple') AND NOT granted`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting >= n {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatalf("%d sessions never waited on the pool row", n)
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	type created struct {
		run types.AgentRun
		err error
	}
	launch := func(r types.AgentRun) chan created {
		ch := make(chan created, 1)
		go func() {
			got, err := st.CreateQueuedRun(ctx, r, store.RunQueueEntry{QueuedAt: time.Now().UTC(), DeadlineAt: time.Now().UTC().Add(time.Hour)}, 0)
			ch <- created{got, err}
		}()
		return ch
	}
	queued := func(task string) types.AgentRun {
		r := poolRun(p, types.RunQueued, "alice")
		r.LaunchIntent = intentRun("alice", `{"agent":"claude-code","task":"`+task+`"}`).LaunchIntent
		return r
	}

	before := queued("before")
	beforeDone := launch(before)
	waitOnPool(1) // holds alice's lock, waits on the pool row
	erased := make(chan int, 1)
	go func() {
		n, err := st.DeleteLaunchIntentsCreatedBy(ctx, "alice")
		if err != nil {
			t.Error(err)
		}
		erased <- n
	}()
	waitAdvisoryWaiters(ctx, t, poolHold, 1) // the erase waits for that launch
	during := queued("during")
	duringDone := launch(during)
	waitOnPool(2) // met the queued erase, now waits on the pool row
	if err := poolHold.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	b, d, n := <-beforeDone, <-duringDone, <-erased
	if b.err != nil || d.err != nil {
		t.Fatalf("queued launches: %v / %v", b.err, d.err)
	}
	if b.run.LaunchIntent == nil || n != 1 {
		t.Errorf("the queued launch ahead of the erase kept %v and the erase removed %d, want kept then removed", b.run.LaunchIntent != nil, n)
	}
	if d.run.LaunchIntent != nil {
		t.Error("the queued launch that met the erase kept an intent")
	}
	for name, run := range map[string]types.AgentRun{"before": b.run, "during": d.run} {
		if _, err := st.GetLaunchIntent(ctx, run.ID, "alice"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("the queued launch %s the erase still has an intent after it: %v", name, err)
		}
	}
}

// waitAdvisoryWaiters returns once n sessions wait on an advisory lock. It polls
// through q, the connection the test already holds: a second pool acquire while
// the cap holder is out would trip the nested-acquire guard.
func waitAdvisoryWaiters(ctx context.Context, t *testing.T, q pgx.Tx, n int) {
	t.Helper()
	for {
		var waiting int
		if err := q.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&waiting); err != nil {
			t.Fatalf("poll pg_locks (want %d waiting): %v", n, err)
		}
		if waiting >= n {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("never saw %d session(s) waiting on an advisory lock (%d)", n, waiting)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// A run that carries an intent is one transaction even with no identity guard on ctx: an intent insert
// that fails leaves no run, through CreateRun and CreateRunUnderCap alike.
func TestPG_LaunchIntentFailureLeavesNoRun(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION li_refuse() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'intent refused'; END $$;
		CREATE TRIGGER li_refuse BEFORE INSERT ON run_launch_intents FOR EACH ROW EXECUTE FUNCTION li_refuse();`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRun(ctx, intentRun("alice", `{"agent":"claude-code"}`)); err == nil {
		t.Fatal("CreateRun succeeded with a failing intent insert")
	}
	if _, err := st.CreateRunUnderCap(ctx, intentRun("alice", `{"agent":"claude-code"}`), 10); err == nil {
		t.Fatal("CreateRunUnderCap succeeded with a failing intent insert")
	}
	var runs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_runs`).Scan(&runs); err != nil || runs != 0 {
		t.Errorf("agent_runs holds %d row(s) (%v) after the intent inserts failed, want none", runs, err)
	}
}
