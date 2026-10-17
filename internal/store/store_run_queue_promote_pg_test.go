// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func cappedPool(t *testing.T, pg store.PG, maxRuns int) types.RunnerPool {
	t.Helper()
	p, err := pg.CreateRunnerPool(context.Background(), types.RunnerPool{ID: uuid.New(), Name: "Capped " + uuid.NewString()[:8],
		Scope: types.RunnerPoolOrganization, Limits: poolLimitsFixture(t, maxRuns)}, "test")
	if err != nil {
		t.Fatalf("create capped pool: %v", err)
	}
	return p
}

func admitAll(context.Context, types.RunnerPool, store.RunQueueEntry) (store.QueueVerdict, error) {
	return store.QueueVerdict{Admit: true}, nil
}

func pass(p types.RunnerPool) store.PromotePass {
	return store.PromotePass{PoolID: p.ID, ScanLimit: 50, WatcherOwner: "test-replica"}
}

func runState(t *testing.T, pg store.PG, id uuid.UUID) types.AgentRun {
	t.Helper()
	r, err := pg.GetRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Two promoters racing for one free slot promote exactly one of five waiting runs, and the pool never
// holds more than its cap.
func TestPG_PromoteQueueExactlyOnce(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	p := cappedPool(t, pg, 1)
	now := time.Now().UTC()
	var ids []uuid.UUID
	for i := range 5 {
		r, err := enqueue(t, pg, p, "alice", now.Add(time.Duration(i)*time.Second), 0)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
	}
	// The verdicts are asked with no lock held, so both promoters judge the queue as having one free
	// slot, each admitting a different run, and they meet before either goes on. Only the pool row's lock
	// then stops the second from counting the slot as free and promoting its own run too.
	var arrived atomic.Int32
	meet := func(mine uuid.UUID) store.QueueAdmit {
		return func(_ context.Context, _ types.RunnerPool, e store.RunQueueEntry) (store.QueueVerdict, error) {
			if e.RunID != mine {
				return store.QueueVerdict{}, nil
			}
			arrived.Add(1)
			for deadline := time.Now().Add(2 * time.Second); arrived.Load() < 2 && time.Now().Before(deadline); {
				time.Sleep(time.Millisecond)
			}
			return store.QueueVerdict{Admit: true}, nil
		}
	}
	var wg sync.WaitGroup
	var promoted atomic.Int32
	errs := make(chan error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, _, err := pg.PromoteRunQueue(ctx, pass(p), meet(ids[i]))
			promoted.Add(int32(len(got)))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("promote: %v", err)
		}
	}
	if promoted.Load() != 1 {
		t.Fatalf("two promoters on a pool with one free slot promoted %d runs, want exactly 1", promoted.Load())
	}
	if n, err := pg.CountRunnerPoolSlotHolders(ctx, p.ID); err != nil || n != 1 {
		t.Errorf("slot holders = %d, %v; want 1", n, err)
	}
	pending := 0
	for _, id := range ids {
		if r := runState(t, pg, id); r.State == types.RunPending {
			pending++
			if r.AdmittedAt == nil {
				t.Errorf("promoted run %s has no admitted_at", id)
			}
		}
	}
	if pending != 1 {
		t.Errorf("%d runs PENDING, want 1", pending)
	}
}

// Promotion takes the oldest first; a head the verdict will not admit keeps its place and the next
// is tried; the held run goes first once it can be placed.
func TestPG_PromoteQueueFIFOAndSkip(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	p := cappedPool(t, pg, 2)
	now := time.Now().UTC()
	var ids []uuid.UUID
	for i, owner := range []string{"bob", "alice", "alice"} {
		r, err := enqueue(t, pg, p, owner, now.Add(time.Duration(i)*time.Second), 0)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
	}
	skipHead := func(_ context.Context, _ types.RunnerPool, e store.RunQueueEntry) (store.QueueVerdict, error) {
		if e.RunID == ids[0] {
			return store.QueueVerdict{Wait: "runner_pool_no_runner_free"}, nil
		}
		return store.QueueVerdict{Admit: true}, nil
	}
	got, held, err := pg.PromoteRunQueue(ctx, pass(p), skipHead)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Run.ID != ids[1] || got[1].Run.ID != ids[2] {
		t.Fatalf("promoted %v, want the second and third in order", got)
	}
	if len(held) != 1 || held[0].RunID != ids[0] || held[0].Wait != "runner_pool_no_runner_free" {
		t.Errorf("held = %+v, want the head with its wait reason", held)
	}
	if r := runState(t, pg, ids[0]); r.State != types.RunQueued || queueRows(t, pgPool(t, pg), ids[0]) != 1 {
		t.Errorf("the held head is %s with %d queue rows; it must keep its place", r.State, queueRows(t, pgPool(t, pg), ids[0]))
	}
	// One slot frees: the held head is the oldest, so it is the one that starts.
	if ok, err := pg.UpdateRunStateIf(ctx, ids[1], types.RunPending, types.RunCompleted); err != nil || !ok {
		t.Fatalf("finish a promoted run: %v %v", ok, err)
	}
	got, _, err = pg.PromoteRunQueue(ctx, pass(p), admitAll)
	if err != nil || len(got) != 1 || got[0].Run.ID != ids[0] {
		t.Fatalf("second pass promoted %v, %v; want the held head", got, err)
	}
}

// A pool at its cap, a pool switched off, a pool that is gone and a deployment at its cap each promote
// nothing and leave every waiting run in place.
func TestPG_PromoteQueueRespectsEveryCap(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	p := cappedPool(t, pg, 1)
	if _, err := pg.CreateRun(ctx, poolRun(p, types.RunRunning, "alice")); err != nil {
		t.Fatal(err)
	}
	waiting, err := enqueue(t, pg, p, "alice", time.Now().UTC(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, _, err := pg.PromoteRunQueue(ctx, pass(p), admitAll); err != nil || len(got) != 0 {
		t.Fatalf("a pool at its cap promoted %v, %v", got, err)
	}

	roomy := cappedPool(t, pg, 5)
	w2, err := enqueue(t, pg, roomy, "alice", time.Now().UTC(), 0)
	if err != nil {
		t.Fatal(err)
	}
	capped := pass(roomy)
	capped.DeploymentCap = 1 // the one running run above already holds the deployment's slot
	if got, _, err := pg.PromoteRunQueue(ctx, capped, admitAll); err != nil || len(got) != 0 {
		t.Fatalf("a deployment at its cap promoted %v, %v", got, err)
	}

	off := types.RunnerPoolDisabled
	if _, err := pg.UpdateRunnerPool(ctx, roomy.ID, roomy.Revision, nil, &off, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got, _, err := pg.PromoteRunQueue(ctx, pass(roomy), admitAll); err != nil || len(got) != 0 {
		t.Fatalf("a pool switched off promoted %v, %v", got, err)
	}
	if got, _, err := pg.PromoteRunQueue(ctx, store.PromotePass{PoolID: uuid.New()}, admitAll); err != nil || len(got) != 0 {
		t.Fatalf("an unknown pool promoted %v, %v", got, err)
	}
	for _, id := range []uuid.UUID{waiting.ID, w2.ID} {
		if r := runState(t, pg, id); r.State != types.RunQueued || r.AdmittedAt != nil || queueRows(t, pool, id) != 1 {
			t.Errorf("run %s is %s, admitted %v, %d queue rows; it must be left waiting", id, r.State, r.AdmittedAt, queueRows(t, pool, id))
		}
	}
}

// pgPool reaches the pool a PG wraps for a raw count.
func pgPool(t *testing.T, pg store.PG) *pgxpool.Pool {
	t.Helper()
	return pg.Pool
}

// Enqueue, promotion, completion and revive racing on a pool of two never put a third run in it.
func TestPG_QueueRaceNeverExceedsTheCap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pg := store.NewPG(runsPGPoolIsolated(t))
	const capRuns = 2
	p := cappedPool(t, pg, capRuns)
	var over atomic.Int32
	stop := make(chan struct{})
	var bg sync.WaitGroup
	sample := func() {
		defer bg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if n, err := pg.CountRunnerPoolSlotHolders(ctx, p.ID); err == nil && n > capRuns {
				over.Add(1)
			}
			time.Sleep(time.Millisecond)
		}
	}
	bg.Add(1)
	go sample()

	// A kept run, revived while the others race: it must take the pool's lock first.
	kept, err := pg.CreateRun(ctx, poolRun(p, types.RunRunning, "keeper"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pgPool(t, pg).Exec(ctx, `UPDATE agent_runs SET lost_at=now(), lost_reason='reboot' WHERE id=$1`, kept.ID); err != nil {
		t.Fatal(err)
	}

	var work sync.WaitGroup
	for range 3 {
		work.Add(1)
		go func() {
			defer work.Done()
			for i := range 8 {
				_, _ = enqueue(t, pg, p, "alice", time.Now().UTC().Add(time.Duration(i)*time.Millisecond), 0)
			}
		}()
	}
	for range 3 {
		work.Add(1)
		go func() {
			defer work.Done()
			for range 40 {
				got, _, err := pg.PromoteRunQueue(ctx, pass(p), admitAll)
				if err != nil {
					t.Errorf("promote: %v", err)
					return
				}
				for _, g := range got {
					time.Sleep(2 * time.Millisecond)
					_, _ = pg.UpdateRunStateIf(ctx, g.Run.ID, types.RunPending, types.RunCompleted)
				}
			}
		}()
	}
	work.Add(1)
	go func() {
		defer work.Done()
		for range 40 {
			claimed, err := pg.MarkRunRevived(ctx, kept.ID, types.LostReboot, nil, 0, true)
			var full *store.RunnerPoolFullError
			if err != nil && !errors.As(err, &full) {
				t.Errorf("revive: %v", err)
				return
			}
			if claimed {
				_, _ = pgPool(t, pg).Exec(ctx, `UPDATE agent_runs SET lost_at=now(), lost_reason='reboot' WHERE id=$1`, kept.ID)
			}
		}
	}()
	work.Wait()
	close(stop)
	bg.Wait()
	if over.Load() != 0 {
		t.Errorf("the pool held more than %d slot-holders on %d samples", capRuns, over.Load())
	}
}

// A revive takes a slot only when the pool has one free and nobody waits.
func TestPG_ReviveIntoACappedPool(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	p := cappedPool(t, pg, 2)
	if _, err := pg.CreateRun(ctx, poolRun(p, types.RunRunning, "alice")); err != nil {
		t.Fatal(err)
	}
	kept, err := pg.CreateRun(ctx, poolRun(p, types.RunRunning, "alice"))
	if err != nil {
		t.Fatal(err)
	}
	keep := func() {
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at=now(), lost_reason='reboot' WHERE id=$1`, kept.ID); err != nil {
			t.Fatal(err)
		}
	}
	keep()
	// One other slot-holder, one free slot, nobody waiting: the revive is claimed.
	if claimed, err := pg.MarkRunRevived(ctx, kept.ID, types.LostReboot, nil, 0, true); err != nil || !claimed {
		t.Fatalf("revive with a free slot = %v, %v", claimed, err)
	}
	keep()
	// A waiting run is first in line: the same revive is refused, naming the pool and its cap.
	if _, err := enqueue(t, pg, p, "bob", time.Now().UTC(), 0); err != nil {
		t.Fatal(err)
	}
	_, err = pg.MarkRunRevived(ctx, kept.ID, types.LostReboot, nil, 0, true)
	var full *store.RunnerPoolFullError
	if !errors.Is(err, store.ErrRunnerPoolFull) || !errors.As(err, &full) || full.Max != 2 || full.Pool != p.Name {
		t.Fatalf("revive with a run waiting = %v; want ErrRunnerPoolFull naming %q and 2", err, p.Name)
	}
	// A revive that starts no agent takes no slot and is never refused for one.
	if claimed, err := pg.MarkRunRevived(ctx, kept.ID, types.LostReboot, nil, 0, false); err != nil || !claimed {
		t.Errorf("a proxy-only revive = %v, %v; want claimed", claimed, err)
	}
}

// The timeout scan lists only waiting runs past their deadline; the stale-lease scan lists only
// promoted runs whose start stopped touching its lease, never a plain PENDING run in its build.
func TestPG_QueueScans(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	p := cappedPool(t, pg, 1)
	now := time.Now().UTC()
	late, err := pg.CreateQueuedRun(ctx, poolRun(p, types.RunQueued, "alice"), store.RunQueueEntry{QueuedAt: now.Add(-3 * time.Hour), DeadlineAt: now.Add(-time.Hour)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pg.CreateQueuedRun(ctx, poolRun(p, types.RunQueued, "alice"), store.RunQueueEntry{QueuedAt: now, DeadlineAt: now.Add(time.Hour)}, 0); err != nil {
		t.Fatal(err)
	}
	expired, err := pg.ExpiredQueueEntries(ctx, now, 10)
	if err != nil || len(expired) != 1 || expired[0].RunID != late.ID {
		t.Fatalf("expired = %+v, %v; want only the run past its deadline", expired, err)
	}

	// A plain run five minutes into its build, its lease two minutes old.
	plain := newRun(types.RunPending)
	plain.CreatedAt = now.Add(-5 * time.Minute)
	if _, err := pg.CreateRun(ctx, plain); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET watcher_heartbeat = now() - interval '2 minutes' WHERE id=$1`, plain.ID); err != nil {
		t.Fatal(err)
	}
	// A promoted run whose start died, and one whose start is alive.
	dead, alive := promotedRun(t, pg, now), promotedRun(t, pg, now.Add(time.Second))
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET watcher_heartbeat = now() - interval '2 minutes' WHERE id=$1`, dead.ID); err != nil {
		t.Fatal(err)
	}
	stale, err := pg.StalePromotedRuns(ctx, 90*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0].ID != dead.ID {
		var got []uuid.UUID
		for _, r := range stale {
			got = append(got, r.ID)
		}
		t.Errorf("stale promoted runs = %v; want only %s (not the live start %s, nor the plain PENDING run %s)", got, dead.ID, alive.ID, plain.ID)
	}
}

// promotedRun enqueues a run on a pool with room and promotes it.
func promotedRun(t *testing.T, pg store.PG, at time.Time) types.AgentRun {
	t.Helper()
	roomy := cappedPool(t, pg, 10)
	r, err := pg.CreateQueuedRun(context.Background(), poolRun(roomy, types.RunQueued, "carol"), store.RunQueueEntry{QueuedAt: at, DeadlineAt: at.Add(time.Hour)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := pg.PromoteRunQueue(context.Background(), pass(roomy), admitAll)
	if err != nil || len(got) != 1 || got[0].Run.ID != r.ID {
		t.Fatalf("promote: %v %v", got, err)
	}
	return got[0].Run
}

// Removing a pool ends the runs waiting for it in the delete's own transaction, KILLED with the removal
// reason and no queue row left; switching a pool off holds them.
func TestPG_DeletingAPoolCancelsItsWaitingRuns(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	p := cappedPool(t, pg, 1)
	a, err := enqueue(t, pg, p, "alice", time.Now().UTC(), 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := enqueue(t, pg, p, "bob", time.Now().UTC().Add(time.Second), 0)
	if err != nil {
		t.Fatal(err)
	}
	off := types.RunnerPoolDisabled
	switched, err := pg.UpdateRunnerPool(ctx, p.ID, p.Revision, nil, &off, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _, err := pg.PromoteRunQueue(ctx, pass(p), admitAll); err != nil || len(got) != 0 {
		t.Fatalf("a switched-off pool promoted %v, %v", got, err)
	}
	for _, id := range []uuid.UUID{a.ID, b.ID} {
		if r := runState(t, pg, id); r.State != types.RunQueued {
			t.Fatalf("switching the pool off moved run %s to %s; it must keep waiting", id, r.State)
		}
	}
	del, err := pg.DeleteRunnerPool(ctx, switched.ID)
	if err != nil {
		t.Fatalf("delete a pool with waiting runs: %v", err)
	}
	if len(del.CancelledRuns) != 2 {
		t.Errorf("cancelled runs = %v, want both", del.CancelledRuns)
	}
	for _, id := range []uuid.UUID{a.ID, b.ID} {
		r := runState(t, pg, id)
		if r.State != types.RunKilled || r.StatusDetail != "queue: runner_pool_removed" || r.FailureHint != types.RunEndRunnerPoolRemoved.Sentence() || r.EndedAt == nil {
			t.Errorf("run %s = %s %q %q; want KILLED with the removal reason and sentence", id, r.State, r.StatusDetail, r.FailureHint)
		}
		if queueRows(t, pool, id) != 0 {
			t.Errorf("run %s left a queue row behind", id)
		}
	}
}

// The start's column write lands on a promoted, undispatched run only; the wait, the end and the
// promoted status detail are written only while they apply.
func TestPG_QueueWritersGuardTheirState(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	p := cappedPool(t, pg, 1)
	queued, err := enqueue(t, pg, p, "alice", time.Now().UTC(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := pg.StartQueuedRun(ctx, queued.ID, store.QueueStart{SPIFFEID: "spiffe://x/early"}); err != nil || ok {
		t.Errorf("the start wrote a run that is still QUEUED: ok=%v err=%v", ok, err)
	}
	if err := pg.SetQueueWait(ctx, queued.ID, "queue: runner_pool_no_runner_free"); err != nil || runState(t, pg, queued.ID).StatusDetail != "queue: runner_pool_no_runner_free" {
		t.Errorf("SetQueueWait on a waiting run: %v %q", err, runState(t, pg, queued.ID).StatusDetail)
	}
	got, _, err := pg.PromoteRunQueue(ctx, pass(p), admitAll)
	if err != nil || len(got) != 1 {
		t.Fatalf("promote: %v %v", got, err)
	}
	if err := pg.SetQueueWait(ctx, queued.ID, "queue: runner_pool_at_capacity"); err != nil || runState(t, pg, queued.ID).StatusDetail == "queue: runner_pool_at_capacity" {
		t.Errorf("SetQueueWait wrote a run that left the queue: %v", err)
	}
	ends := time.Now().UTC().Add(time.Hour)
	started, ok, err := pg.StartQueuedRun(ctx, queued.ID, store.QueueStart{
		SPIFFEID: "spiffe://x/started", ConfinementClass: types.CC2, EndsAt: &ends, WaitBudgetSec: 600, RunLimits: types.RunLimits{MaxEndAheadSec: 3600},
		Placement: types.PlacementRemote, EvidenceSource: types.RunEvidenceSubstrate,
	})
	if err != nil || !ok || started.SPIFFEID != "spiffe://x/started" || started.EndsAt == nil || started.WaitBudgetSec != 600 || started.RunLimits.MaxEndAheadSec != 3600 {
		t.Fatalf("the start's write = %+v ok=%v err=%v", started, ok, err)
	}
	if err := pg.SetPromotedStatusDetail(ctx, queued.ID, "queue: queue_policy_changed"); err != nil || runState(t, pg, queued.ID).StatusDetail != "queue: queue_policy_changed" {
		t.Errorf("SetPromotedStatusDetail on a promoted run: %v", err)
	}
	if ok, err := pg.UpdateRunStateIf(ctx, queued.ID, types.RunPending, types.RunKilled); err != nil || !ok {
		t.Fatal(err)
	}
	if _, ok, err := pg.StartQueuedRun(ctx, queued.ID, store.QueueStart{SPIFFEID: "spiffe://x/late"}); err != nil || ok {
		t.Errorf("the start wrote a run a kill had won: ok=%v err=%v", ok, err)
	}
	if ended, err := pg.ExitQueuedRun(ctx, queued.ID, types.RunFailed, "queue: queue_timeout", "late"); err != nil || ended {
		t.Errorf("ExitQueuedRun on a run that left the queue = %v %v", ended, err)
	}
}

// CreateQueuedRun refuses a deactivated or stale-epoch owner and a pool that moved, switched off or
// is gone, leaving neither a run nor a queue row; a run that is not QUEUED or has no pool is no
// queued run at all.
func TestPG_CreateQueuedRunRefusals(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	p := cappedPool(t, pg, 1)
	entry := store.RunQueueEntry{QueuedAt: time.Now().UTC(), DeadlineAt: time.Now().UTC().Add(time.Hour)}
	nothingWritten := func(what string) {
		t.Helper()
		var runs, rows int
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM agent_runs), (SELECT count(*) FROM run_queue)`).Scan(&runs, &rows); err != nil {
			t.Fatal(err)
		}
		if runs != 0 || rows != 0 {
			t.Errorf("%s left %d run rows and %d queue rows", what, runs, rows)
		}
	}

	epoch, err := pg.IssueLoginIdentity(ctx, store.LoginIdentity{Principal: "zed", Issuer: "https://idp.example", TenantID: "t", ObjectID: "o-zed", Email: "zed@example.com"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	owner := poolRun(p, types.RunQueued, "zed")
	if _, err := pg.CreateQueuedRun(store.WithIdentityGuard(ctx, store.IdentityGuard{Principal: "zed", Epoch: epoch}), owner, entry, 0); err != nil {
		t.Fatalf("a live owner at their epoch was refused: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM run_queue; DELETE FROM agent_runs`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE principal_identities SET authority_epoch = $2 WHERE principal = $1`, "zed", epoch+1); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.CreateQueuedRun(store.WithIdentityGuard(ctx, store.IdentityGuard{Principal: "zed", Epoch: epoch}), poolRun(p, types.RunQueued, "zed"), entry, 0); !errors.Is(err, store.ErrIdentityDeactivated) {
		t.Errorf("an owner past their epoch = %v; want ErrIdentityDeactivated", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE principal_identities SET deactivated_at = now() WHERE principal = $1`, "zed"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.CreateQueuedRun(store.WithIdentityGuard(ctx, store.IdentityGuard{Principal: "zed", Epoch: -1}), poolRun(p, types.RunQueued, "zed"), entry, 0); !errors.Is(err, store.ErrIdentityDeactivated) {
		t.Errorf("a deactivated owner = %v; want ErrIdentityDeactivated", err)
	}
	nothingWritten("a refused owner")

	moved := poolRun(p, types.RunQueued, "alice")
	moved.RunnerPoolRevision = p.Revision + 1
	gone := poolRun(p, types.RunQueued, "alice")
	gone.RunnerPoolID = ptrTo(uuid.New())
	for name, r := range map[string]types.AgentRun{"a pool that moved": moved, "a pool that is gone": gone} {
		if _, err := pg.CreateQueuedRun(ctx, r, entry, 0); !errors.Is(err, store.ErrRunnerPoolStale) {
			t.Errorf("%s = %v; want ErrRunnerPoolStale", name, err)
		}
	}
	off := types.RunnerPoolDisabled
	switched, err := pg.UpdateRunnerPool(ctx, p.ID, p.Revision, nil, &off, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pg.CreateQueuedRun(ctx, poolRun(switched, types.RunQueued, "alice"), entry, 0); !errors.Is(err, store.ErrRunnerPoolStale) {
		t.Errorf("a pool switched off = %v; want ErrRunnerPoolStale", err)
	}
	if _, err := pg.CreateQueuedRun(ctx, poolRun(p, types.RunPending, "alice"), entry, 0); err == nil {
		t.Error("a run that is not QUEUED was queued")
	}
	unbound := poolRun(p, types.RunQueued, "alice")
	unbound.RunnerPoolID = nil
	if _, err := pg.CreateQueuedRun(ctx, unbound, entry, 0); err == nil {
		t.Error("a run with no pool was queued")
	}
	nothingWritten("a refused pool or state")
}

func ptrTo[T any](v T) *T { return &v }

// Promotion moves a run only while it is still QUEUED: a run that left the queue some other way is
// never revived into PENDING, whatever row is left behind.
func TestPG_PromoteQueueNeverRevivesAnExitedRun(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	p := cappedPool(t, pg, 1)
	r, err := enqueue(t, pg, p, "alice", time.Now().UTC(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET state='KILLED', ended_at=now() WHERE id=$1`, r.ID); err != nil {
		t.Fatal(err)
	}
	got, _, err := pg.PromoteRunQueue(ctx, pass(p), admitAll)
	if err != nil || len(got) != 0 {
		t.Fatalf("promote = %v, %v; want nothing promoted", got, err)
	}
	if st := runState(t, pg, r.ID).State; st != types.RunKilled {
		t.Errorf("a killed run is %s after a promotion pass", st)
	}
	if queueRows(t, pool, r.ID) != 0 {
		t.Error("the pass left the exited run's queue row behind")
	}
}

// A kill that lands while the pass is judging a run wins: the pass holds no lock while it judges, so the
// kill never waits for it, and the promotion CAS then finds the run gone and moves nothing. Neither fails.
func TestPG_PromoteAndKillDoNotDeadlock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	p := cappedPool(t, pg, 1)
	r, err := enqueue(t, pg, p, "alice", time.Now().UTC(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var killed bool
	var killErr error
	judging := func(context.Context, types.RunnerPool, store.RunQueueEntry) (store.QueueVerdict, error) {
		killed, killErr = pg.UpdateRunStateIf(ctx, r.ID, types.RunQueued, types.RunKilled)
		return store.QueueVerdict{Admit: true}, nil
	}
	got, _, err := pg.PromoteRunQueue(ctx, pass(p), judging)
	if err != nil || killErr != nil {
		t.Fatalf("pass error %v, kill error %v; neither may fail", err, killErr)
	}
	if !killed || len(got) != 0 {
		t.Errorf("kill won %v, pass promoted %d; the kill lands while the pass judges, so it wins", killed, len(got))
	}
	if st := runState(t, pg, r.ID).State; st != types.RunKilled || queueRows(t, pool, r.ID) != 0 {
		t.Errorf("the killed run is %s with %d queue rows", st, queueRows(t, pool, r.ID))
	}
}
