// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

// InsertAuditEvents against a real Postgres: a batch takes the chain lock once, its seqs are contiguous and
// chain like single rows, it is all or nothing, a replay holds what is stored, and no row's hash input changes.
// Guarded by WARDYN_TEST_PG (runsPGPoolIsolated skips without it).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auditreceipt"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// batchEvents builds n events on run, each with its own id and a distinct time.
func batchEvents(run *uuid.UUID, n int, tag string) []*types.AuditEvent {
	base := time.Now().UTC()
	evs := make([]*types.AuditEvent, n)
	for i := range evs {
		evs[i] = &types.AuditEvent{ID: uuid.New(), Time: base.Add(time.Duration(i) * time.Microsecond), RunID: run,
			ActorType: types.ActorAgent, Actor: "batch-test", Action: "test.batch", Target: fmt.Sprintf("%s-%d", tag, i),
			Outcome: "success", Data: []byte(fmt.Sprintf(`{"i": %d, "tag": %q}`, i, tag))}
	}
	return evs
}

// seqsOf reads the seq of each id.
func seqsOf(t *testing.T, pool *pgxpool.Pool, evs []*types.AuditEvent) []int64 {
	t.Helper()
	out := make([]int64, len(evs))
	for i, ev := range evs {
		out[i] = auditSeq(t, pool, ev.ID)
	}
	return out
}

func assertContiguous(t *testing.T, what string, seqs []int64) {
	t.Helper()
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("%s: seqs %v are not contiguous at %d", what, seqs, i)
		}
	}
}

func verifyChainOK(t *testing.T, pool *pgxpool.Pool) store.AuditChainStatus {
	t.Helper()
	st, err := store.NewPG(pool).VerifyAuditChain(context.Background())
	if err != nil || !st.OK {
		t.Fatalf("VerifyAuditChain = %+v, %v; want ok", st, err)
	}
	return st
}

// TestInsertAuditEvents_ContiguousSeqUnderOneLock_PG: three writers append 50-row batches while three others
// append single rows; every batch's seqs are contiguous, each row chains to the one before it, and the whole
// chain verifies.
func TestInsertAuditEvents_ContiguousSeqUnderOneLock_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	run := uuid.New()
	batches := make([][]*types.AuditEvent, 3*4)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for w := range 3 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for k := range 4 {
				evs := batchEvents(&run, 50, fmt.Sprintf("w%d-b%d", w, k))
				batches[w*4+k] = evs
				held, err := store.InsertAuditEvents(ctx, pool, evs)
				if err != nil {
					errs <- err
					return
				}
				for i, h := range held {
					if h || evs[i].RowHash == "" {
						errs <- fmt.Errorf("batch w%d-b%d event %d: held=%v row_hash=%q", w, k, i, h, evs[i].RowHash)
						return
					}
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range 20 {
				ev := batchEvents(&run, 1, fmt.Sprintf("single-%d", w))[0]
				if err := store.InsertAuditEvent(ctx, pool, ev); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for i, evs := range batches {
		seqs := seqsOf(t, pool, evs)
		assertContiguous(t, fmt.Sprintf("batch %d", i), seqs)
		for j := 1; j < len(evs); j++ {
			if evs[j].PrevHash != evs[j-1].RowHash {
				t.Fatalf("batch %d row %d: prev_hash %q, want the row before's %q", i, j, evs[j].PrevHash, evs[j-1].RowHash)
			}
		}
	}
	if st := verifyChainOK(t, pool); st.Checked < 3*4*50+3*20 {
		t.Fatalf("checked %d rows, want at least %d", st.Checked, 3*4*50+3*20)
	}
}

// TestInsertAuditEvents_SeqOrderIsCommitOrder_PG: a batch and a single writer queued behind a held chain lock
// are not interleaved; a reader sees none of the batch's rows until it commits, and the seqs follow the chain.
func TestInsertAuditEvents_SeqOrderIsCommitOrder_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	run := uuid.New()
	before := batchEvents(&run, 1, "before")[0]
	if err := store.InsertAuditEvent(ctx, pool, before); err != nil {
		t.Fatal(err)
	}
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, db.AuditChainLockKey); err != nil {
		t.Fatal(err)
	}
	batch := batchEvents(&run, 10, "batch")
	single := batchEvents(&run, 1, "single")[0]
	done := make(chan error, 2)
	go func() { _, err := store.InsertAuditEvents(ctx, pool, batch); done <- err }()
	waitForChainLockWaiters(t, pool, 1)
	go func() { done <- store.InsertAuditEvent(ctx, pool, single) }()
	waitForChainLockWaiters(t, pool, 2)
	var visible int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE run_id = $1`, run).Scan(&visible); err != nil || visible != 1 {
		t.Fatalf("rows visible while the batch waits = %d (%v), want only the row before it", visible, err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	seqs := seqsOf(t, pool, batch)
	assertContiguous(t, "batch", seqs)
	s := auditSeq(t, pool, single.ID)
	if s > seqs[0] && s < seqs[len(seqs)-1] {
		t.Fatalf("the single row's seq %d falls inside the batch's %v", s, seqs)
	}
	if b := auditSeq(t, pool, before.ID); b >= seqs[0] || b >= s {
		t.Fatalf("the row committed first has seq %d, after the later writers' (%d, %v)", b, s, seqs)
	}
	verifyChainOK(t, pool)
}

// waitForChainLockWaiters waits until n sessions wait on the audit chain's advisory lock.
func waitForChainLockWaiters(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted
			 AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d sessions wait on the chain lock, want %d", waiting, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestInsertAuditEvents_AllOrNothing_PG: a batch whose third row the table refuses writes no row and leaves the
// chain head where it was.
func TestInsertAuditEvents_AllOrNothing_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	run := uuid.New()
	head := batchEvents(&run, 1, "head")[0]
	if err := store.InsertAuditEvent(ctx, pool, head); err != nil {
		t.Fatal(err)
	}
	evs := batchEvents(&run, 5, "bad")
	evs[2].Outcome = "not-an-outcome" // the table's outcome check refuses it
	if _, err := store.InsertAuditEvents(ctx, pool, evs); err == nil {
		t.Fatal("a batch with a refused row was accepted")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE run_id = $1`, run).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows on the run = %d (%v), want only the head", n, err)
	}
	st := verifyChainOK(t, pool)
	if st.HeadHash != head.RowHash {
		t.Fatalf("chain head %q, want the row before the batch %q", st.HeadHash, head.RowHash)
	}
	if _, err := store.InsertAuditEvents(ctx, pool, batchEvents(&run, 513, "over")); err == nil {
		t.Fatal("a batch over the limit was accepted")
	}
}

// TestInsertAuditEvents_ReplayHoldsStoredRows_PG: under WithAuditReplay a batch whose rows are stored appends
// none again and hands back the stored hashes; a new row in the same batch is appended; a stored id on another
// run does not hold this run's row.
func TestInsertAuditEvents_ReplayHoldsStoredRows_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := store.WithAuditReplay(context.Background())
	run, other := uuid.New(), uuid.New()
	first := batchEvents(&run, 3, "replay")
	if _, err := store.InsertAuditEvents(ctx, pool, first); err != nil {
		t.Fatal(err)
	}
	again := make([]*types.AuditEvent, 0, 4)
	for _, ev := range first {
		c := *ev
		c.PrevHash, c.RowHash = "", ""
		again = append(again, &c)
	}
	fresh := batchEvents(&run, 1, "fresh")[0]
	again = append(again, fresh)
	foreign := *first[0]
	foreign.RunID, foreign.PrevHash, foreign.RowHash = &other, "", ""
	again = append(again, &foreign)
	held, err := store.InsertAuditEvents(ctx, pool, again)
	if err != nil {
		t.Fatal(err)
	}
	for i := range first {
		if !held[i] || again[i].RowHash != first[i].RowHash || again[i].PrevHash != first[i].PrevHash {
			t.Fatalf("replayed row %d: held=%v hashes (%q,%q), want held with (%q,%q)", i, held[i],
				again[i].PrevHash, again[i].RowHash, first[i].PrevHash, first[i].RowHash)
		}
	}
	if held[3] || held[4] || fresh.RowHash == "" || foreign.RowHash == "" {
		t.Fatalf("new rows: held=%v, want both appended", held[3:])
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE id = $1`, first[0].ID).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows with the replayed id = %d (%v), want this run's one and the other run's one", n, err)
	}
	verifyChainOK(t, pool)
}

// TestVerify_PreBatchRowsUnchanged_PG: rows written one by one before and after a batch keep the hashes they
// were stored with, and every row, the batch's included, re-hashes in Go to its stored row_hash.
func TestVerify_PreBatchRowsUnchanged_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	run := uuid.New()
	var all []*types.AuditEvent
	for _, ev := range batchEvents(&run, 3, "before") {
		if err := store.InsertAuditEvent(ctx, pool, ev); err != nil {
			t.Fatal(err)
		}
		all = append(all, ev)
	}
	batch := batchEvents(&run, 4, "batch")
	batch[1].Data = []byte(`{"quote": "a\"b", "nested": {"z": [1, 2.50, null]}}`)
	if _, err := store.InsertAuditEvents(ctx, pool, batch); err != nil {
		t.Fatal(err)
	}
	all = append(all, batch...)
	for _, ev := range batchEvents(&run, 2, "after") {
		if err := store.InsertAuditEvent(ctx, pool, ev); err != nil {
			t.Fatal(err)
		}
		all = append(all, ev)
	}
	ids := make([]uuid.UUID, len(all))
	want := map[uuid.UUID]string{}
	for i, ev := range all {
		ids[i], want[ev.ID] = ev.ID, ev.RowHash
	}
	rows, err := store.NewPG(pool).AuditReceiptRows(ctx, ids, nil, 0, len(ids)+1)
	if err != nil || len(rows) != len(ids) {
		t.Fatalf("AuditReceiptRows = %d rows, %v; want %d", len(rows), err, len(ids))
	}
	for _, r := range rows {
		if r.RowHash == nil || *r.RowHash != want[r.ID] {
			t.Fatalf("seq %d: stored row_hash %v, want the hash handed back at insert %q", r.Seq, r.RowHash, want[r.ID])
		}
		if got := auditreceipt.RowHash(receiptRow(r)); got != *r.RowHash {
			t.Fatalf("seq %d: Go RowHash %s, stored %s", r.Seq, got, *r.RowHash)
		}
	}
	verifyChainOK(t, pool)
}

// TestVerify_TamperedBatchRowFails_PG: an edit to the middle row of a batch breaks the chain at that row.
func TestVerify_TamperedBatchRowFails_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	run := uuid.New()
	batch := batchEvents(&run, 5, "tamper")
	if _, err := store.InsertAuditEvents(ctx, pool, batch); err != nil {
		t.Fatal(err)
	}
	verifyChainOK(t, pool)
	mid := auditSeq(t, pool, batch[2].ID)
	tamperAudit(t, pool, `UPDATE audit_events SET target = 'edited' WHERE seq = $1`, mid)
	st, err := store.NewPG(pool).VerifyAuditChain(ctx)
	if err != nil || st.OK || st.BrokenSeq != mid {
		t.Fatalf("VerifyAuditChain = %+v, %v; want broken at the edited batch row %d", st, err, mid)
	}
}

// TestRecorderRecordBatch_RefusesWholeBatchOnBadOutcome: one outcome the table cannot store refuses the whole
// batch before anything is sent, counted and logged once per refused row, so nothing of it is stored or noted
// and the spool above keeps it whole. Needs no database.
func TestRecorderRecordBatch_RefusesWholeBatchOnBadOutcome(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	run := uuid.New()
	evs := batchEvents(&run, 4, "outcome")
	evs[1].Outcome, evs[3].Outcome = "maybe", "pending"
	batch := make([]types.AuditEvent, len(evs))
	for i, ev := range evs {
		batch[i] = *ev
	}
	before := store.RefusedAuditOutcomes()
	ctx, out := audit.WithBatchOutcome(context.Background())
	err := store.Recorder{}.RecordBatch(ctx, batch) // a nil pool: any write would panic
	if !errors.Is(err, store.ErrAuditOutcome) {
		t.Fatalf("RecordBatch = %v, want ErrAuditOutcome", err)
	}
	if got := store.RefusedAuditOutcomes() - before; got != 2 {
		t.Fatalf("refused counter moved by %d, want 2", got)
	}
	if n := strings.Count(logs.String(), "audit row refused"); n != 2 {
		t.Fatalf("%d refusal lines, want one per refused row: %s", n, logs.String())
	}
	for _, ev := range batch {
		if st := out.Status(ev.ID); st != audit.BatchUnsettled {
			t.Fatalf("event %s noted %d, want nothing noted", ev.ID, st)
		}
	}
}
