// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

// The live state a second wardynd must see (migration 0129), against Postgres: the exec tail's
// chunks, the attach lease and the sign-in end counter.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Chunks come back in order, are trimmed to about one tail as new ones arrive, die in the
// transaction that writes the final row, are refused once that row exists, and stop at an
// erasure tombstone.
func TestPG_RunOutputChunks(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	run := persistRun(t, ctx, pool, newRun(types.RunRunning)).ID

	if got, err := pg.ReadRunOutputChunks(ctx, run, 100); err != nil || got.Found {
		t.Fatalf("a run with no chunks read %+v err=%v, want none found", got, err)
	}
	for _, p := range []string{"one ", "two ", "three"} {
		if wrote, err := pg.AppendRunOutputChunk(ctx, run, "replica-a", []byte(p), 1<<20); err != nil || !wrote {
			t.Fatalf("append %q: wrote=%v err=%v", p, wrote, err)
		}
	}
	got, err := pg.ReadRunOutputChunks(ctx, run, 100)
	if err != nil || !got.Found || string(got.Bytes) != "one two three" || got.Truncated {
		t.Fatalf("read = %+v err=%v, want the chunks in order and not truncated", got, err)
	}
	if got, _ = pg.ReadRunOutputChunks(ctx, run, 5); string(got.Bytes) != "three" || !got.Truncated {
		t.Fatalf("a read capped at 5 bytes = %+v, want the last five, truncated", got)
	}

	// Trimming keeps the fewest newest chunks that hold keep bytes.
	run2 := persistRun(t, ctx, pool, newRun(types.RunRunning)).ID
	for _, p := range []string{"aaaa", "bbbb", "cccc", "dddd"} {
		if _, err := pg.AppendRunOutputChunk(ctx, run2, "r", []byte(p), 6); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ = pg.ReadRunOutputChunks(ctx, run2, 100); string(got.Bytes) != "ccccdddd" || !got.Truncated {
		t.Fatalf("after trimming to 6 bytes the chunks = %q truncated=%v, want the newest two", got.Bytes, got.Truncated)
	}

	// The final row replaces the chunks, in its own transaction, and no later chunk is written.
	if err := pg.SaveFinalRunOutput(ctx, store.RunOutput{RunID: run, Output: []byte("one two three"), Source: "stdout"}); err != nil {
		t.Fatal(err)
	}
	if got, _ = pg.ReadRunOutputChunks(ctx, run, 100); got.Found {
		t.Fatalf("chunks survived the final row: %q", got.Bytes)
	}
	if wrote, err := pg.AppendRunOutputChunk(ctx, run, "slow", []byte("late"), 1<<20); err != nil || wrote {
		t.Fatalf("a chunk after the final row: wrote=%v err=%v, want it dropped", wrote, err)
	}
	if got, _ = pg.ReadRunOutputChunks(ctx, run, 100); got.Found {
		t.Fatal("a late chunk recreated what the final row replaced")
	}

	// A row from chunks: a capture gap, incomplete, with the bytes; never over a final row.
	if wrote, err := pg.SaveChunkedRunOutput(ctx, run2, "run", 100); err != nil || !wrote {
		t.Fatalf("a row from chunks: wrote=%v err=%v", wrote, err)
	}
	row, found, _ := pg.GetRunOutput(ctx, run2)
	if !found || row.CapturedAt == nil || !row.CaptureGap || !row.Incomplete || !row.Truncated || string(row.Output) != "ccccdddd" || row.MaskScope != "run" {
		t.Fatalf("row from chunks = %+v, want an incomplete capture gap holding the chunk bytes", row)
	}
	if got, _ = pg.ReadRunOutputChunks(ctx, run2, 100); got.Found {
		t.Fatal("chunks survived the row written from them")
	}
	if wrote, err := pg.SaveChunkedRunOutput(ctx, run, "run", 100); err != nil || wrote {
		t.Fatalf("a row from chunks over nothing: wrote=%v err=%v, want false", wrote, err)
	}

	// An erasure deletes the chunks with the rows, and every writer and reader then refuses.
	run3 := persistRun(t, ctx, pool, newRun(types.RunRunning)).ID
	if _, err := pg.AppendRunOutputChunk(ctx, run3, "r", []byte("secret-ish output"), 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := pg.EraseRunOutputs(ctx, []uuid.UUID{run3}); err != nil {
		t.Fatal(err)
	}
	var left int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM run_output_chunks WHERE run_id = $1`, run3).Scan(&left)
	if left != 0 {
		t.Fatalf("%d chunks left after the erasure", left)
	}
	if _, err := pg.AppendRunOutputChunk(ctx, run3, "late", []byte("x"), 1<<20); !errors.Is(err, store.ErrRunOutputErased) {
		t.Errorf("a chunk after the erasure: %v, want ErrRunOutputErased", err)
	}
	if _, err := pg.ReadRunOutputChunks(ctx, run3, 10); !errors.Is(err, store.ErrRunOutputErased) {
		t.Errorf("a read after the erasure: %v, want ErrRunOutputErased", err)
	}
	if _, err := pg.SaveChunkedRunOutput(ctx, run3, "", 10); !errors.Is(err, store.ErrRunOutputErased) {
		t.Errorf("a row from chunks after the erasure: %v, want ErrRunOutputErased", err)
	}
}

// One holder at a time; a lapsed lease is free; the holder is the identity every check is made
// against; a take-over deletes the row.
func TestPG_AttachLeases(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	run := persistRun(t, ctx, pool, newRun(types.RunRunning)).ID
	a, b := uuid.New(), uuid.New()
	lease := func(id uuid.UUID, replica string) store.AttachLease {
		return store.AttachLease{RunID: run, HolderID: id, Replica: replica, Principal: "alice", Source: "web", Since: time.Now().UTC()}
	}

	if ok, err := pg.AcquireAttachLease(ctx, lease(a, "A"), uuid.Nil, time.Minute); err != nil || !ok {
		t.Fatalf("a free lease: ok=%v err=%v", ok, err)
	}
	if ok, err := pg.AcquireAttachLease(ctx, lease(b, "B"), uuid.Nil, time.Minute); err != nil || ok {
		t.Fatalf("a held lease taken by another: ok=%v err=%v, want refused", ok, err)
	}
	if held, _ := pg.HoldsAttachLease(ctx, run, a); !held {
		t.Error("the holder does not hold its lease")
	}
	if held, _ := pg.HoldsAttachLease(ctx, run, b); held {
		t.Error("another holder holds a lease it was refused")
	}
	if ok, _ := pg.RenewAttachLease(ctx, run, b, time.Minute); ok {
		t.Error("a non-holder renewed the lease")
	}
	// A hand-off names the holder it takes from.
	if ok, err := pg.AcquireAttachLease(ctx, lease(b, "B"), a, time.Minute); err != nil || !ok {
		t.Fatalf("a hand-off from the holder: ok=%v err=%v", ok, err)
	}
	if held, _ := pg.HoldsAttachLease(ctx, run, a); held {
		t.Error("the previous holder still holds the lease after a hand-off")
	}
	got, found, err := pg.GetAttachLease(ctx, run)
	if err != nil || !found || got.HolderID != b || got.Replica != "B" || !got.Live || got.Epoch < 2 {
		t.Fatalf("lease = %+v found=%v err=%v, want B's, live, a later epoch", got, found, err)
	}

	// A take-over reserves the slot for the taker: the displaced holder fails its next check,
	// another client cannot take the slot, the taker's client can, and a release by a non-holder
	// deletes nothing.
	if released, _ := pg.ReleaseAttachLease(ctx, run, a); released {
		t.Error("a non-holder released the lease")
	}
	prev, reservation, found, err := pg.ReserveAttachLease(ctx, run, uuid.Nil, "admin", "B", time.Minute)
	if err != nil || !found || prev.HolderID != b || reservation == uuid.Nil || reservation == b {
		t.Fatalf("take-over replaced %+v (reservation %s) found=%v err=%v, want B's lease replaced by a reservation", prev, reservation, found, err)
	}
	if ok, _ := pg.RenewAttachLease(ctx, run, b, time.Minute); ok {
		t.Error("the displaced holder renewed a lease that was taken over")
	}
	if held, _ := pg.HoldsAttachLease(ctx, run, b); held {
		t.Error("the displaced holder still holds the lease")
	}
	if ok, _ := pg.AcquireAttachLease(ctx, store.AttachLease{RunID: run, HolderID: a, Replica: "A", Principal: "alice", Source: "web", Since: time.Now().UTC()}, uuid.Nil, time.Minute); ok {
		t.Error("a bystander took the slot reserved for the taker")
	}
	if _, _, found, _ := pg.ReserveAttachLease(ctx, run, b, "admin", "B", time.Minute); found {
		t.Error("a take-over named a holder that no longer holds the lease and still replaced the lease")
	}
	taker := uuid.New()
	if ok, err := pg.AcquireAttachLease(ctx, store.AttachLease{RunID: run, HolderID: taker, Replica: "B", Principal: "admin", Source: "web", Since: time.Now().UTC()}, uuid.Nil, time.Minute); err != nil || !ok {
		t.Fatalf("the taker's client could not take its reservation: ok=%v err=%v", ok, err)
	}
	if got, _, _ = pg.GetAttachLease(ctx, run); got.HolderID != taker || got.Source != "web" {
		t.Fatalf("lease after the taker took it = %+v, want the taker's client as a web holder", got)
	}

	// A released lease is free, and a lapsed one is free to anyone.
	if released, _ := pg.ReleaseAttachLease(ctx, run, taker); !released {
		t.Fatal("the holder could not release its lease")
	}
	if ok, _ := pg.AcquireAttachLease(ctx, lease(a, "A"), uuid.Nil, 50*time.Millisecond); !ok {
		t.Fatal("could not take a free lease")
	}
	time.Sleep(120 * time.Millisecond)
	if held, _ := pg.HoldsAttachLease(ctx, run, a); held {
		t.Error("a lapsed lease is still held")
	}
	if ok, err := pg.AcquireAttachLease(ctx, lease(b, "B"), uuid.Nil, time.Minute); err != nil || !ok {
		t.Fatalf("a lapsed lease taken by another: ok=%v err=%v", ok, err)
	}
}

// The count moves at each end's start and finish, and an end that never finished stops counting
// as running after ADOSignInEndsStaleAfter.
func TestPG_ADOSignInEnds(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)

	st, err := pg.ADOSignInEnds(ctx, "alice")
	if err != nil || st.Gen != 0 || st.Running {
		t.Fatalf("a person with no row = %+v err=%v, want gen 0 and nothing running", st, err)
	}
	if err := pg.BeginADOSignInEnd(ctx, "alice", "disconnect"); err != nil {
		t.Fatal(err)
	}
	st, _ = pg.ADOSignInEnds(ctx, "alice")
	if st.Gen != 1 || !st.Running || st.Reason != "disconnect" {
		t.Fatalf("after begin = %+v, want gen 1, running, reason disconnect", st)
	}
	if err := pg.FinishADOSignInEnd(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	st, _ = pg.ADOSignInEnds(ctx, "alice")
	if st.Gen != 2 || st.Running {
		t.Fatalf("after finish = %+v, want gen 2 and nothing running", st)
	}
	if other, _ := pg.ADOSignInEnds(ctx, "bob"); other.Gen != 0 {
		t.Fatalf("another person's count moved: %+v", other)
	}

	// An end whose replica died.
	if err := pg.BeginADOSignInEnd(ctx, "alice", "offboarding"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE ado_signin_ends SET updated_at = now() - interval '11 minutes' WHERE owner = 'alice'`); err != nil {
		t.Fatal(err)
	}
	if st, _ = pg.ADOSignInEnds(ctx, "alice"); st.Running {
		t.Fatalf("an end that began 11 minutes ago and never finished still counts as running: %+v", st)
	}
	if st.Reason != "offboarding" {
		t.Errorf("reason = %q", st.Reason)
	}
}
