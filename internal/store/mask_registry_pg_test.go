// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

// The shared masking registry (migration 0124, internal/maskstore) against a
// real Postgres. Two registries over one pool and one KEK are two wardynds: each
// has its own cache, and only the database is shared. Nothing here starts the
// store's listener unless the test says so: every guarantee is proved with no
// notification delivered.

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/maskstore"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey/subjectkeytest"
)

const regAlice = "alice@example.com"

type regReplica struct {
	reg  *secretmask.Registry
	keys *subjectkey.Manager
	st   *maskstore.Store
	m    *maskmanifest.Manifests
}

// gatedKeys fails every key request while down is set: a KEK that does not answer.
type gatedKeys struct {
	maskstore.Keys
	down atomic.Bool
}

var errKEKDown = errors.New("the key service is down")

func (g *gatedKeys) Current(ctx context.Context, owner, purpose string) (int, []byte, error) {
	if g.down.Load() {
		return 0, nil, errKEKDown
	}
	return g.Keys.Current(ctx, owner, purpose)
}

func newRegReplica(t *testing.T, pool *pgxpool.Pool, k kek.KEK) regReplica {
	t.Helper()
	reg := secretmask.NewRegistry()
	keys := subjectkeytest.Manager(pool, k)
	return regReplica{reg: reg, keys: keys, st: maskstore.New(pool, keys, reg), m: maskmanifest.New(pool, keys, reg)}
}

// read is one read of the table: what a masking consumer's Fresh does.
func (r regReplica) read(t *testing.T) {
	t.Helper()
	if err := r.st.Fresh(t.Context(), time.Now()); err != nil {
		t.Fatalf("read the shared registry: %v", err)
	}
}

// dispatchedRun persists a run owned by owner with a complete (empty) manifest,
// which is what lets its per-run values be sealed under the owner's key.
func dispatchedRun(t *testing.T, pool *pgxpool.Pool, r regReplica, owner string) uuid.UUID {
	t.Helper()
	run := manifestRun(t, pool, owner)
	if err := r.m.Start(t.Context(), run, owner); err != nil {
		t.Fatal(err)
	}
	if err := r.m.Complete(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	return run
}

func regMasks(r regReplica, run uuid.UUID, value string) bool {
	return bytes.Contains(r.reg.Masker(run).Mask([]byte("x "+value+" y")), []byte("<secret-hidden>"))
}

func liveRows(t *testing.T, pool *pgxpool.Pool, where string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM mask_values WHERE NOT tombstone AND `+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A value registered on A is masked on B once B reads the table, for a run
// value and for a credential value, and a restarted replica masks both with no
// help from the process that registered them.
func TestPG_MaskRegistry_ARegistrationOnAIsMaskedOnBAndAfterARestart(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	k := localKEK(t)
	a, b := newRegReplica(t, pool, k), newRegReplica(t, pool, k)
	run := dispatchedRun(t, pool, a, regAlice)

	if err := a.reg.Add(run, []byte("injected-after-dispatch-value")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := a.reg.AddGlobalUntil(regAlice, "aws-sso", time.Now(), time.Now().Add(time.Hour), []byte("sso-access-token-value"), []byte("sso-refresh-token-value")); err != nil {
		t.Fatalf("AddGlobalUntil: %v", err)
	}
	if regMasks(b, run, "injected-after-dispatch-value") || regMasks(b, run, "sso-refresh-token-value") {
		t.Fatal("B masked a value before it read the table")
	}
	b.read(t)
	for _, v := range []string{"injected-after-dispatch-value", "sso-access-token-value", "sso-refresh-token-value"} {
		if !regMasks(b, run, v) {
			t.Errorf("B does not mask %q after reading the table", v)
		}
	}
	// The value is run-scoped: another run's output is not masked for it.
	if regMasks(b, uuid.New(), "injected-after-dispatch-value") {
		t.Error("a per-run value is masked on another run")
	}
	if !regMasks(b, uuid.New(), "sso-access-token-value") {
		t.Error("a credential value is not masked on every run")
	}

	restarted := newRegReplica(t, pool, k)
	restarted.read(t)
	for _, v := range []string{"injected-after-dispatch-value", "sso-access-token-value", "sso-refresh-token-value"} {
		if !regMasks(restarted, run, v) {
			t.Errorf("a restarted replica does not mask %q", v)
		}
	}
	// Nothing in the table holds a plaintext value, and every row has its owner.
	rows, err := pool.Query(t.Context(), `SELECT sealed, owner FROM mask_values WHERE NOT tombstone`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var sealed []byte
		var owner string
		if err := rows.Scan(&sealed, &owner); err != nil {
			t.Fatal(err)
		}
		n++
		if owner != regAlice || bytes.Contains(sealed, []byte("token-value")) || bytes.Contains(sealed, []byte("after-dispatch")) {
			t.Errorf("a mask_values row has owner %q or holds plaintext", owner)
		}
	}
	if n != 3 {
		t.Errorf("mask_values has %d live rows, want 3", n)
	}
}

func pendingOn(t *testing.T, pool *pgxpool.Pool, like string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE $1`, like).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A registering transaction that took its generation and has not committed
// holds every later one back: a second registration cannot commit, so a reader
// that has advanced past the second can never have skipped the first. A plain
// sequence passes every single-writer test and fails this one: T2 would commit
// generation 2 while T1 is open, B would advance to 2, and T1's value would
// never be read.
func TestPG_MaskRegistry_ALateCommittingValueIsNeverSkipped(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	k := localKEK(t)
	ctx := t.Context()
	t1, t2, b := newRegReplica(t, pool, k), newRegReplica(t, pool, k), newRegReplica(t, pool, k)
	r1, r2 := dispatchedRun(t, pool, t1, regAlice), dispatchedRun(t, pool, t1, regAlice)

	// An outside transaction holds r1's manifest row, which T1's own transaction
	// needs right after it takes its generation: T1 is held open mid-flight.
	hold, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := hold.Exec(ctx, `SELECT 1 FROM run_mask_manifest WHERE run_id=$1 FOR UPDATE`, r1); err != nil {
		t.Fatal(err)
	}
	done1, done2 := make(chan error, 1), make(chan error, 1)
	go func() { done1 <- t1.reg.Add(r1, []byte("the-late-committing-value")) }()
	waitFor(t, "T1 to hold its generation and wait on the manifest row", func() bool {
		return pendingOn(t, pool, "%FOR SHARE%")
	})
	go func() { done2 <- t2.reg.Add(r2, []byte("the-second-committing-value")) }()
	waitFor(t, "T2 to queue on the generation", func() bool {
		return pendingOn(t, pool, "%UPDATE mask_gen%")
	})

	// T2 has not committed, so B reads neither value and its cursor is below both.
	b.read(t)
	if regMasks(b, r1, "the-late-committing-value") || regMasks(b, r2, "the-second-committing-value") {
		t.Fatal("B read a value that is not committed")
	}
	select {
	case err := <-done2:
		t.Fatalf("T2 committed (%v) while T1 was open: generation order is not commit order", err)
	case err := <-done1:
		t.Fatalf("T1 finished (%v) while its row was held", err)
	case <-time.After(300 * time.Millisecond):
	}

	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done1; err != nil {
		t.Fatalf("T1: %v", err)
	}
	if err := <-done2; err != nil {
		t.Fatalf("T2: %v", err)
	}
	b.read(t)
	if !regMasks(b, r1, "the-late-committing-value") || !regMasks(b, r2, "the-second-committing-value") {
		t.Fatal("B skipped a value that committed late")
	}
	var g1, g2 int64
	if err := pool.QueryRow(ctx, `SELECT gen FROM mask_values WHERE run_id=$1 AND NOT tombstone`, r1).Scan(&g1); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT gen FROM mask_values WHERE run_id=$1 AND NOT tombstone`, r2).Scan(&g2); err != nil {
		t.Fatal(err)
	}
	if g1 >= g2 {
		t.Errorf("generations %d (T1) and %d (T2): commit order is not generation order", g1, g2)
	}
}

// Two replicas committing one value for one run at once: the loser's insert does
// nothing and it records no row of its own, so a later full reload (a replica away
// past the prune horizon) keeps masking the value from the winner's row.
func TestPG_MaskRegistry_ARacedRunValueIsStillMaskedAfterAFullReload(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	k := localKEK(t)
	ctx := t.Context()
	t1, t2 := newRegReplica(t, pool, k), newRegReplica(t, pool, k)
	run := dispatchedRun(t, pool, t1, regAlice)
	const value = "the-raced-value"
	t2.read(t) // t2 is loaded, with a cursor below everything that follows

	// T1 is held open mid-flight, after its dedup check and its generation, so
	// T2's dedup check also finds no committed row.
	hold, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := hold.Exec(ctx, `SELECT 1 FROM run_mask_manifest WHERE run_id=$1 FOR UPDATE`, run); err != nil {
		t.Fatal(err)
	}
	done1, done2 := make(chan error, 1), make(chan error, 1)
	go func() { done1 <- t1.reg.Add(run, []byte(value)) }()
	waitFor(t, "T1 to hold its generation and wait on the manifest row", func() bool {
		return pendingOn(t, pool, "%FOR SHARE%")
	})
	go func() { done2 <- t2.reg.Add(run, []byte(value)) }()
	waitFor(t, "T2 to queue on the generation", func() bool {
		return pendingOn(t, pool, "%UPDATE mask_gen%")
	})
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done1; err != nil {
		t.Fatalf("T1: %v", err)
	}
	if err := <-done2; err != nil {
		t.Fatalf("T2: %v", err)
	}
	if n := liveRows(t, pool, `run_id=$1`, run); n != 1 {
		t.Fatalf("%d live rows for the raced value, want the winner's one", n)
	}

	// The loser is away past the prune horizon: its next read reloads the table.
	if _, err := pool.Exec(ctx, `UPDATE mask_gen SET pruned = gen`); err != nil {
		t.Fatal(err)
	}
	t2.read(t)
	if !regMasks(t2, run, value) {
		t.Error("the loser stopped masking a live run's value after a full reload")
	}
}

// Every eviction is a tombstone with no ciphertext: EvictGlobal, retirement past
// the grace, a run's purge, and an erasure each leave no row that can be opened,
// replicas drop the value when they read it, and the row itself is gone after
// the prune horizon, a replica that was away across the prune reloading the table.
func TestPG_MaskRegistry_EvictionsAreTombstonesWithNoCiphertext(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	k := localKEK(t)
	ctx := t.Context()
	a, b, c := newRegReplica(t, pool, k), newRegReplica(t, pool, k), newRegReplica(t, pool, k)
	run := dispatchedRun(t, pool, a, regAlice)
	now := time.Now()

	mustNil := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	mustNil("per-run", a.reg.Add(run, []byte("per-run-value-to-purge")))
	mustNil("evicted credential", a.reg.AddGlobal(regAlice, "deleted-credential", now, []byte("deleted-credential-value")))
	mustNil("replaced credential", a.reg.AddGlobal(regAlice, "rotating-credential", now, []byte("rotating-old-value-1")))
	mustNil("replacement", a.reg.AddGlobal(regAlice, "rotating-credential", now, []byte("rotating-new-value-2")))
	mustNil("erased person", a.reg.AddGlobal("bob@example.com", "bobs-credential", now, []byte("bobs-credential-value")))
	for _, r := range []regReplica{b, c} {
		r.read(t)
	}
	for _, v := range []string{"per-run-value-to-purge", "deleted-credential-value", "rotating-old-value-1", "rotating-new-value-2", "bobs-credential-value"} {
		if !regMasks(b, run, v) {
			t.Fatalf("B does not mask %q before any eviction", v)
		}
	}
	// The replaced value is retired, not dropped: it stays sealed and masked.
	if liveRows(t, pool, `name = 'rotating-credential' AND retired_at IS NOT NULL`) != 1 {
		t.Fatal("the replaced credential value was not retired")
	}

	// EvictGlobal: the credential's current value is tombstoned at once.
	mustNil("EvictGlobal", a.reg.EvictGlobal(regAlice, "deleted-credential", now))
	if n := liveRows(t, pool, `name = 'deleted-credential'`); n != 0 {
		t.Fatalf("%d live rows after EvictGlobal", n)
	}
	assertNoCiphertext := func(what string) {
		t.Helper()
		var bad int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM mask_values WHERE tombstone AND (sealed IS NOT NULL OR digest IS NOT NULL OR owner <> '' OR name <> '')`).Scan(&bad); err != nil || bad != 0 {
			t.Fatalf("%s: %d tombstones hold ciphertext, a digest, an owner or a name (err %v)", what, bad, err)
		}
	}
	assertNoCiphertext("after EvictGlobal")
	b.read(t)
	if !regMasks(b, run, "deleted-credential-value") {
		t.Error("an evicted credential's value stops being masked at once: it should stay masked until the grace")
	}

	// Retirement: past the grace the retired value is swept.
	swept, err := a.reg.SweepPersisted(ctx, now.Add(time.Hour))
	if err != nil || swept != 1 {
		t.Fatalf("SweepPersisted = %d, %v; want the one retired value", swept, err)
	}
	b.read(t)
	for _, v := range []string{"rotating-old-value-1"} {
		if regMasks(b, run, v) {
			t.Errorf("B still masks %q after the sweep tombstoned it", v)
		}
	}
	if !regMasks(b, run, "rotating-new-value-2") {
		t.Error("the current replacement stopped being masked")
	}

	// A run's purge tombstones its per-run values and deletes its manifest.
	mustNil("PurgeRuns", a.reg.PurgeRuns(ctx, []uuid.UUID{run}))
	if n := liveRows(t, pool, `run_id IS NOT NULL`); n != 0 {
		t.Errorf("%d live per-run rows after the purge", n)
	}
	var manifests int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_mask_manifest WHERE run_id=$1`, run).Scan(&manifests); err != nil || manifests != 0 {
		t.Errorf("manifest rows after the purge = %d (err %v)", manifests, err)
	}
	b.read(t)
	if regMasks(b, run, "per-run-value-to-purge") {
		t.Error("B still masks a purged run's value")
	}

	// Erasure: every row of the person goes, per-run and global, whatever its bucket.
	left, err := a.reg.EraseOwner(ctx, "bob@example.com")
	if err != nil || left != 0 {
		t.Fatalf("EraseOwner = %d, %v", left, err)
	}
	if n := liveRows(t, pool, `owner = 'bob@example.com'`); n != 0 {
		t.Errorf("%d live rows for an erased person", n)
	}
	assertNoCiphertext("after erasure")
	b.read(t)
	if regMasks(b, run, "bobs-credential-value") {
		t.Error("B still masks an erased person's value")
	}

	// After the horizon the tombstones are deleted. C, which has not read since
	// before any of this, finds its cursor below the prune horizon and reloads.
	if _, err := pool.Exec(ctx, `UPDATE mask_values SET updated_at = now() - make_interval(secs => $1) WHERE tombstone`, (2 * maskstore.TombstoneHorizon).Seconds()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.reg.SweepPersisted(ctx, now); err != nil {
		t.Fatal(err)
	}
	var tombstones int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM mask_values WHERE tombstone`).Scan(&tombstones); err != nil || tombstones != 0 {
		t.Fatalf("tombstones after the horizon = %d (err %v), want 0", tombstones, err)
	}
	c.read(t)
	for _, v := range []string{"per-run-value-to-purge", "rotating-old-value-1", "bobs-credential-value"} {
		if regMasks(c, run, v) {
			t.Errorf("C, which never saw the tombstones, still masks %q after its reload", v)
		}
	}
	if !regMasks(c, run, "rotating-new-value-2") {
		t.Error("C's reload lost the live value")
	}
}

// With no listener and no notification, a replica still converges: the read a
// consumer makes before masking is the whole mechanism.
func TestPG_MaskRegistry_ConvergesWithNoNotification(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	k := localKEK(t)
	a, b := newRegReplica(t, pool, k), newRegReplica(t, pool, k)
	if err := a.reg.AddGlobal(regAlice, "unannounced", time.Now(), []byte("unannounced-value-one")); err != nil {
		t.Fatal(err)
	}
	// A notification would not have arrived: B has no listener at all.
	if err := b.st.Fresh(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if !regMasks(b, uuid.New(), "unannounced-value-one") {
		t.Fatal("B did not converge on a commit it was never told about")
	}
	// A value committed after B's read is invisible to a read that began before
	// it, and visible to the next: Fresh waits for a read that STARTED after the
	// arrival it is asked about.
	early := time.Now()
	if err := a.reg.AddGlobal(regAlice, "unannounced", time.Now(), []byte("unannounced-value-two")); err != nil {
		t.Fatal(err)
	}
	if err := b.st.Fresh(t.Context(), early); err != nil {
		t.Fatal(err)
	}
	if err := b.st.Fresh(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if !regMasks(b, uuid.New(), "unannounced-value-two") {
		t.Fatal("B did not read a commit that preceded its Fresh")
	}
}

// A listener turns a commit into a prompt read, but only as a hint: it never
// reads a value out of the notification, which carries a number.
func TestPG_MaskRegistry_AListenerReadsOnANotification(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	k := localKEK(t)
	a, b := newRegReplica(t, pool, k), newRegReplica(t, pool, k)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b.st.Start(ctx)
	if err := a.reg.AddGlobal(regAlice, "announced", time.Now(), []byte("announced-value-one")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "B's listener to read the commit", func() bool { return regMasks(b, uuid.New(), "announced-value-one") })
}

// A registration that cannot be made fails, and the failed value is not in the
// registry: the retry persists it instead of finding it already known.
func TestPG_MaskRegistry_AFailedRegistrationFailsTheAddAndIsRetryable(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	k := localKEK(t)
	reg := secretmask.NewRegistry()
	keys := &gatedKeys{Keys: subjectkeytest.Manager(pool, k)}
	maskstore.New(pool, keys, reg)
	m := maskmanifest.New(pool, keys, reg)
	run := manifestRun(t, pool, regAlice)
	if err := m.Start(t.Context(), run, regAlice); err != nil {
		t.Fatal(err)
	}
	if err := m.Complete(t.Context(), run); err != nil {
		t.Fatal(err)
	}

	keys.down.Store(true)
	if err := reg.Add(run, []byte("value-with-the-kek-down")); !errors.Is(err, errKEKDown) {
		t.Fatalf("Add with the key service down = %v, want it to fail", err)
	}
	if err := reg.AddGlobal(regAlice, "cred", time.Now(), []byte("global-with-the-kek-down")); !errors.Is(err, errKEKDown) {
		t.Fatalf("AddGlobal with the key service down = %v, want it to fail", err)
	}
	if bytes.Contains(reg.Masker(run).Mask([]byte("value-with-the-kek-down")), []byte("<secret-hidden>")) {
		t.Error("a value whose registration failed is in the registry, so a retry would skip persisting it")
	}
	if n := liveRows(t, pool, `TRUE`); n != 0 {
		t.Errorf("%d rows after failed registrations", n)
	}
	keys.down.Store(false)
	if err := reg.Add(run, []byte("value-with-the-kek-down")); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if n := liveRows(t, pool, `run_id IS NOT NULL`); n != 1 {
		t.Errorf("%d per-run rows after the retry, want 1", n)
	}

	// Postgres itself unreachable: a registration through a closed pool fails.
	closed := pgxpoolCopy(t, pool)
	cr := secretmask.NewRegistry()
	maskstore.New(closed, subjectkeytest.Manager(closed, k), cr)
	closed.Close()
	if err := cr.AddGlobal(regAlice, "cred", time.Now(), []byte("global-with-postgres-down")); err == nil {
		t.Error("AddGlobal with Postgres down succeeded")
	}
	if err := cr.Add(run, []byte("per-run-with-postgres-down")); err == nil {
		t.Error("Add with Postgres down succeeded")
	}
}

func pgxpoolCopy(t *testing.T, pool *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.NewWithConfig(t.Context(), pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A fenced run takes no further value, a run with no manifest keeps its value in
// this process only, and the operator namespace has no subject key to seal under.
func TestPG_MaskRegistry_FencedLegacyAndOperatorRegistrations(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	k := localKEK(t)
	ctx := t.Context()
	a := newRegReplica(t, pool, k)

	fenced := dispatchedRun(t, pool, a, regAlice)
	if _, err := a.m.FenceSubject(ctx, regAlice); err != nil {
		t.Fatal(err)
	}
	if err := a.reg.Add(fenced, []byte("value-after-the-fence")); !errors.Is(err, maskstore.ErrFenced) {
		t.Errorf("Add on a fenced run = %v, want ErrFenced", err)
	}

	legacy := manifestRun(t, pool, regAlice) // dispatched before manifests: no row
	if err := a.reg.Add(legacy, []byte("legacy-run-injected-value")); err != nil {
		t.Errorf("Add on a legacy run = %v, want the value kept locally", err)
	}
	if !regMasks(a, legacy, "legacy-run-injected-value") {
		t.Error("a legacy run's value is not masked in this process")
	}
	if err := a.reg.AddGlobal("", "operator-cred", time.Now(), []byte("operator-namespace-value")); err != nil {
		t.Errorf("AddGlobal in the operator namespace = %v, want it kept locally", err)
	}
	if n := liveRows(t, pool, `TRUE`); n != 0 {
		t.Errorf("%d rows for values that have no subject key", n)
	}
}

// A value is sealed under its owner's key, bound to its row: destroy the key and
// a restarted replica masks nothing of the owner's, and a row whose bytes are
// moved to another row does not open.
func TestPG_MaskRegistry_SealingBindsOwnerAndRow(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	k := localKEK(t)
	ctx := t.Context()
	a := newRegReplica(t, pool, k)
	aliceRun, bobRun := dispatchedRun(t, pool, a, regAlice), dispatchedRun(t, pool, a, "bob@example.com")
	if err := a.reg.AddGlobal(regAlice, "cred-one", time.Now(), []byte("sealed-credential-one")); err != nil {
		t.Fatal(err)
	}
	if err := a.reg.AddGlobal(regAlice, "cred-two", time.Now(), []byte("sealed-credential-two")); err != nil {
		t.Fatal(err)
	}
	if err := a.reg.AddGlobal("bob@example.com", "cred-bob", time.Now(), []byte("bobs-sealed-credential")); err != nil {
		t.Fatal(err)
	}
	// Move cred-one's ciphertext onto cred-two's row: it must not open there.
	if _, err := pool.Exec(ctx, `UPDATE mask_values SET sealed = (SELECT sealed FROM mask_values WHERE name='cred-one') WHERE name='cred-two'`); err != nil {
		t.Fatal(err)
	}
	r := newRegReplica(t, pool, k)
	r.read(t)
	if regMasks(r, uuid.New(), "sealed-credential-two") {
		t.Error("a row opened with another row's ciphertext")
	}
	if !regMasks(r, uuid.New(), "sealed-credential-one") || !regMasks(r, uuid.New(), "bobs-sealed-credential") {
		t.Error("an intact row did not open")
	}
	if r.m.Covered(ctx, aliceRun) {
		t.Error("a run of the owner of a value that does not open is still covered")
	}
	if !r.m.Covered(ctx, bobRun) {
		t.Error("another person's run lost its coverage")
	}
	if _, err := a.keys.Destroy(ctx, regAlice, subjectkey.PurposeCred); err != nil {
		t.Fatal(err)
	}
	after := newRegReplica(t, pool, k)
	after.read(t)
	if regMasks(after, uuid.New(), "sealed-credential-one") {
		t.Error("a value opens after its owner's key was destroyed")
	}
	if !regMasks(after, uuid.New(), "bobs-sealed-credential") {
		t.Error("destroying one person's key lost another's values")
	}
}

// A live value that can never be opened again fails its runs closed instead of
// being dropped from the corpus: a per-run value fences its run, a credential
// value every run of its owner. The row is tombstoned with the fence, so a
// later full read does not fence a run the owner starts afterwards.
func TestPG_MaskStore_UnopenableLiveRowFencesRun(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	k := localKEK(t)
	ctx := t.Context()
	const bob = "bob@example.com"
	a := newRegReplica(t, pool, k)
	aliceRun, aliceOther, bobRun := dispatchedRun(t, pool, a, regAlice), dispatchedRun(t, pool, a, regAlice), dispatchedRun(t, pool, a, bob)
	if err := a.reg.Add(aliceRun, []byte("alice-runtime-value")); err != nil {
		t.Fatal(err)
	}
	if err := a.reg.AddGlobal(bob, "bob-cred", time.Now(), []byte("bobs-credential-value")); err != nil {
		t.Fatal(err)
	}

	// Alice's key is destroyed: her runtime value fences the run it belongs to.
	if _, err := a.keys.Destroy(ctx, regAlice, subjectkey.PurposeCred); err != nil {
		t.Fatal(err)
	}
	b := newRegReplica(t, pool, k)
	b.read(t)
	if b.m.Covered(ctx, aliceRun) {
		t.Error("a run whose value can never open again is still covered")
	}
	if !b.m.Covered(ctx, aliceOther) || !b.m.Covered(ctx, bobRun) {
		t.Error("a run with nothing unopenable lost its coverage")
	}
	if n := liveRows(t, pool, `run_id = $1`, aliceRun); n != 0 {
		t.Errorf("%d live rows left for the unopenable value, want it tombstoned", n)
	}

	// Bob's credential value is damaged: every run of his is fenced.
	if _, err := pool.Exec(ctx,
		`UPDATE mask_values SET sealed = set_byte(sealed, octet_length(sealed)-1, get_byte(sealed, octet_length(sealed)-1) # 1) WHERE name = 'bob-cred'`); err != nil {
		t.Fatal(err)
	}
	c := newRegReplica(t, pool, k)
	c.read(t)
	if c.m.Covered(ctx, bobRun) {
		t.Error("a run of the owner of a damaged credential value is still covered")
	}

	// A run bob starts afterwards is not fenced by a later replica's full read.
	later := dispatchedRun(t, pool, a, bob)
	d := newRegReplica(t, pool, k)
	d.read(t)
	if !d.m.Covered(ctx, later) {
		t.Error("a full read fenced a run started after the damaged value was handled")
	}

	// A credentials erase retires the owner's credential values before it destroys the key: a
	// replica that never cached one skips it, so the run the person starts under the next key
	// generation is not fenced for a value it cannot hold.
	const carol = "carol@example.com"
	if err := a.reg.AddGlobal(carol, "carol-cred", time.Now(), []byte("carols-credential-value")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.m.FenceSubject(ctx, carol); err != nil {
		t.Fatal(err)
	}
	if err := a.reg.RetireOwnerGlobals(ctx, carol, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.keys.Destroy(ctx, carol, subjectkey.PurposeCred); err != nil {
		t.Fatal(err)
	}
	afterErase := dispatchedRun(t, pool, a, carol)
	e := newRegReplica(t, pool, k)
	e.read(t)
	if !e.m.Covered(ctx, afterErase) {
		t.Error("a replica that never cached a retired credential value fenced a run started after the erase")
	}
}
