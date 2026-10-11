// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/notify"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// activateNotify installs a one-channel notify config for the test and removes it afterwards.
func activateNotify(t *testing.T) {
	t.Helper()
	cfg, err := notify.Parse(`{"channels":[{"id":"hook","type":"webhook","url":"http://127.0.0.1:9/x"}]}`)
	if err != nil {
		t.Fatalf("parse notify config: %v", err)
	}
	notify.SetActive(cfg, nil)
	t.Cleanup(func() { notify.SetActive(nil, nil) })
}

func outboxRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) int {
	t.Helper()
	var n int
	err := pool.QueryRow(ctx, `SELECT count(*) FROM approval_notifications n JOIN approvals a ON a.id = n.approval_id WHERE a.run_id = $1`, runID).Scan(&n)
	if err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	return n
}

func gatedGrant(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) (uuid.UUID, types.GrantSpec) {
	t.Helper()
	spec := types.GrantSpec{Kind: types.GrantGitHubToken, Scope: pgGithubScope(t), RequiresApproval: true, TTLSeconds: 600}
	return pgSeedGrant(ctx, t, pool, runID, spec), spec
}

// TestPG_EnsureApproval_EnqueuesOutboxInTheApprovalsStatement proves the credential path writes its
// outbox rows through the CTE over the inserted approval: a lost race inserts neither, and a repeat
// call that finds the winner enqueues nothing more.
func TestPG_EnsureApproval_EnqueuesOutboxInTheApprovalsStatement(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()
	activateNotify(t)
	runID := uuid.New()
	seedRun(ctx, t, pool, runID)
	grantID, spec := gatedGrant(ctx, t, pool, runID)
	b := New(NewPgxStore(pool), nil, &fakeAudit{}, nil, nil)

	const racers = 8
	var wg sync.WaitGroup
	ids := make([]uuid.UUID, racers)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ap, err := b.ensureApproval(ctx, grantID, runID, spec)
			if err != nil {
				t.Errorf("ensureApproval: %v", err)
			}
			ids[i] = ap.ID
		}()
	}
	close(start)
	wg.Wait()
	for _, id := range ids[1:] {
		if id != ids[0] {
			t.Fatalf("racers saw different approvals: %v", ids)
		}
	}
	var approvals int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM approvals WHERE run_id = $1`, runID).Scan(&approvals); err != nil {
		t.Fatal(err)
	}
	if approvals != 1 {
		t.Fatalf("approvals = %d, want 1", approvals)
	}
	if rows := outboxRows(ctx, t, pool, runID); rows != 1 {
		t.Fatalf("outbox rows = %d after %d concurrent raises, want exactly 1 (one approval, one channel)", rows, racers)
	}
	var tier int
	var channel, state string
	if err := pool.QueryRow(ctx, `SELECT tier, channel, state FROM approval_notifications WHERE approval_id = $1`, ids[0]).Scan(&tier, &channel, &state); err != nil {
		t.Fatal(err)
	}
	if tier != 0 || channel != "hook" || state != "pending" {
		t.Fatalf("row = tier %d channel %q state %q, want 0 hook pending", tier, channel, state)
	}
}

// TestPG_EnsureApproval_RunBudgetSuppressesThe26thRaise: a run that already has RunBudget tier-0 rows in
// the last hour gets an approval with no outbox row and a suppressed count; once the window passes it
// enqueues again.
func TestPG_EnsureApproval_RunBudgetSuppressesThe26thRaise(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()
	activateNotify(t)
	runID := uuid.New()
	seedRun(ctx, t, pool, runID)
	b := New(NewPgxStore(pool), nil, &fakeAudit{}, nil, nil)

	raise := func() uuid.UUID {
		grantID, spec := gatedGrant(ctx, t, pool, runID)
		ap, err := b.ensureApproval(ctx, grantID, runID, spec)
		if err != nil {
			t.Fatalf("ensureApproval: %v", err)
		}
		return ap.ID
	}
	before := notify.Snapshot().Suppressed["hook"]
	for range notify.RunBudget {
		raise()
	}
	if rows := outboxRows(ctx, t, pool, runID); rows != notify.RunBudget {
		t.Fatalf("outbox rows = %d, want %d", rows, notify.RunBudget)
	}
	over := raise()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM approvals WHERE id = $1`, over).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the suppressed raise must still create its approval: count %d err %v", n, err)
	}
	if rows := outboxRows(ctx, t, pool, runID); rows != notify.RunBudget {
		t.Fatalf("the 26th raise enqueued: outbox rows = %d, want %d", rows, notify.RunBudget)
	}
	if got := notify.Snapshot().Suppressed["hook"]; got != before+1 {
		t.Fatalf("suppressed counter = %d, want %d", got, before+1)
	}

	if _, err := pool.Exec(ctx, `UPDATE approval_notifications SET created_at = now() - interval '2 hours'
		WHERE approval_id IN (SELECT id FROM approvals WHERE run_id = $1)`, runID); err != nil {
		t.Fatal(err)
	}
	after := raise()
	var enq int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM approval_notifications WHERE approval_id = $1`, after).Scan(&enq); err != nil || enq != 1 {
		t.Fatalf("a raise after the window must enqueue again: rows %d err %v", enq, err)
	}
}

// TestPG_EnsureApproval_NotifyOffWritesNoRows: with no config the statement is the old plain INSERT.
func TestPG_EnsureApproval_NotifyOffWritesNoRows(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()
	notify.SetActive(nil, nil)
	runID := uuid.New()
	seedRun(ctx, t, pool, runID)
	grantID, spec := gatedGrant(ctx, t, pool, runID)
	b := New(NewPgxStore(pool), nil, &fakeAudit{}, nil, nil)
	if _, err := b.ensureApproval(ctx, grantID, runID, spec); err != nil {
		t.Fatal(err)
	}
	if rows := outboxRows(ctx, t, pool, runID); rows != 0 {
		t.Fatalf("outbox rows = %d with notifications off, want 0", rows)
	}
}

// TestPG_EnsureApproval_RoutesByTheRunsLeafProfileAndEnqueuesEveryTier: the credential path reads the
// run's profile in the approval's transaction, so a profile route matches and its tiers are all
// enqueued with their due_at; a run on no profile matches no route and enqueues nothing.
func TestPG_EnsureApproval_RoutesByTheRunsLeafProfileAndEnqueuesEveryTier(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()
	profileID := uuid.New()
	cfg, err := notify.Parse(`{"channels":[
		{"id":"hook","type":"webhook","url":"http://127.0.0.1:9/x"},
		{"id":"mail","type":"webhook","url":"http://127.0.0.1:9/y"}],
		"routes":[{"kinds":["credential"],"profiles":["` + profileID.String() + `"],"tiers":[
			{"after":"0s","channels":["hook"]},
			{"after":"45m","channels":["hook","mail"]}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	notify.SetActive(cfg, nil)
	t.Cleanup(func() { notify.SetActive(nil, nil) })
	b := New(NewPgxStore(pool), nil, &fakeAudit{}, nil, nil)

	profiled, plain := uuid.New(), uuid.New()
	seedRun(ctx, t, pool, profiled)
	seedRun(ctx, t, pool, plain)
	if _, err := pool.Exec(ctx, `INSERT INTO governance_profiles (id, name, ceiling) VALUES ($1, $2, '{}')`, profileID, "routed-"+profileID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET governance_profile_id = $1 WHERE id = $2`, profileID, profiled); err != nil {
		t.Fatal(err)
	}

	grantID, spec := gatedGrant(ctx, t, pool, profiled)
	ap, err := b.ensureApproval(ctx, grantID, profiled, spec)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `
		SELECT n.tier, n.channel, round(extract(epoch FROM n.due_at - a.requested_at))::int
		  FROM approval_notifications n JOIN approvals a ON a.id = n.approval_id
		 WHERE n.approval_id = $1 ORDER BY n.tier, n.channel`, ap.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var tier, secs int
		var ch string
		if err := rows.Scan(&tier, &ch, &secs); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%d/%s/%d", tier, ch, secs))
	}
	if want := []string{"0/hook/0", "1/hook/2700", "1/mail/2700"}; !slices.Equal(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}

	grantID, spec = gatedGrant(ctx, t, pool, plain)
	if _, err := b.ensureApproval(ctx, grantID, plain, spec); err != nil {
		t.Fatal(err)
	}
	if n := outboxRows(ctx, t, pool, plain); n != 0 {
		t.Fatalf("a run on no profile got %d outbox rows, want 0", n)
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

// TestPG_EnsureApproval_ConcurrentDistinctRaisesStayWithinTheRunBudget is the credential seam's half of the
// store test of the same name: a run with RunBudget-1 tier-0 rows and two distinct grants raised at once ends
// at RunBudget rows, the loser's approval exists, and the suppression is audited once. The outbox insert of
// the channel "budget-race" sleeps inside its statement, after the count, so the two raises overlap.
func TestPG_EnsureApproval_ConcurrentDistinctRaisesStayWithinTheRunBudget(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()
	cfg, err := notify.Parse(`{"channels":[{"id":"budget-race","type":"webhook","url":"http://127.0.0.1:9/x"}]}`)
	if err != nil {
		t.Fatalf("parse notify config: %v", err)
	}
	rec := &budgetAudit{}
	notify.SetActive(cfg, rec)
	t.Cleanup(func() { notify.SetActive(nil, nil) })
	runID := uuid.New()
	seedRun(ctx, t, pool, runID)
	b := New(NewPgxStore(pool), nil, &fakeAudit{}, nil, nil)
	raise := func() (types.ApprovalRequest, error) {
		grantID, spec := gatedGrant(ctx, t, pool, runID)
		return b.ensureApproval(ctx, grantID, runID, spec)
	}
	for i := range notify.RunBudget - 1 {
		if _, err := raise(); err != nil {
			t.Fatalf("seed raise %d: %v", i, err)
		}
	}
	for _, q := range []string{
		`CREATE OR REPLACE FUNCTION notify_budget_barrier() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN IF NEW.channel = 'budget-race' THEN PERFORM pg_sleep(0.6); END IF; RETURN NEW; END $$`,
		`CREATE TRIGGER notify_budget_barrier BEFORE INSERT ON approval_notifications FOR EACH ROW EXECUTE FUNCTION notify_budget_barrier()`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS notify_budget_barrier ON approval_notifications`)
	})

	var wg sync.WaitGroup
	start := make(chan struct{})
	ids := make([]uuid.UUID, 2)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ap, err := raise()
			if err != nil {
				t.Errorf("concurrent raise: %v", err)
			}
			ids[i] = ap.ID
		}()
	}
	close(start)
	wg.Wait()

	if rows := outboxRows(ctx, t, pool, runID); rows != notify.RunBudget {
		t.Fatalf("outbox rows = %d after two concurrent raises on %d, want %d", rows, notify.RunBudget-1, notify.RunBudget)
	}
	for _, id := range ids {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM approvals WHERE id = $1`, id).Scan(&n); err != nil || n != 1 {
			t.Errorf("approval %s: %d rows (err %v), want 1: an approval is never lost to the budget", id, n, err)
		}
	}
	if got := rec.count("approval.notify.suppressed"); got != 1 {
		t.Errorf("approval.notify.suppressed audited %d times, want 1", got)
	}
}
