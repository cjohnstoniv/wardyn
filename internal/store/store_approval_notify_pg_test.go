// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/notify"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// activateNotify installs a two-channel notify config for the test.
func activateNotify(t *testing.T) {
	t.Helper()
	cfg, err := notify.Parse(`{"channels":[
		{"id":"hook-a","type":"webhook","url":"http://127.0.0.1:9/a"},
		{"id":"hook-b","type":"webhook","url":"http://127.0.0.1:9/b"}]}`)
	if err != nil {
		t.Fatalf("parse notify config: %v", err)
	}
	notify.SetActive(cfg, nil)
	t.Cleanup(func() { notify.SetActive(nil, nil) })
}

func pendingApproval(runID uuid.UUID, kind types.ApprovalKind, scope string) types.ApprovalRequest {
	return types.ApprovalRequest{
		ID: uuid.New(), RunID: runID, Kind: kind, RequestedScope: json.RawMessage(scope),
		State: types.ApprovalPending, RequestedAt: time.Now().UTC(),
	}
}

func countRows(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

const outboxForRun = `SELECT count(*) FROM approval_notifications n JOIN approvals a ON a.id = n.approval_id WHERE a.run_id = $1`

// TestPG_CreateApproval_EveryKindWritesItsOutboxRows: every approval kind gets one tier-0 pending row per
// configured channel, written by the same transaction as the approval.
func TestPG_CreateApproval_EveryKindWritesItsOutboxRows(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	activateNotify(t)
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	st := store.NewPG(pool)
	for i, kind := range types.ApprovalKinds {
		a, err := st.CreateApproval(ctx, pendingApproval(run.ID, kind, `{"n":`+string(rune('0'+i))+`}`))
		if err != nil {
			t.Fatalf("%s: CreateApproval: %v", kind, err)
		}
		rows := countRows(t, pool, `SELECT count(*) FROM approval_notifications WHERE approval_id = $1 AND tier = 0 AND state = 'pending'`, a.ID)
		if rows != 2 {
			t.Errorf("%s: %d pending tier-0 rows, want 2 (one per channel)", kind, rows)
		}
	}
}

// TestPG_CreateApproval_NotifyOffWritesNoRows pins the off-by-default contract at the store seam.
func TestPG_CreateApproval_NotifyOffWritesNoRows(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	notify.SetActive(nil, nil)
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	if _, err := store.NewPG(pool).CreateApproval(ctx, pendingApproval(run.ID, types.ApprovalEgressDomain, `{"host":"a.example"}`)); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, pool, outboxForRun, run.ID); n != 0 {
		t.Fatalf("%d outbox rows with notifications off, want 0", n)
	}
}

// TestPG_CreateApproval_RunBudget: the 26th raise on one run inside an hour enqueues nothing and
// counts as suppressed, yet the approval exists; a raise after the window enqueues again.
func TestPG_CreateApproval_RunBudget(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	cfg, err := notify.Parse(`{"channels":[{"id":"solo","type":"webhook","url":"http://127.0.0.1:9/a"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	notify.SetActive(cfg, nil)
	t.Cleanup(func() { notify.SetActive(nil, nil) })
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	st := store.NewPG(pool)
	raise := func(i int) types.ApprovalRequest {
		a, err := st.CreateApproval(ctx, pendingApproval(run.ID, types.ApprovalEgressDomain, `{"host":"h`+uuid.NewString()+`"}`))
		if err != nil {
			t.Fatalf("raise %d: %v", i, err)
		}
		return a
	}
	before := notify.Snapshot().Suppressed["solo"]
	for i := range notify.RunBudget {
		raise(i)
	}
	over := raise(notify.RunBudget)
	if n := countRows(t, pool, outboxForRun, run.ID); n != notify.RunBudget {
		t.Fatalf("outbox rows = %d after %d raises, want %d: the 26th must enqueue nothing", n, notify.RunBudget+1, notify.RunBudget)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM approvals WHERE id = $1`, over.ID); n != 1 {
		t.Fatalf("the suppressed raise must still create its approval, found %d", n)
	}
	if got := notify.Snapshot().Suppressed["solo"]; got != before+1 {
		t.Fatalf("suppressed counter = %d, want %d", got, before+1)
	}
	if _, err := pool.Exec(ctx, `UPDATE approval_notifications SET created_at = now() - interval '2 hours'
		WHERE approval_id IN (SELECT id FROM approvals WHERE run_id = $1)`, run.ID); err != nil {
		t.Fatal(err)
	}
	later := raise(notify.RunBudget + 1)
	if n := countRows(t, pool, `SELECT count(*) FROM approval_notifications WHERE approval_id = $1`, later.ID); n != 1 {
		t.Fatalf("a raise after the window wrote %d rows, want 1", n)
	}
}

// TestPG_CreateApproval_ConcurrentDuplicateRaisesLeaveOneOutboxSet: racing raises of one run+kind+scope
// leave one approval and one outbox set (the loser's transaction rolls back whole).
func TestPG_CreateApproval_ConcurrentDuplicateRaisesLeaveOneOutboxSet(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	activateNotify(t)
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	st := approvalStore{store.NewPG(pool)}
	const racers = 12
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := approval.RequestApproval(ctx, st, types.ApprovalRequest{
				RunID: run.ID, Kind: types.ApprovalEgressDomain, RequestedScope: json.RawMessage(`{"host":"dup.example"}`),
			}); err != nil {
				t.Errorf("RequestApproval: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if n := countRows(t, pool, `SELECT count(*) FROM approvals WHERE run_id = $1`, run.ID); n != 1 {
		t.Fatalf("approvals = %d, want 1", n)
	}
	if n := countRows(t, pool, outboxForRun, run.ID); n != 2 {
		t.Fatalf("outbox rows = %d, want 2 (one set for one approval)", n)
	}
}

// TestPG_CreateApproval_DuplicateStillMapsToErrDuplicatePending: the transaction does not change the
// 23505 mapping approval.RequestApproval's dedup path depends on.
func TestPG_CreateApproval_DuplicateStillMapsToErrDuplicatePending(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	activateNotify(t)
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	st := store.NewPG(pool)
	if _, err := st.CreateApproval(ctx, pendingApproval(run.ID, types.ApprovalEgressDomain, `{"host":"x.example"}`)); err != nil {
		t.Fatal(err)
	}
	_, err := st.CreateApproval(ctx, pendingApproval(run.ID, types.ApprovalEgressDomain, `{"host":"x.example"}`))
	if !errors.Is(err, store.ErrDuplicatePending) {
		t.Fatalf("duplicate raise error = %v, want ErrDuplicatePending", err)
	}
	if n := countRows(t, pool, outboxForRun, run.ID); n != 2 {
		t.Fatalf("outbox rows = %d after a duplicate, want 2 (the loser enqueued nothing)", n)
	}
}

// TestPG_CreateApproval_InjectedFailuresLeaveNeitherRow: a failure at the approval write, at the outbox
// write, or at COMMIT leaves no approval and no outbox row. Run in a throwaway database because the
// injected triggers would break every other test sharing one.
func TestPG_CreateApproval_InjectedFailuresLeaveNeitherRow(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	activateNotify(t)
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	st := store.NewPG(pool)
	exec := func(q string) {
		t.Helper()
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`CREATE FUNCTION boom() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected'; END $$`)
	cases := []struct{ name, create, drop string }{
		{"approval write", `CREATE TRIGGER inj BEFORE INSERT ON approvals FOR EACH ROW EXECUTE FUNCTION boom()`, `DROP TRIGGER inj ON approvals`},
		{"outbox write", `CREATE TRIGGER inj BEFORE INSERT ON approval_notifications FOR EACH ROW EXECUTE FUNCTION boom()`, `DROP TRIGGER inj ON approval_notifications`},
		// A deferred constraint trigger fires at COMMIT: the nearest a test can get to a crash there.
		{"commit", `CREATE CONSTRAINT TRIGGER inj AFTER INSERT ON approval_notifications DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION boom()`, `DROP TRIGGER inj ON approval_notifications`},
	}
	for _, c := range cases {
		exec(c.create)
		if _, err := st.CreateApproval(ctx, pendingApproval(run.ID, types.ApprovalEgressDomain, `{"host":"fail.example"}`)); err == nil {
			t.Fatalf("%s: CreateApproval succeeded despite the injected failure", c.name)
		}
		exec(c.drop)
		if n := countRows(t, pool, `SELECT count(*) FROM approvals WHERE run_id = $1`, run.ID); n != 0 {
			t.Errorf("%s: %d approval rows survived, want 0", c.name, n)
		}
		if n := countRows(t, pool, `SELECT count(*) FROM approval_notifications`); n != 0 {
			t.Errorf("%s: %d outbox rows survived, want 0", c.name, n)
		}
	}
}

// TestPG_CreateApproval_RoutedTiersAreAllEnqueuedWithTheirDueAt: a matching route enqueues every tier's
// rows at insert, due `after` past requested_at; a kind no route matches enqueues nothing.
func TestPG_CreateApproval_RoutedTiersAreAllEnqueuedWithTheirDueAt(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	cfg, err := notify.Parse(`{"channels":[
		{"id":"hook-a","type":"webhook","url":"http://127.0.0.1:9/a"},
		{"id":"hook-b","type":"webhook","url":"http://127.0.0.1:9/b"}],
		"routes":[{"kinds":["push_content"],"tiers":[
			{"after":"0s","channels":["hook-a"]},
			{"after":"10m","channels":["hook-a","hook-b"]},
			{"after":"90m","channels":["hook-b"]}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	notify.SetActive(cfg, nil)
	t.Cleanup(func() { notify.SetActive(nil, nil) })
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	st := store.NewPG(pool)

	a, err := st.CreateApproval(ctx, pendingApproval(run.ID, types.ApprovalPushContent, `{"n":1}`))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `
		SELECT n.tier, n.channel, extract(epoch FROM n.due_at - a.requested_at), n.next_attempt_at = n.due_at
		  FROM approval_notifications n JOIN approvals a ON a.id = n.approval_id
		 WHERE n.approval_id = $1 ORDER BY n.tier, n.channel`, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type row struct {
		tier   int
		ch     string
		secs   int
		synced bool
	}
	var got []row
	for rows.Next() {
		var r row
		var secs float64
		if err := rows.Scan(&r.tier, &r.ch, &secs, &r.synced); err != nil {
			t.Fatal(err)
		}
		r.secs = int(secs + 0.5)
		got = append(got, r)
	}
	want := []row{{0, "hook-a", 0, true}, {1, "hook-a", 600, true}, {1, "hook-b", 600, true}, {2, "hook-b", 5400, true}}
	if len(got) != len(want) {
		t.Fatalf("rows = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	other, err := st.CreateApproval(ctx, pendingApproval(run.ID, types.ApprovalEgressDomain, `{"n":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM approval_notifications WHERE approval_id = $1`, other.ID); n != 0 {
		t.Fatalf("an unrouted kind got %d outbox rows, want 0", n)
	}
}

// budgetAudit records what the notify package audits, for the suppression row.
type budgetAudit struct {
	mu  sync.Mutex
	evs []types.AuditEvent
}

func (a *budgetAudit) Record(_ context.Context, ev types.AuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.evs = append(a.evs, ev)
	return nil
}

func (a *budgetAudit) count(action string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, ev := range a.evs {
		if ev.Action == action {
			n++
		}
	}
	return n
}

// holdBudgetSnapshot makes every outbox insert for channel sleep inside its INSERT, after the statement
// took its snapshot and counted the run's rows and before it commits: the instant two concurrent raises
// would both read the same count if nothing serialised them.
func holdBudgetSnapshot(t *testing.T, pool *pgxpool.Pool, channel string) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`CREATE OR REPLACE FUNCTION notify_budget_barrier() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN IF NEW.channel = '` + channel + `' THEN PERFORM pg_sleep(0.6); END IF; RETURN NEW; END $$`,
		`CREATE TRIGGER notify_budget_barrier BEFORE INSERT ON approval_notifications FOR EACH ROW EXECUTE FUNCTION notify_budget_barrier()`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS notify_budget_barrier ON approval_notifications`)
	})
}

// TestPG_CreateApproval_ConcurrentDistinctRaisesStayWithinTheRunBudget: a run with RunBudget-1 tier-0 rows
// and two distinct raises at once. Both read RunBudget-1 under Read Committed, so unserialised both would
// enqueue and the run would carry RunBudget+1 rows. The per-run lock makes the second wait for the first's
// commit and count again: it ends at RunBudget rows, the loser's approval exists, and the suppression is
// audited once.
func TestPG_CreateApproval_ConcurrentDistinctRaisesStayWithinTheRunBudget(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	cfg, err := notify.Parse(`{"channels":[{"id":"budget-race","type":"webhook","url":"http://127.0.0.1:9/a"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	rec := &budgetAudit{}
	notify.SetActive(cfg, rec)
	t.Cleanup(func() { notify.SetActive(nil, nil) })
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	st := store.NewPG(pool)
	raise := func() (types.ApprovalRequest, error) {
		return st.CreateApproval(ctx, pendingApproval(run.ID, types.ApprovalEgressDomain, `{"host":"h`+uuid.NewString()+`"}`))
	}
	for i := range notify.RunBudget - 1 {
		if _, err := raise(); err != nil {
			t.Fatalf("seed raise %d: %v", i, err)
		}
	}
	holdBudgetSnapshot(t, pool, "budget-race")

	var wg sync.WaitGroup
	start := make(chan struct{})
	ids := make([]uuid.UUID, 2)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			a, err := raise()
			if err != nil {
				t.Errorf("concurrent raise: %v", err)
			}
			ids[i] = a.ID
		}()
	}
	close(start)
	wg.Wait()

	if n := countRows(t, pool, outboxForRun, run.ID); n != notify.RunBudget {
		t.Fatalf("outbox rows = %d after two concurrent raises on %d, want %d", n, notify.RunBudget-1, notify.RunBudget)
	}
	for _, id := range ids {
		if n := countRows(t, pool, `SELECT count(*) FROM approvals WHERE id = $1`, id); n != 1 {
			t.Errorf("approval %s: %d rows, want 1: an approval is never lost to the budget", id, n)
		}
	}
	if got := rec.count("approval.notify.suppressed"); got != 1 {
		t.Errorf("approval.notify.suppressed audited %d times, want 1", got)
	}
}
