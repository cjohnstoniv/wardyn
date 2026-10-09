// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests for registered runners, their pending actions and the credential delivery
// policy (migration 0146). Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func newRunner(owner string) types.Runner {
	id := uuid.New()
	return types.Runner{ID: id, Owner: owner, Name: "laptop", PublicKey: make([]byte, 32), KeyFingerprint: "fp-" + id.String(), Version: "0.9.0"}
}

func TestPG_Runners_CreateStartsUnclaimedAndFingerprintIsUnique(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	r := newRunner("alice@example.com")
	r.State = types.RunnerClaimed // ignored: every registration starts unclaimed
	got, err := st.CreateRunner(ctx, r)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM runners WHERE id=$1`, r.ID) })
	if got.State != types.RunnerUnclaimed || got.ClaimedAt != nil || got.PostureSource != "" || got.PostureReportedAt != nil {
		t.Fatalf("a new runner is not plainly unclaimed: %+v", got)
	}
	if got.Owner != r.Owner || got.KeyFingerprint != r.KeyFingerprint || len(got.PublicKey) != 32 {
		t.Fatalf("round trip lost fields: %+v", got)
	}
	dup := newRunner("bob@example.com")
	dup.KeyFingerprint = r.KeyFingerprint
	if _, err := st.CreateRunner(ctx, dup); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a reused key fingerprint = %v, want ErrConflict", err)
	}
	bad := newRunner("x")
	bad.PublicKey = []byte("short")
	if _, err := st.CreateRunner(ctx, bad); err == nil {
		t.Fatal("a public key that is not 32 bytes was stored")
	}
}

func TestPG_Runners_ClaimOnlyByOwnerWithFingerprint(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	r, err := st.CreateRunner(ctx, newRunner("alice@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM runners WHERE id=$1`, r.ID) })
	now := time.Now().UTC()

	if _, err := st.ClaimRunner(ctx, r.ID, "admin@example.com", r.KeyFingerprint, now); !errors.Is(err, store.ErrRunnerClaimMismatch) {
		t.Fatalf("a claim by someone but the owner = %v, want ErrRunnerClaimMismatch", err)
	}
	if _, err := st.ClaimRunner(ctx, r.ID, r.Owner, "another-fingerprint", now); !errors.Is(err, store.ErrRunnerClaimMismatch) {
		t.Fatalf("a claim with another fingerprint = %v, want ErrRunnerClaimMismatch", err)
	}
	if _, err := st.ClaimRunner(ctx, uuid.New(), r.Owner, r.KeyFingerprint, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a claim of an unknown runner = %v", err)
	}
	got, err := st.ClaimRunner(ctx, r.ID, r.Owner, r.KeyFingerprint, now)
	if err != nil || got.State != types.RunnerClaimed || got.ClaimedAt == nil {
		t.Fatalf("claim = %+v, %v", got, err)
	}
	if _, err := st.ClaimRunner(ctx, r.ID, r.Owner, r.KeyFingerprint, now); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a second claim = %v, want ErrConflict", err)
	}
}

func TestPG_Runners_ConcurrentClaimHasOneWinner(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	r, err := st.CreateRunner(ctx, newRunner("alice@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM runners WHERE id=$1`, r.ID) })
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := st.ClaimRunner(ctx, r.ID, r.Owner, r.KeyFingerprint, time.Now().UTC()); err == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d claims won, want exactly one", wins.Load())
	}
}

func TestPG_Runners_PostureOnlyForClaimedAndReportsChange(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	r, err := st.CreateRunner(ctx, newRunner("alice@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM runners WHERE id=$1`, r.ID) })
	now := time.Now().UTC()
	p := types.RunnerPosture{MDMManaged: true, DiskEncrypted: true, OS: "darwin", OSVersion: "15.1"}

	if _, err := st.RecordRunnerPosture(ctx, r.ID, p, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("posture from an unclaimed runner = %v, want ErrNotFound", err)
	}
	if _, err := st.ClaimRunner(ctx, r.ID, r.Owner, r.KeyFingerprint, now); err != nil {
		t.Fatal(err)
	}
	if changed, err := st.RecordRunnerPosture(ctx, r.ID, p, now); err != nil || !changed {
		t.Fatalf("first report: changed=%v err=%v", changed, err)
	}
	if changed, err := st.RecordRunnerPosture(ctx, r.ID, p, now); err != nil || changed {
		t.Fatalf("an identical report: changed=%v err=%v, want unchanged", changed, err)
	}
	p.DiskEncrypted = false
	if changed, err := st.RecordRunnerPosture(ctx, r.ID, p, now); err != nil || !changed {
		t.Fatalf("a different report: changed=%v err=%v", changed, err)
	}
	got, err := st.GetRunner(ctx, r.ID)
	if err != nil || got.Posture != p || got.PostureSource != types.RunnerPostureSourceRunnerAsserted || got.PostureReportedAt == nil {
		t.Fatalf("stored posture = %+v, %v", got, err)
	}
}

func TestPG_Runners_RevokeDropsOpenActionsAndIsIdempotent(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	r, err := st.CreateRunner(ctx, newRunner("alice@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM runners WHERE id=$1`, r.ID) })
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	if _, _, err := st.QueueRunnerAction(ctx, types.RunnerPendingAction{ID: uuid.New(), RunnerID: r.ID, RunID: run.ID, Kind: types.RunnerActionKill, Ref: "runner:x/y"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	got, err := st.RevokeRunner(ctx, r.ID, now)
	if err != nil || got.State != types.RunnerRevoked || got.RevokedAt == nil {
		t.Fatalf("revoke = %+v, %v", got, err)
	}
	if acts, err := st.ListPendingRunnerActions(ctx, r.ID); err != nil || len(acts) != 0 {
		t.Fatalf("a revoked runner kept %d pending actions (%v)", len(acts), err)
	}
	if _, err := st.RevokeRunner(ctx, r.ID, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a second revoke = %v, want ErrNotFound", err)
	}
	if _, err := st.ClaimRunner(ctx, r.ID, r.Owner, r.KeyFingerprint, now); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("claiming a revoked runner = %v, want ErrConflict", err)
	}
}

func TestPG_Runners_ListAndExpireUnclaimed(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	owner := "owner-" + uuid.NewString()
	a, _ := st.CreateRunner(ctx, newRunner(owner))
	b, _ := st.CreateRunner(ctx, newRunner(owner))
	other, _ := st.CreateRunner(ctx, newRunner("someone-else"))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM runners WHERE id = ANY($1)`, []uuid.UUID{a.ID, b.ID, other.ID})
	})
	if _, err := st.ClaimRunner(ctx, b.ID, owner, b.KeyFingerprint, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	mine, err := st.ListRunnersByOwner(ctx, owner)
	if err != nil || len(mine) != 2 {
		t.Fatalf("by owner = %d runners, %v", len(mine), err)
	}
	if all, err := st.ListRunners(ctx); err != nil || len(all) < 3 {
		t.Fatalf("list = %d, %v", len(all), err)
	}
	// Only the unclaimed runner older than the cutoff goes; the claimed one stays.
	n, err := st.ExpireUnclaimedRunners(ctx, time.Now().Add(time.Hour))
	if err != nil || n < 2 {
		t.Fatalf("expire = %d, %v", n, err)
	}
	if _, err := st.GetRunner(ctx, a.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the unclaimed runner survived: %v", err)
	}
	if _, err := st.GetRunner(ctx, b.ID); err != nil {
		t.Fatalf("the claimed runner was expired: %v", err)
	}
}

func TestPG_RunnerActions_QueueIsIdempotentOrderedAndFences(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	r, err := st.CreateRunner(ctx, newRunner("alice@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM runners WHERE id=$1`, r.ID) })
	run1 := persistRun(t, ctx, pool, newRun(types.RunRunning))
	run2 := persistRun(t, ctx, pool, newRun(types.RunRunning))

	if has, err := st.RunHasPendingRunnerAction(ctx, run1.ID); err != nil || has {
		t.Fatalf("fence before any action = %v, %v", has, err)
	}
	mk := func(run uuid.UUID, kind types.RunnerActionKind) types.RunnerPendingAction {
		return types.RunnerPendingAction{ID: uuid.New(), RunnerID: r.ID, RunID: run, Kind: kind, Ref: "runner:r/" + run.String()}
	}
	first, queued, err := st.QueueRunnerAction(ctx, mk(run1.ID, types.RunnerActionKill))
	if err != nil || !queued {
		t.Fatalf("first queue: queued=%v err=%v", queued, err)
	}
	again, queued, err := st.QueueRunnerAction(ctx, mk(run1.ID, types.RunnerActionKill))
	if err != nil || queued || again.ID != first.ID {
		t.Fatalf("the same (run, kind) queued twice: queued=%v id=%s want %s err=%v", queued, again.ID, first.ID, err)
	}
	if _, queued, err = st.QueueRunnerAction(ctx, mk(run1.ID, types.RunnerActionEnd)); err != nil || !queued {
		t.Fatalf("another kind for the run: queued=%v err=%v", queued, err)
	}
	if _, queued, err = st.QueueRunnerAction(ctx, mk(run2.ID, types.RunnerActionStopProxy)); err != nil || !queued {
		t.Fatalf("another run: queued=%v err=%v", queued, err)
	}
	if has, _ := st.RunHasPendingRunnerAction(ctx, run1.ID); !has {
		t.Fatal("a run with an unapplied action is not fenced")
	}
	acts, err := st.ListPendingRunnerActions(ctx, r.ID)
	if err != nil || len(acts) != 3 || acts[0].ID != first.ID {
		t.Fatalf("pending = %d actions (first %v), %v; want 3, oldest first", len(acts), acts, err)
	}
	for i := 1; i < len(acts); i++ {
		if acts[i].QueuedAt.Before(acts[i-1].QueuedAt) {
			t.Fatal("pending actions are not oldest first")
		}
	}

	now := time.Now().UTC()
	done, err := st.ApplyRunnerAction(ctx, first.ID, "applied", now.Add(-time.Second), now)
	if err != nil || done.AppliedAt == nil || done.Outcome != "applied" || done.ObservedAt == nil {
		t.Fatalf("apply = %+v, %v", done, err)
	}
	if _, err := st.ApplyRunnerAction(ctx, first.ID, "applied", now, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a replayed action_result = %v, want ErrNotFound", err)
	}
	// Applying the kill lifts only that kind's fence; the end is still open.
	if has, _ := st.RunHasPendingRunnerAction(ctx, run1.ID); !has {
		t.Fatal("the fence lifted while an action was still open")
	}
	// An applied (run, kind) may be queued again.
	if _, queued, err = st.QueueRunnerAction(ctx, mk(run1.ID, types.RunnerActionKill)); err != nil || !queued {
		t.Fatalf("re-queue after apply: queued=%v err=%v", queued, err)
	}
}

func TestPG_RunnerActions_RejectUnknownKindAndCascadeWithRun(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	r, _ := st.CreateRunner(ctx, newRunner("alice@example.com"))
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM runners WHERE id=$1`, r.ID) })
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	if _, _, err := st.QueueRunnerAction(ctx, types.RunnerPendingAction{ID: uuid.New(), RunnerID: r.ID, RunID: run.ID, Kind: "freeze", Ref: "x"}); err == nil {
		t.Fatal("an action kind outside kill/end/stop_proxy was stored")
	}
	if _, _, err := st.QueueRunnerAction(ctx, types.RunnerPendingAction{ID: uuid.New(), RunnerID: r.ID, RunID: run.ID, Kind: types.RunnerActionEnd, Ref: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM agent_runs WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if acts, _ := st.ListPendingRunnerActions(ctx, r.ID); len(acts) != 0 {
		t.Fatalf("%d actions outlived their run", len(acts))
	}
}

func TestPG_CredentialDeliveryPolicy_DefaultsEmptyAndUpserts(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	_, _ = pool.Exec(ctx, `DELETE FROM credential_delivery_policy`)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM credential_delivery_policy`) })

	d, err := st.GetCredentialDeliveryPolicy(ctx)
	if err != nil || string(d.Policy) != `{"classes":{}}` {
		t.Fatalf("default policy = %s, %v", d.Policy, err)
	}
	doc := json.RawMessage(`{"classes":{"api_key":{"mode":"via_org"}}}`)
	now := time.Now().UTC()
	if _, err := st.PutCredentialDeliveryPolicy(ctx, doc, "sec@example.com", now); err != nil {
		t.Fatal(err)
	}
	next := json.RawMessage(`{"classes":{"env_secret":{"mode":"runner_resident","require_posture":{"mdm_managed":true}}}}`)
	if _, err := st.PutCredentialDeliveryPolicy(ctx, next, "sec2@example.com", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetCredentialDeliveryPolicy(ctx)
	if err != nil || got.UpdatedBy != "sec2@example.com" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	var a, b any
	_ = json.Unmarshal(got.Policy, &a)
	_ = json.Unmarshal(next, &b)
	if ja, jb := mustJSON(a), mustJSON(b); ja != jb {
		t.Fatalf("policy = %s, want %s", ja, jb)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM credential_delivery_policy`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d policy rows, want the singleton", n)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO credential_delivery_policy (singleton) VALUES (false)`); err == nil {
		t.Fatal("a second policy row was inserted")
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestPG_CredentialGrants_DeliveryRoundTripAndCheck(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	for _, d := range []types.GrantDelivery{types.GrantDeliveryNone, types.GrantDeliveryOwn, types.GrantDeliveryViaOrg, types.GrantDeliveryResident} {
		g, err := st.CreateGrant(ctx, types.CredentialGrant{ID: uuid.New(), RunID: run.ID, CreatedAt: time.Now().UTC(), Delivery: d,
			Spec: types.GrantSpec{Kind: types.GrantEnvSecret}})
		if err != nil || g.Delivery != d {
			t.Fatalf("create with delivery %q = %q, %v", d, g.Delivery, err)
		}
	}
	grants, err := st.ListGrantsByRun(ctx, run.ID)
	if err != nil || len(grants) != 4 {
		t.Fatalf("list = %d, %v", len(grants), err)
	}
	seen := map[types.GrantDelivery]bool{}
	for _, g := range grants {
		seen[g.Delivery] = true
	}
	if len(seen) != 4 {
		t.Fatalf("deliveries read back = %v", seen)
	}
	if _, err := st.CreateGrant(ctx, types.CredentialGrant{ID: uuid.New(), RunID: run.ID, CreatedAt: time.Now().UTC(), Delivery: "everywhere",
		Spec: types.GrantSpec{Kind: types.GrantEnvSecret}}); err == nil {
		t.Fatal("a delivery outside the CHECK was stored")
	}
	// A grant written the old way (no delivery column) reads back as ''.
	id := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO credential_grants (id, run_id, spec) VALUES ($1,$2,'{"kind":"env_secret"}')`, id, run.ID); err != nil {
		t.Fatal(err)
	}
	var d string
	if err := pool.QueryRow(ctx, `SELECT delivery FROM credential_grants WHERE id=$1`, id).Scan(&d); err != nil || d != "" {
		t.Fatalf("legacy grant delivery = %q, %v", d, err)
	}
}
