// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// residentRun is a local run in state bound to rn under its owner.
func (f actionFixture) residentRun(t *testing.T, rn types.Runner, state types.RunState) types.AgentRun {
	t.Helper()
	run := newRun(state)
	run.CreatedBy, run.Placement, run.RunnerID, run.EvidenceSource = rn.Owner, types.PlacementLocal, &rn.ID, types.RunEvidenceRunnerAsserted
	run, err := f.st.CreateRun(t.Context(), run)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

type residentRow struct {
	notAfter              time.Time
	holder                *uuid.UUID
	eraseBegun, erased    *time.Time
	attempts              int
	nextErase             time.Time
	lastReason, abandoned *string
}

func (f actionFixture) residentRow(t *testing.T, runID uuid.UUID) residentRow {
	t.Helper()
	var r residentRow
	if err := f.pool.QueryRow(t.Context(), `SELECT not_after, delivery_holder, erase_begun_at, erased_at, erase_attempts, next_erase_at,
		erase_last_reason, abandoned_reason FROM runner_resident_runs WHERE run_id = $1`, runID).Scan(&r.notAfter, &r.holder,
		&r.eraseBegun, &r.erased, &r.attempts, &r.nextErase, &r.lastReason, &r.abandoned); err != nil {
		t.Fatal(err)
	}
	return r
}

func (f actionFixture) endRun(t *testing.T, runID uuid.UUID) {
	t.Helper()
	if ok, err := f.st.UpdateRunStateIf(t.Context(), runID, types.RunStarting, types.RunFailed); err != nil || !ok {
		t.Fatalf("end run: %v %v", ok, err)
	}
}

// A delivery opens its obligation only for a STARTING run bound to a claimed,
// unrevoked runner, and only while no other dispatch holds a live lease; a
// repeat by the same holder keeps the sooner not_after; renewal needs the same
// holder and a STARTING run; once an erase began nothing is delivered.
func TestResidentObligation_LeaseRequiresStartingAndBinding(t *testing.T) {
	f := newActionFixture(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	run := f.residentRun(t, f.rn, types.RunStarting)
	a, b := uuid.New(), uuid.New()
	soon, later := now.Add(2*time.Hour), now.Add(20*time.Hour)

	if err := f.st.BeginResidentDelivery(ctx, run.ID, f.rn.ID, a, now, now.Add(2*time.Minute), soon); err != nil {
		t.Fatal(err)
	}
	if err := f.st.BeginResidentDelivery(ctx, run.ID, f.rn.ID, b, now, now.Add(2*time.Minute), soon); !errors.Is(err, store.ErrResidentDeliveryRefused) {
		t.Fatalf("a second holder inside the lease = %v, want refused", err)
	}
	if err := f.st.BeginResidentDelivery(ctx, run.ID, f.rn.ID, a, now, now.Add(2*time.Minute), later); err != nil {
		t.Fatalf("the same holder again = %v", err)
	}
	if got := f.residentRow(t, run.ID); !got.notAfter.Equal(soon) || got.holder == nil || *got.holder != a {
		t.Fatalf("row = %+v, want the sooner not_after %v held by the first holder", got, soon)
	}
	if err := f.st.RenewResidentDelivery(ctx, run.ID, b, now.Add(3*time.Minute)); !errors.Is(err, store.ErrResidentDeliveryRefused) {
		t.Fatalf("renewal by another holder = %v, want refused", err)
	}
	if err := f.st.RenewResidentDelivery(ctx, run.ID, a, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Past the lease, another dispatch may take it over.
	past := now.Add(4 * time.Minute)
	if err := f.st.BeginResidentDelivery(ctx, run.ID, f.rn.ID, b, past, past.Add(2*time.Minute), later); err != nil {
		t.Fatalf("a holder after the lease lapsed = %v", err)
	}

	// The binding: another runner of the same owner, a run not STARTING, a revoked runner.
	other := claimed(t, f.st, ctx, f.rn.Owner)
	if err := f.st.BeginResidentDelivery(ctx, run.ID, other.ID, uuid.New(), now, now.Add(time.Minute), soon); !errors.Is(err, store.ErrRunnerActionRefused) {
		t.Fatalf("another runner = %v, want ErrRunnerActionRefused", err)
	}
	running := f.residentRun(t, f.rn, types.RunRunning)
	if err := f.st.BeginResidentDelivery(ctx, running.ID, f.rn.ID, uuid.New(), now, now.Add(time.Minute), soon); !errors.Is(err, store.ErrResidentDeliveryRefused) {
		t.Fatalf("a RUNNING run = %v, want refused", err)
	}
	if n := f.count(t, `SELECT count(*) FROM runner_resident_runs WHERE run_id = $1`, running.ID); n != 0 {
		t.Fatal("a refused delivery wrote an obligation")
	}
	revokedRun := f.residentRun(t, other, types.RunStarting)
	if _, err := f.st.RevokeRunner(ctx, other.ID, now, "admin", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.st.BeginResidentDelivery(ctx, revokedRun.ID, other.ID, uuid.New(), now, now.Add(time.Minute), soon); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a revoked runner = %v, want ErrNotFound", err)
	}

	// A run that left STARTING is not renewed; once an erase began, nothing is delivered.
	f.endRun(t, run.ID)
	if err := f.st.RenewResidentDelivery(ctx, run.ID, b, past.Add(3*time.Minute)); !errors.Is(err, store.ErrResidentDeliveryRefused) {
		t.Fatalf("renewal after the run ended = %v, want refused", err)
	}
	second := f.residentRun(t, f.rn, types.RunStarting)
	if err := f.st.BeginResidentDelivery(ctx, second.ID, f.rn.ID, a, now, now.Add(time.Minute), soon); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE runner_resident_runs SET erase_begun_at = now() WHERE run_id = $1`, second.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.st.BeginResidentDelivery(ctx, second.ID, f.rn.ID, a, now, now.Add(time.Minute), soon); !errors.Is(err, store.ErrResidentDeliveryRefused) {
		t.Fatalf("a delivery after the erase began = %v, want refused", err)
	}
	if err := f.st.RenewResidentDelivery(ctx, second.ID, a, now.Add(time.Minute)); !errors.Is(err, store.ErrResidentDeliveryRefused) {
		t.Fatalf("a renewal after the erase began = %v, want refused", err)
	}
}

// An erase claims only an ended run whose delivery lease is released or lapsed
// and whose backoff is over; each claim counts the attempt and moves the next
// one out (30 s, doubling, capped at 1 h); a row another claim holds is
// skipped, not waited on; an erased row is never claimed again.
func TestResidentObligation_ClaimBackoffAndSkipLocked(t *testing.T) {
	f := newActionFixture(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	run := f.residentRun(t, f.rn, types.RunStarting)
	holder := uuid.New()
	if err := f.st.BeginResidentDelivery(ctx, run.ID, f.rn.ID, holder, now, now.Add(2*time.Minute), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	claim := func(at time.Time) []uuid.UUID {
		t.Helper()
		got, err := f.st.ClaimResidentErase(ctx, f.rn.ID, at, 8)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := claim(now); len(got) != 0 {
		t.Fatalf("claimed %v while the run is STARTING", got)
	}
	f.endRun(t, run.ID)
	if got := claim(now); len(got) != 0 {
		t.Fatalf("claimed %v inside a live delivery lease", got)
	}
	if err := f.st.EndResidentDelivery(ctx, run.ID, holder); err != nil {
		t.Fatal(err)
	}
	if due, err := f.st.DueResidentEraseRunners(ctx, now); err != nil || !slices.Equal(due, []uuid.UUID{f.rn.ID}) {
		t.Fatalf("due runners = %v (%v), want this runner", due, err)
	}
	if got := claim(now); !slices.Equal(got, []uuid.UUID{run.ID}) {
		t.Fatalf("claim = %v, want the ended run", got)
	}
	row := f.residentRow(t, run.ID)
	if row.attempts != 1 || row.eraseBegun == nil || !row.eraseBegun.Equal(now) || !row.nextErase.Equal(now.Add(30*time.Second)) {
		t.Fatalf("after the first claim %+v, want one attempt, erase begun now, next in 30s", row)
	}
	if got := claim(now.Add(10 * time.Second)); len(got) != 0 {
		t.Fatalf("claimed %v inside the backoff", got)
	}
	if due, _ := f.st.DueResidentEraseRunners(ctx, now.Add(10*time.Second)); len(due) != 0 {
		t.Fatalf("due runners %v inside the backoff", due)
	}
	at := now.Add(31 * time.Second)
	if got := claim(at); len(got) != 1 {
		t.Fatal("not claimed after the backoff")
	}
	if row := f.residentRow(t, run.ID); row.attempts != 2 || !row.eraseBegun.Equal(now) || !row.nextErase.Equal(at.Add(time.Minute)) {
		t.Fatalf("after the second claim %+v, want two attempts, the first erase time kept, next in 60s", row)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE runner_resident_runs SET erase_attempts = 12, next_erase_at = $2 WHERE run_id = $1`, run.ID, at); err != nil {
		t.Fatal(err)
	}
	if got := claim(at); len(got) != 1 {
		t.Fatal("not claimed when due")
	}
	if row := f.residentRow(t, run.ID); !row.nextErase.Equal(at.Add(time.Hour)) {
		t.Fatalf("next erase %v, want the 1h cap %v", row.nextErase, at.Add(time.Hour))
	}

	// A row another claim holds is skipped at once. The hold is on a pool of its own: the claim asks
	// f.pool for a connection on this goroutine (WARDYN_TEST_POOL_GUARD).
	due := at.Add(2 * time.Hour)
	holdPool, err := pgxpool.New(ctx, f.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer holdPool.Close()
	tx, err := holdPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // rolled back below
	if _, err := tx.Exec(ctx, `SELECT 1 FROM runner_resident_runs WHERE run_id = $1 FOR UPDATE`, run.ID); err != nil {
		t.Fatal(err)
	}
	quick, cancel := context.WithTimeout(ctx, 3*time.Second)
	got, err := f.st.ClaimResidentErase(quick, f.rn.ID, due, 8)
	cancel()
	if err != nil || len(got) != 0 {
		t.Fatalf("claim of a locked row = %v (%v), want none at once", got, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := claim(due); len(got) != 1 {
		t.Fatal("not claimed once the other claim let go")
	}

	long := strings.Repeat("é", 400)
	if err := f.st.NoteResidentEraseFailed(ctx, run.ID, long); err != nil {
		t.Fatal(err)
	}
	if row := f.residentRow(t, run.ID); row.lastReason == nil || len(*row.lastReason) > 512 || !strings.HasPrefix(long, *row.lastReason) {
		t.Fatalf("last reason %v, want the reason cut to 512 bytes on a rune boundary", row.lastReason)
	}
	if err := f.st.MarkResidentErased(ctx, run.ID, due); err != nil {
		t.Fatal(err)
	}
	if got := claim(due.Add(24 * time.Hour)); len(got) != 0 {
		t.Fatalf("claimed %v after the erase was confirmed", got)
	}
	if row := f.residentRow(t, run.ID); row.erased == nil || row.lastReason != nil {
		t.Fatalf("after the confirmation %+v, want erased with no failure reason", row)
	}
}

// Neither the run nor the runner of an open obligation can be deleted (M3).
func TestResidentObligation_RestrictsRunAndRunnerDelete(t *testing.T) {
	f := newActionFixture(t)
	ctx := t.Context()
	now := time.Now().UTC()
	run := f.residentRun(t, f.rn, types.RunStarting)
	if err := f.st.BeginResidentDelivery(ctx, run.ID, f.rn.ID, uuid.New(), now, now.Add(time.Minute), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for what, q := range map[string]string{
		"run":    `DELETE FROM agent_runs WHERE id = $1`,
		"runner": `DELETE FROM runners WHERE id = $1`,
	} {
		id := run.ID
		if what == "runner" {
			id = f.rn.ID
		}
		_, err := f.pool.Exec(ctx, q, id)
		if err == nil || !strings.Contains(err.Error(), "runner_resident_runs") {
			t.Fatalf("delete of the %s = %v, want the obligation's foreign key to refuse it", what, err)
		}
	}
}

// The org closes, unconfirmed, the obligation of a revoked runner and one long
// past not_after, each with its runner.resident.erase rows (one per resident
// grant, or one for the run) in the same transaction; a recent obligation stays
// open, and an audit failure leaves the obligation open too.
func TestAbandonResidentErase_RevokedAndExpiredAuditedInTx(t *testing.T) {
	f := newActionFixture(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	gone := claimed(t, f.st, ctx, f.rn.Owner)
	open := func(rn types.Runner, at, notAfter time.Time) types.AgentRun {
		t.Helper()
		run := f.residentRun(t, rn, types.RunStarting)
		if err := f.st.BeginResidentDelivery(ctx, run.ID, rn.ID, uuid.New(), at, at.Add(time.Minute), notAfter); err != nil {
			t.Fatal(err)
		}
		return run
	}
	revokedRun := open(gone, now, now.Add(time.Hour))
	grant, err := f.st.CreateGrant(ctx, types.CredentialGrant{ID: uuid.New(), RunID: revokedRun.ID, Delivery: types.GrantDeliveryResident,
		Spec: types.GrantSpec{Kind: types.GrantEnvSecret, Scope: json.RawMessage(`{"name":"TOKEN","secret_name":"ci-token"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.CreateGrant(ctx, types.CredentialGrant{ID: uuid.New(), RunID: revokedRun.ID,
		Spec: types.GrantSpec{Kind: types.GrantEnvSecret, Scope: json.RawMessage(`{"name":"OTHER","secret_name":"other"}`)}}); err != nil {
		t.Fatal(err)
	}
	expiredRun := open(f.rn, now.Add(-26*time.Hour), now.Add(-25*time.Hour))
	recentRun := open(f.rn, now.Add(-2*time.Hour), now.Add(-time.Hour))
	if _, err := f.st.RevokeRunner(ctx, gone.ID, now, "admin", ""); err != nil {
		t.Fatal(err)
	}

	failing := func(context.Context, types.AuditEvent) (types.AuditEvent, error) {
		return types.AuditEvent{}, errors.New("audit sealing failed")
	}
	if _, err := f.st.AbandonResidentErase(ctx, now, failing); err == nil {
		t.Fatal("an abandonment whose audit failed returned nil")
	}
	if n := f.count(t, `SELECT count(*) FROM runner_resident_runs WHERE abandoned_at IS NOT NULL`); n != 0 {
		t.Fatalf("%d obligations closed although their audit failed", n)
	}

	n, err := f.st.AbandonResidentErase(ctx, now, prepareSpoolTest)
	if err != nil || n != 2 {
		t.Fatalf("abandoned %d (%v), want the revoked and the expired", n, err)
	}
	for run, reason := range map[uuid.UUID]string{revokedRun.ID: store.ResidentAbandonRunnerRevoked, expiredRun.ID: store.ResidentAbandonExpired} {
		if row := f.residentRow(t, run); row.abandoned == nil || *row.abandoned != reason {
			t.Fatalf("run %s abandoned %v, want %s", run, row.abandoned, reason)
		}
	}
	if row := f.residentRow(t, recentRun.ID); row.abandoned != nil {
		t.Fatalf("an obligation inside its 24h grace was abandoned: %v", *row.abandoned)
	}
	if got := f.count(t, `SELECT count(*) FROM audit_events WHERE action = 'runner.resident.erase' AND run_id = $1 AND target = $2
		AND outcome = 'failure' AND actor = 'wardynd' AND data->>'outcome' = 'unconfirmed' AND data->>'reason' = $3
		AND data->>'grant_id' = $2 AND data->>'runner_id' = $4`, revokedRun.ID, grant.ID.String(), store.ResidentAbandonRunnerRevoked, gone.ID.String()); got != 1 {
		t.Fatalf("%d org runner.resident.erase rows for the revoked runner's resident grant, want one", got)
	}
	if got := f.count(t, `SELECT count(*) FROM audit_events WHERE action = 'runner.resident.erase' AND run_id = $1`, revokedRun.ID); got != 1 {
		t.Fatalf("%d rows for the revoked run, want one (its non-resident grant has none)", got)
	}
	if got := f.count(t, `SELECT count(*) FROM audit_events WHERE action = 'runner.resident.erase' AND run_id = $1 AND target = $1::text
		AND data->>'reason' = $2 AND NOT data ? 'grant_id'`, expiredRun.ID, store.ResidentAbandonExpired); got != 1 {
		t.Fatalf("%d run-level rows for the expired obligation with no resident grant, want one", got)
	}
	if n, err := f.st.AbandonResidentErase(ctx, now, prepareSpoolTest); err != nil || n != 0 {
		t.Fatalf("a second pass abandoned %d (%v), want none", n, err)
	}
	if got, err := f.st.ClaimResidentErase(ctx, gone.ID, now.Add(time.Hour), 8); err != nil || len(got) != 0 {
		t.Fatalf("an abandoned obligation was claimed: %v (%v)", got, err)
	}
}
