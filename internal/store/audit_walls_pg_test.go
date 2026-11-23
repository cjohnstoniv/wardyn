// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// auditWallsPGActions is the three egress actions the Walls view reads.
var auditWallsPGActions = []string{"egress.allow", "egress.deny", "egress.hold"}

// auditWallsSeed appends one audit row for run (nil for none) with data, through the store's insert.
func auditWallsSeed(t *testing.T, ctx context.Context, pool *pgxpool.Pool, run *uuid.UUID, action string, data json.RawMessage) types.AuditEvent {
	t.Helper()
	ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), RunID: run, ActorType: types.ActorAgent,
		Actor: "spiffe://wardyn.test/agent-run/seed", Action: action, Outcome: "success", Data: data}
	if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
		t.Fatalf("seed %s: %v", action, err)
	}
	return ev
}

// auditWallsHostData is a decodable row's data for host.
func auditWallsHostData(host string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"host": host, "port": 443, "rule_source": "policy", "approval_id": nil})
	return raw
}

// auditWallsBadData is data DecodeDecisionRow refuses: a port that is not a number.
var auditWallsBadData = json.RawMessage(`{"host":"bad.example","port":"443"}`)

// auditWallsRun persists one run in state, frozen to unit (nil for none), and returns its id.
func auditWallsRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, state types.RunState, unit *uuid.UUID) uuid.UUID {
	t.Helper()
	r := newRun(state)
	r.UnitID = unit
	return persistRun(t, ctx, pool, r).ID
}

// auditWallsSince is a lower bound on every row seeded after it: an hour back, so a clock skew between this
// process and the database cannot drop a row.
func auditWallsSince() time.Time { return time.Now().UTC().Add(-time.Hour) }

// Rows come back in seq order, oldest first, with their seq and recorded_at, other actions left out, and the
// cursor is the last row's seq.
func TestAuditWallsRowsSeqOrder_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	since := auditWallsSince()
	run := auditWallsRun(t, ctx, pool, types.RunRunning, nil)
	var want []uuid.UUID
	for i := range 6 {
		ev := auditWallsSeed(t, ctx, pool, &run, auditWallsPGActions[i%3], auditWallsHostData("h.example"))
		want = append(want, ev.ID)
		auditWallsSeed(t, ctx, pool, &run, "run.create", nil)
	}
	rows, cur, err := pg.AuditWallsRows(ctx, store.DecisionQuery{Actions: auditWallsPGActions, Since: since, RunIDs: []uuid.UUID{run}, Limit: 100})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rows) != len(want) {
		t.Fatalf("read %d rows, want the %d egress rows", len(rows), len(want))
	}
	for i, r := range rows {
		if r.ID != want[i] {
			t.Errorf("row %d is %s, want %s: not in seq order", i, r.ID, want[i])
		}
		if i > 0 && (r.Seq <= rows[i-1].Seq || r.RecordedAt.Before(rows[i-1].RecordedAt)) {
			t.Errorf("row %d seq %d recorded %v after row %d seq %d recorded %v", i, r.Seq, r.RecordedAt, i-1, rows[i-1].Seq, rows[i-1].RecordedAt)
		}
		if r.RunID != run || r.Host != "h.example" || r.Format != 1 {
			t.Errorf("row %d decoded as %+v", i, r)
		}
	}
	if cur.Seq != rows[len(rows)-1].Seq {
		t.Errorf("cursor %d, want the last row's seq %d", cur.Seq, rows[len(rows)-1].Seq)
	}
}

// The cursor is the last row scanned, decoded or not: a page holding only rows that do not decode returns no
// row and still moves it, and a read past the end returns the cursor it was sent.
func TestAuditWallsRowsCursor_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	since := auditWallsSince()
	run := auditWallsRun(t, ctx, pool, types.RunRunning, nil)
	var good []uuid.UUID
	for range 3 {
		good = append(good, auditWallsSeed(t, ctx, pool, &run, "egress.allow", auditWallsHostData("a.example")).ID)
	}
	for range 3 {
		auditWallsSeed(t, ctx, pool, &run, "egress.deny", auditWallsBadData)
	}
	for range 2 {
		good = append(good, auditWallsSeed(t, ctx, pool, &run, "egress.allow", auditWallsHostData("b.example")).ID)
	}
	q := store.DecisionQuery{Actions: auditWallsPGActions, Since: since, RunIDs: []uuid.UUID{run}, Limit: 3}
	var got []uuid.UUID
	var sizes []int
	for range 6 {
		rows, next, err := pg.AuditWallsRows(ctx, q)
		if err != nil {
			t.Fatalf("read after %d: %v", q.After.Seq, err)
		}
		sizes = append(sizes, len(rows))
		for _, r := range rows {
			got = append(got, r.ID)
		}
		if next == q.After {
			break
		}
		if next.Seq <= q.After.Seq {
			t.Fatalf("cursor went from %d to %d", q.After.Seq, next.Seq)
		}
		q.After = next
	}
	if len(sizes) != 4 || sizes[0] != 3 || sizes[1] != 0 || sizes[2] != 2 || sizes[3] != 0 {
		t.Errorf("page sizes %v, want [3 0 2 0]: the page of undecodable rows must move the cursor", sizes)
	}
	if len(got) != len(good) {
		t.Fatalf("read %d decodable rows, want %d", len(got), len(good))
	}
	for i := range good {
		if got[i] != good[i] {
			t.Errorf("row %d is %s, want %s", i, got[i], good[i])
		}
	}
}

// RunIDs narrows the read to those runs; an empty list reads every run, a row with no run included.
func TestAuditWallsRowsRunIDs_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	since := auditWallsSince()
	r1 := auditWallsRun(t, ctx, pool, types.RunRunning, nil)
	r2 := auditWallsRun(t, ctx, pool, types.RunRunning, nil)
	mine := auditWallsSeed(t, ctx, pool, &r1, "egress.allow", auditWallsHostData("one.example")).ID
	auditWallsSeed(t, ctx, pool, &r2, "egress.allow", auditWallsHostData("two.example"))
	auditWallsSeed(t, ctx, pool, nil, "egress.deny", auditWallsHostData("none.example"))

	rows, _, err := pg.AuditWallsRows(ctx, store.DecisionQuery{Actions: auditWallsPGActions, Since: since, RunIDs: []uuid.UUID{r1}, Limit: 10})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != mine {
		t.Errorf("RunIDs [r1] read %d rows, want only r1's", len(rows))
	}
	all, _, err := pg.AuditWallsRows(ctx, store.DecisionQuery{Actions: auditWallsPGActions, Since: since, Limit: 10})
	if err != nil {
		t.Fatalf("read all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("no RunIDs read %d rows, want all 3", len(all))
	}
}

// Asking for egress.hold also reads the older egress.pending rows, which decode as egress.hold.
func TestAuditWallsRowsPendingFold_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	since := auditWallsSince()
	run := auditWallsRun(t, ctx, pool, types.RunRunning, nil)
	auditWallsSeed(t, ctx, pool, &run, "egress.pending", auditWallsHostData("old.example"))
	auditWallsSeed(t, ctx, pool, &run, "egress.hold", auditWallsHostData("new.example"))
	rows, _, err := pg.AuditWallsRows(ctx, store.DecisionQuery{Actions: []string{"egress.hold"}, Since: since, RunIDs: []uuid.UUID{run}, Limit: 10})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("hold read %d rows, want the pending and the hold row", len(rows))
	}
	for _, r := range rows {
		if r.Action != "egress.hold" {
			t.Errorf("row %s has action %q, want egress.hold", r.Host, r.Action)
		}
	}
	allows, _, err := pg.AuditWallsRows(ctx, store.DecisionQuery{Actions: []string{"egress.allow"}, Since: since, RunIDs: []uuid.UUID{run}, Limit: 10})
	if err != nil || len(allows) != 0 {
		t.Errorf("allow read gave %d rows (%v), want none", len(allows), err)
	}
}

// A limit outside 1..5000 is refused; a limit below the row count reads that many rows.
func TestAuditWallsRowsLimit_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	since := auditWallsSince()
	run := auditWallsRun(t, ctx, pool, types.RunRunning, nil)
	for range 5 {
		auditWallsSeed(t, ctx, pool, &run, "egress.allow", auditWallsHostData("l.example"))
	}
	for _, bad := range []int{0, -1, 5001} {
		if _, cur, err := pg.AuditWallsRows(ctx, store.DecisionQuery{Actions: auditWallsPGActions, Since: since, Limit: bad, After: store.DecisionCursor{Seq: 7}}); err == nil || cur.Seq != 7 {
			t.Errorf("limit %d: err %v, cursor %d; want an error and the cursor sent", bad, err, cur.Seq)
		}
	}
	rows, _, err := pg.AuditWallsRows(ctx, store.DecisionQuery{Actions: auditWallsPGActions, Since: since, RunIDs: []uuid.UUID{run}, Limit: 2})
	if err != nil || len(rows) != 2 {
		t.Errorf("limit 2 read %d rows (%v), want 2", len(rows), err)
	}
	if _, _, err := pg.AuditWallsRows(ctx, store.DecisionQuery{Actions: auditWallsPGActions, Since: since, Limit: 5000}); err != nil {
		t.Errorf("limit 5000: %v", err)
	}
}

// auditWallsCountKey names one AuditWallsCounts group.
type auditWallsCountKey struct {
	action  string
	inScope bool
}

func auditWallsCountMap(t *testing.T, got []store.WallsCount) map[auditWallsCountKey]store.WallsCount {
	t.Helper()
	out := map[auditWallsCountKey]store.WallsCount{}
	for _, c := range got {
		k := auditWallsCountKey{c.Action, c.InScope}
		if _, dup := out[k]; dup {
			t.Errorf("group %+v returned twice", k)
		}
		out[k] = c
	}
	return out
}

// The counts group the newest rows by action, pending folded into hold, and by whether the row's run is in
// one of the units; a row with no run, or whose run has no unit, counts out of scope. The all-actions group
// carries Action "". Live counts the distinct runs whose state is not terminal.
func TestAuditWallsCounts_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	since := auditWallsSince()
	u1, u2 := uuid.New(), uuid.New()
	rIn := auditWallsRun(t, ctx, pool, types.RunRunning, &u1)
	rInDone := auditWallsRun(t, ctx, pool, types.RunCompleted, &u1)
	rOut := auditWallsRun(t, ctx, pool, types.RunRunning, &u2)
	rNull := auditWallsRun(t, ctx, pool, types.RunRunning, nil)
	data := auditWallsHostData("c.example")
	auditWallsSeed(t, ctx, pool, &rIn, "egress.allow", data)
	auditWallsSeed(t, ctx, pool, &rIn, "egress.allow", data)
	auditWallsSeed(t, ctx, pool, &rIn, "egress.deny", data)
	auditWallsSeed(t, ctx, pool, &rIn, "run.create", nil)
	auditWallsSeed(t, ctx, pool, &rInDone, "egress.allow", data)
	for range 3 {
		auditWallsSeed(t, ctx, pool, &rOut, "egress.allow", data)
	}
	auditWallsSeed(t, ctx, pool, &rOut, "egress.pending", data)
	auditWallsSeed(t, ctx, pool, &rNull, "egress.deny", data)
	auditWallsSeed(t, ctx, pool, nil, "egress.allow", data)

	got, err := pg.AuditWallsCounts(ctx, auditWallsPGActions, since, []uuid.UUID{u1}, 1000)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	want := map[auditWallsCountKey]store.WallsCount{
		{"egress.allow", true}:  {Action: "egress.allow", InScope: true, Rows: 3, Runs: 2, Live: 1},
		{"egress.deny", true}:   {Action: "egress.deny", InScope: true, Rows: 1, Runs: 1, Live: 1},
		{"", true}:              {Action: "", InScope: true, Rows: 4, Runs: 2, Live: 1},
		{"egress.allow", false}: {Action: "egress.allow", InScope: false, Rows: 4, Runs: 1, Live: 1},
		{"egress.hold", false}:  {Action: "egress.hold", InScope: false, Rows: 1, Runs: 1, Live: 1},
		{"egress.deny", false}:  {Action: "egress.deny", InScope: false, Rows: 1, Runs: 1, Live: 1},
		{"", false}:             {Action: "", InScope: false, Rows: 6, Runs: 2, Live: 2},
	}
	gotMap := auditWallsCountMap(t, got)
	if len(gotMap) != len(want) {
		t.Errorf("%d groups, want %d: %+v", len(gotMap), len(want), got)
	}
	for k, w := range want {
		if g, ok := gotMap[k]; !ok || g != w {
			t.Errorf("group %+v = %+v (present %v), want %+v", k, g, ok, w)
		}
	}

	t.Run("a limit below the row count counts only the newest rows", func(t *testing.T) {
		got, err := pg.AuditWallsCounts(ctx, auditWallsPGActions, since, []uuid.UUID{u1}, 2)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		want := map[auditWallsCountKey]store.WallsCount{
			{"egress.deny", false}:  {Action: "egress.deny", InScope: false, Rows: 1, Runs: 1, Live: 1},
			{"egress.allow", false}: {Action: "egress.allow", InScope: false, Rows: 1, Runs: 0, Live: 0},
			{"", false}:             {Action: "", InScope: false, Rows: 2, Runs: 1, Live: 1},
		}
		gotMap := auditWallsCountMap(t, got)
		if len(gotMap) != len(want) {
			t.Errorf("%d groups, want %d: %+v", len(gotMap), len(want), got)
		}
		for k, w := range want {
			if g, ok := gotMap[k]; !ok || g != w {
				t.Errorf("group %+v = %+v (present %v), want %+v", k, g, ok, w)
			}
		}
	})
	t.Run("a limit outside 1..250000 is refused", func(t *testing.T) {
		for _, bad := range []int{0, 250001} {
			if _, err := pg.AuditWallsCounts(ctx, auditWallsPGActions, since, []uuid.UUID{u1}, bad); err == nil {
				t.Errorf("limit %d: no error", bad)
			}
		}
	})
}

// Each run's first perRun allow rows, in seq order, from its own since: deny rows and earlier rows are left out,
// a run with none is absent, and arrays of unequal length are refused.
func TestAuditWallsFirstAllows_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	since := auditWallsSince()
	rA := auditWallsRun(t, ctx, pool, types.RunRunning, nil)
	rB := auditWallsRun(t, ctx, pool, types.RunRunning, nil)
	rC := auditWallsRun(t, ctx, pool, types.RunRunning, nil)
	auditWallsSeed(t, ctx, pool, &rA, "egress.deny", auditWallsHostData("deny.example"))
	var wantA []uuid.UUID
	for range 3 {
		wantA = append(wantA, auditWallsSeed(t, ctx, pool, &rA, "egress.allow", auditWallsHostData("a.example")).ID)
	}
	early := auditWallsSeed(t, ctx, pool, &rB, "egress.allow", auditWallsHostData("early.example"))
	var earlyAt time.Time
	if err := pool.QueryRow(ctx, `SELECT recorded_at FROM audit_events WHERE id = $1`, early.ID).Scan(&earlyAt); err != nil {
		t.Fatalf("read the early row's recorded_at: %v", err)
	}
	var wantB []uuid.UUID
	for range 2 {
		wantB = append(wantB, auditWallsSeed(t, ctx, pool, &rB, "egress.allow", auditWallsHostData("b.example")).ID)
	}
	auditWallsSeed(t, ctx, pool, &rC, "egress.deny", auditWallsHostData("c.example"))

	rows, err := pg.AuditWallsFirstAllows(ctx, []uuid.UUID{rA, rB, rC}, []time.Time{since, earlyAt.Add(time.Microsecond), since}, 2)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := []uuid.UUID{wantA[0], wantA[1], wantB[0], wantB[1]}
	if len(rows) != len(want) {
		t.Fatalf("read %d rows, want %d", len(rows), len(want))
	}
	for i, r := range rows {
		if r.ID != want[i] || r.Action != "egress.allow" {
			t.Errorf("row %d is %s %s, want %s", i, r.ID, r.Action, want[i])
		}
	}
	for _, bad := range []struct {
		name   string
		ids    []uuid.UUID
		since  []time.Time
		perRun int
	}{
		{"unequal arrays", []uuid.UUID{rA, rB}, []time.Time{since}, 2},
		{"perRun 0", []uuid.UUID{rA}, []time.Time{since}, 0},
		{"perRun 65", []uuid.UUID{rA}, []time.Time{since}, 65},
		{"5001 runs", make([]uuid.UUID, 5001), make([]time.Time, 5001), 1},
	} {
		if _, err := pg.AuditWallsFirstAllows(ctx, bad.ids, bad.since, bad.perRun); err == nil {
			t.Errorf("%s: no error", bad.name)
		}
	}
}

// auditWallsTracer keeps the SQL and arguments of every statement a pool runs, so a test can EXPLAIN the
// statements an accessor sent without a copy of their text.
type auditWallsTracer struct {
	mu    sync.Mutex
	stmts []pgx.TraceQueryStartData
}

func (tr *auditWallsTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.stmts = append(tr.stmts, d)
	return ctx
}

func (tr *auditWallsTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// auditWallsTraced is a second pool on pool's database whose statements tr records.
func auditWallsTraced(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tr *auditWallsTracer) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(pool.Config().ConnString())
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	cfg.ConnConfig.Tracer = tr
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(traced.Close)
	return traced
}

// auditWallsExplain logs EXPLAIN (ANALYZE, BUFFERS) of every statement tr recorded, under label.
func auditWallsExplain(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tr *auditWallsTracer, label string) {
	t.Helper()
	tr.mu.Lock()
	stmts := append([]pgx.TraceQueryStartData(nil), tr.stmts...)
	tr.stmts = nil
	tr.mu.Unlock()
	if len(stmts) == 0 {
		t.Fatalf("%s: no statement was recorded", label)
	}
	for i, st := range stmts {
		rows, err := pool.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+st.SQL, st.Args...)
		if err != nil {
			t.Fatalf("%s statement %d: explain: %v", label, i+1, err)
		}
		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("%s statement %d: explain rows: %v", label, i+1, err)
		}
		t.Logf("EXPLAIN %s statement %d:\n%s\n%s", label, i+1, strings.TrimSpace(st.SQL), strings.Join(lines, "\n"))
	}
}

// The plans of the three accessor statements on a seeded table, printed for the record: a sequential scan of
// every audit partition is reported, not fixed here.
func TestAuditWallsRowsExplain_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	since := auditWallsSince()
	unit := uuid.New()
	var runs []uuid.UUID
	for i := range 20 {
		run := auditWallsRun(t, ctx, pool, types.RunRunning, &unit)
		runs = append(runs, run)
		for j := range 10 {
			auditWallsSeed(t, ctx, pool, &run, auditWallsPGActions[(i+j)%3], auditWallsHostData("x.example"))
		}
	}
	tr := &auditWallsTracer{}
	pg := store.NewPG(auditWallsTraced(t, ctx, pool, tr))
	if _, _, err := pg.AuditWallsRows(ctx, store.DecisionQuery{Actions: auditWallsPGActions, Since: since, RunIDs: runs, Limit: 5000}); err != nil {
		t.Fatalf("rows: %v", err)
	}
	auditWallsExplain(t, ctx, pool, tr, "AuditWallsRows")
	if _, err := pg.AuditWallsCounts(ctx, auditWallsPGActions, since, []uuid.UUID{unit}, 250000); err != nil {
		t.Fatalf("counts: %v", err)
	}
	auditWallsExplain(t, ctx, pool, tr, "AuditWallsCounts")
	sinces := make([]time.Time, len(runs))
	for i := range sinces {
		sinces[i] = since
	}
	if _, err := pg.AuditWallsFirstAllows(ctx, runs, sinces, 64); err != nil {
		t.Fatalf("first allows: %v", err)
	}
	auditWallsExplain(t, ctx, pool, tr, "AuditWallsFirstAllows")
}
