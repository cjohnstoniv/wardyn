// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sendRunWithApprovals creates a run of owner with n tool_call approvals, each with a scope of its own so
// the pending index never folds two.
func sendRunWithApprovals(t *testing.T, st store.PG, owner string, n int) (uuid.UUID, []uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	run := newRun(types.RunPending)
	run.CreatedBy = owner
	if _, err := st.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	ids := make([]uuid.UUID, n)
	for i := range ids {
		ap, err := st.CreateApproval(ctx, types.ApprovalRequest{ID: uuid.New(), RunID: run.ID, Kind: types.ApprovalToolCall,
			State: types.ApprovalPending, RequestedAt: time.Now().UTC(), RequestedScope: json.RawMessage(`{"tool":"t-` + uuid.NewString() + `"}`)})
		if err != nil {
			t.Fatalf("create approval: %v", err)
		}
		ids[i] = ap.ID
	}
	return run.ID, ids
}

// TestPG_MCPCallSends_OneRecordPerKey: the first record of a key in its namespace is made and every later
// one returns it unchanged; another owner or scope has its own; the answer stamp is set once, and only
// for the approval's own run.
func TestPG_MCPCallSends_OneRecordPerKey(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	owner := "sends-" + uuid.NewString()
	runID, aps := sendRunWithApprovals(t, st, owner, 3)
	otherRun, _ := sendRunWithApprovals(t, st, owner, 0)
	rec := store.MCPCallSend{Owner: owner, OperationScope: "corr:job-7", OperationKey: "step-1", ApprovalID: aps[0], RunID: runID, CallSHA256: "aaa"}
	first, made, err := st.RecordMCPCallSend(ctx, rec)
	if err != nil || !made || first.ApprovalID != aps[0] || first.SentAt.IsZero() || first.AnsweredAt != nil {
		t.Fatalf("first record = %+v, %v, %v; want made, dated, unanswered", first, made, err)
	}
	again := rec
	again.ApprovalID, again.CallSHA256 = aps[1], "bbb"
	if got, made, err := st.RecordMCPCallSend(ctx, again); err != nil || made || got.ApprovalID != aps[0] || got.CallSHA256 != "aaa" || !got.SentAt.Equal(first.SentAt) {
		t.Fatalf("second record = %+v, %v, %v; want the first, unchanged", got, made, err)
	}
	for _, other := range []store.MCPCallSend{
		{Owner: "x-" + owner, OperationScope: rec.OperationScope, OperationKey: rec.OperationKey, ApprovalID: aps[2], RunID: runID, CallSHA256: "aaa"},
		{Owner: owner, OperationScope: "run:" + runID.String(), OperationKey: rec.OperationKey, ApprovalID: aps[2], RunID: runID, CallSHA256: "aaa"},
	} {
		if _, made, err := st.RecordMCPCallSend(ctx, other); err != nil || !made {
			t.Fatalf("record %+v = %v, %v; want a row of its own", other, made, err)
		}
	}
	if found, err := st.MarkMCPCallAnswered(ctx, aps[0], otherRun); err != nil || found {
		t.Fatalf("stamp for another run = %v, %v; want not found", found, err)
	}
	if found, err := st.MarkMCPCallAnswered(ctx, aps[0], runID); err != nil || !found {
		t.Fatalf("stamp = %v, %v; want found", found, err)
	}
	stamped, err := st.GetMCPCallSend(ctx, owner, rec.OperationScope, rec.OperationKey)
	if err != nil || stamped.AnsweredAt == nil {
		t.Fatalf("after the stamp = %+v, %v; want answered_at set", stamped, err)
	}
	if found, err := st.MarkMCPCallAnswered(ctx, aps[0], runID); err != nil || !found {
		t.Fatalf("second stamp = %v, %v; want found", found, err)
	}
	if later, err := st.GetMCPCallSend(ctx, owner, rec.OperationScope, rec.OperationKey); err != nil || later.AnsweredAt == nil || !later.AnsweredAt.Equal(*stamped.AnsweredAt) {
		t.Fatalf("a second stamp moved answered_at: %+v, %v", later, err)
	}
	if _, err := st.GetMCPCallSend(ctx, owner, rec.OperationScope, "step-2"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unrecorded key = %v, want ErrNotFound", err)
	}
}

// TestPG_MCPCallSends_AnswerFor24Hours: a send answers for its key on the database's clock for 24 hours
// and not at their end. Just inside, a read finds it and a record of the key changes nothing; at exactly
// 24 hours a read finds nothing and a record overwrites it with the new send, unanswered, whose own
// stamp then lands and the old approval's does not. Without the clock, neither a read nor a record
// answers.
func TestPG_MCPCallSends_AnswerFor24Hours(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	// The database's clock stands at at (DBNow), three days behind the real one, so a decision on any
	// other clock is caught. In microseconds, as the column keeps them.
	var at time.Time
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp() - interval '72 hours'`).Scan(&at); err != nil {
		t.Fatalf("read the database's clock: %v", err)
	}
	st.DBNow = func() (time.Time, error) { return at, nil }
	owner := "window-" + uuid.NewString()
	runID, aps := sendRunWithApprovals(t, st, owner, 2)
	rec := store.MCPCallSend{Owner: owner, OperationScope: "corr:job-7", OperationKey: "step-1", ApprovalID: aps[0], RunID: runID, CallSHA256: "aaa"}
	sent := at
	if first, made, err := st.RecordMCPCallSend(ctx, rec); err != nil || !made || !first.SentAt.Equal(sent) {
		t.Fatalf("first record = %+v, %v, %v; want made at the database's time %s", first, made, err, sent)
	}
	if found, err := st.MarkMCPCallAnswered(ctx, aps[0], runID); err != nil || !found {
		t.Fatalf("stamp = %v, %v; want found", found, err)
	}
	later := rec
	later.ApprovalID, later.CallSHA256 = aps[1], "bbb"
	at = sent.Add(types.MCPCallSendWindow - time.Microsecond)
	if got, err := st.GetMCPCallSend(ctx, owner, rec.OperationScope, rec.OperationKey); err != nil || got.ApprovalID != aps[0] {
		t.Fatalf("read a microsecond inside the window = %+v, %v; want the send", got, err)
	}
	if got, made, err := st.RecordMCPCallSend(ctx, later); err != nil || made || got.ApprovalID != aps[0] || !got.SentAt.Equal(sent) {
		t.Fatalf("record a microsecond inside the window = %+v, %v, %v; want the first send, unchanged", got, made, err)
	}
	at = sent.Add(types.MCPCallSendWindow)
	if got, err := st.GetMCPCallSend(ctx, owner, rec.OperationScope, rec.OperationKey); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("read at exactly 24 hours = %+v, %v; want ErrNotFound", got, err)
	}
	got, made, err := st.RecordMCPCallSend(ctx, later)
	if err != nil || !made || got.ApprovalID != aps[1] || got.CallSHA256 != "bbb" || !got.SentAt.Equal(at) {
		t.Fatalf("record at exactly 24 hours = %+v, %v, %v; want the new send, made at %s", got, made, err, at)
	}
	if cur, err := st.GetMCPCallSend(ctx, owner, rec.OperationScope, rec.OperationKey); err != nil || cur.ApprovalID != aps[1] || cur.AnsweredAt != nil {
		t.Fatalf("after the overwrite = %+v, %v; want the new send, unanswered", cur, err)
	}
	if found, err := st.MarkMCPCallAnswered(ctx, aps[0], runID); err != nil || found {
		t.Fatalf("the overwritten send's stamp = %v, %v; want not found", found, err)
	}
	if found, err := st.MarkMCPCallAnswered(ctx, aps[1], runID); err != nil || !found {
		t.Fatalf("the new send's stamp = %v, %v; want found", found, err)
	}
	st.DBNow = func() (time.Time, error) { return time.Time{}, errors.New("clock read failed") }
	if _, err := st.GetMCPCallSend(ctx, owner, rec.OperationScope, rec.OperationKey); err == nil || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("read without the clock = %v, want the clock's error", err)
	}
	if _, made, err := st.RecordMCPCallSend(ctx, rec); err == nil || made {
		t.Fatalf("record without the clock = %v, %v; want the clock's error", made, err)
	}
}

// TestPG_MCPCallSends_ErasureLeavesNoCorrelationID: erasing a person's run tasks overwrites the send records of
// their runs, a send still inside its 24 hours included, so no row keeps the correlation ID, the key or the call's
// hash and none is deleted; the same key in that namespace is then new (it needs an approval again), and another
// person's send of the same key is untouched.
func TestPG_MCPCallSends_ErasureLeavesNoCorrelationID(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	owner, other := "erase-sends-"+uuid.NewString(), "keep-sends-"+uuid.NewString()
	corrID := "payroll-" + uuid.NewString()
	scope, key := "corr:"+corrID, "refund-for-a-named-person"
	runID, aps := sendRunWithApprovals(t, st, owner, 3)
	otherRun, otherAps := sendRunWithApprovals(t, st, other, 1)
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET correlation_id = $2 WHERE id = $1`, runID, corrID); err != nil {
		t.Fatal(err)
	}
	for _, rec := range []store.MCPCallSend{
		{Owner: owner, OperationScope: scope, OperationKey: key, ApprovalID: aps[0], RunID: runID, CallSHA256: "aaa"},
		{Owner: owner, OperationScope: "run:" + runID.String(), OperationKey: "step-1", ApprovalID: aps[1], RunID: runID, CallSHA256: "bbb"},
		{Owner: other, OperationScope: scope, OperationKey: key, ApprovalID: otherAps[0], RunID: otherRun, CallSHA256: "aaa"},
	} {
		if _, made, err := st.RecordMCPCallSend(ctx, rec); err != nil || !made {
			t.Fatalf("record %+v = %v, %v; want made", rec, made, err)
		}
	}
	if n, err := st.BlankRunTasks(ctx, owner); err != nil || n != 1 {
		t.Fatalf("BlankRunTasks = %d, %v, want her one run", n, err)
	}
	var rows, named int
	if err := pool.QueryRow(ctx, `SELECT count(*),
			count(*) FILTER (WHERE operation_scope NOT LIKE 'erased:%' OR operation_key <> '' OR call_sha256 <> '')
		FROM mcp_call_sends WHERE run_id = $1`, runID).Scan(&rows, &named); err != nil || rows != 2 || named != 0 {
		t.Fatalf("her run's send records after erasure: %d rows, %d still naming a scope, key or hash (%v); want 2, 0", rows, named, err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM mcp_call_sends WHERE owner = $1 AND operation_scope = $2`,
		owner, scope).Scan(&left); err != nil || left != 0 {
		t.Fatalf("rows of hers holding the correlation ID = %d (%v), want 0", left, err)
	}
	if got, err := st.GetMCPCallSend(ctx, other, scope, key); err != nil || got.ApprovalID != otherAps[0] || got.CallSHA256 != "aaa" {
		t.Fatalf("another person's send = %+v, %v; want it untouched", got, err)
	}
	if _, err := st.GetMCPCallSend(ctx, owner, scope, key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("her key read inside the 24 hours, after erasure = %v; want ErrNotFound, so the call needs an approval again", err)
	}
	again := store.MCPCallSend{Owner: owner, OperationScope: scope, OperationKey: key, ApprovalID: aps[2], RunID: runID, CallSHA256: "aaa"}
	if got, made, err := st.RecordMCPCallSend(ctx, again); err != nil || !made || got.ApprovalID != aps[2] {
		t.Fatalf("her key's next record = %+v, %v, %v; want a new send", got, made, err)
	}
}
