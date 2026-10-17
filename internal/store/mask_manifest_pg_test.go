// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

// The run masking manifest (migration 0109, internal/maskmanifest) against a
// real Postgres. Two Manifests over one pool and one KEK are two wardynds: each
// has its own registry and its own cache, and only the database is shared.

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey/subjectkeytest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// replica is one wardynd's masking state over a shared database.
type replica struct {
	reg  *secretmask.Registry
	keys *subjectkey.Manager
	m    *maskmanifest.Manifests
}

func newReplica(t *testing.T, pool *pgxpool.Pool, k kek.KEK) replica {
	t.Helper()
	reg := secretmask.NewRegistry()
	keys := subjectkeytest.Manager(pool, k)
	return replica{reg: reg, keys: keys, m: maskmanifest.New(pool, keys, reg)}
}

func localKEK(t *testing.T) kek.KEK {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	k, err := kek.NewLocalPurpose(id, kek.PurposeCred)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// manifestRun persists a RUNNING run owned by owner and returns its id.
func manifestRun(t *testing.T, pool *pgxpool.Pool, owner string) uuid.UUID {
	t.Helper()
	r := newRun(types.RunRunning)
	r.CreatedBy = owner
	return persistRun(t, context.Background(), pool, r).ID
}

// dispatched is what dispatch does: begin, append every rendering, complete.
func dispatched(t *testing.T, r replica, run uuid.UUID, owner string, values ...string) {
	t.Helper()
	ctx := context.Background()
	if err := r.m.Start(ctx, run, owner); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for _, v := range values {
		if err := r.m.Append(ctx, run, []byte(v)); err != nil {
			t.Fatalf("Append(%q): %v", v, err)
		}
	}
	if err := r.m.Complete(ctx, run); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func masks(r replica, run uuid.UUID, value string) bool {
	return bytes.Contains(r.reg.Masker(run).Mask([]byte("x "+value+" y")), []byte("<secret-hidden>"))
}

// A value committed at dispatch is masked by a replica that never saw it, and
// stays masked after a restart whatever the secret has been rotated to since.
func TestPG_MaskManifest_SurvivesRestartAndServesAnotherReplica(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	k := localKEK(t)
	ctx := context.Background()
	const owner = "alice@example.com"
	run := manifestRun(t, pool, owner)

	a := newReplica(t, pool, k)
	dispatched(t, a, run, owner, "value-one-at-dispatch", "Basic dmFsdWUtb25l")
	if !a.m.Covered(ctx, run) {
		t.Fatal("the dispatching replica does not hold what it just committed")
	}

	// B never dispatched anything: healthy A, an upload landing on B.
	b := newReplica(t, pool, k)
	if masks(b, run, "value-one-at-dispatch") {
		t.Fatal("B masked a value before it loaded the manifest")
	}
	if !b.m.Covered(ctx, run) {
		t.Fatal("B cannot certify a complete manifest it did not write (it must need no watcher lease)")
	}
	for _, v := range []string{"value-one-at-dispatch", "Basic dmFsdWUtb25l"} {
		if !masks(b, run, v) {
			t.Errorf("B does not mask %q after loading the manifest", v)
		}
	}

	// "Rotated after dispatch, restarted before upload": the secret now says
	// value-two, a fresh process loads the manifest and must still hold value-one.
	restarted := newReplica(t, pool, k)
	if !restarted.m.Covered(ctx, run) {
		t.Fatal("a restarted replica is not covered")
	}
	if !masks(restarted, run, "value-one-at-dispatch") {
		t.Error("the pre-rotation value is not masked after a restart")
	}
	if masks(restarted, run, "value-two-after-rotation") {
		t.Error("a value the run never received is masked: the manifest re-resolved by name")
	}
}

// Only a complete manifest covers. A run that never began one, one that began
// and did not finish, and a value injected after a restart are all uncovered.
func TestPG_MaskManifest_OnlyACompleteManifestCovers(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	k := localKEK(t)
	ctx := context.Background()
	const owner = "alice@example.com"
	a := newReplica(t, pool, k)

	legacy := manifestRun(t, pool, owner)
	if a.m.Covered(ctx, legacy) {
		t.Error("a run with no manifest row is covered")
	}
	// A restarted replica's injection-time Add puts a value in the registry; it
	// is not proof the corpus is whole.
	a.reg.Add(legacy, []byte("injected-after-restart"))
	if a.m.Covered(ctx, legacy) {
		t.Error("an injection-time Add covered an unmarked run")
	}
	if !masks(a, legacy, "injected-after-restart") {
		t.Error("the injected value is no longer masked: an uncovered run's registry must keep what it holds")
	}
	if err := a.m.Append(ctx, legacy, []byte("a-token-minted-after-the-upgrade")); err != maskmanifest.ErrNoManifest {
		t.Errorf("Append on a legacy run = %v, want ErrNoManifest", err)
	}
	if err := a.m.Complete(ctx, legacy); err != maskmanifest.ErrNoManifest {
		t.Errorf("Complete on a legacy run = %v, want ErrNoManifest", err)
	}

	partial := manifestRun(t, pool, owner)
	if err := a.m.Start(ctx, partial, owner); err != nil {
		t.Fatal(err)
	}
	if err := a.m.Append(ctx, partial, []byte("committed-but-not-complete")); err != nil {
		t.Fatal(err)
	}
	if a.m.Covered(ctx, partial) {
		t.Error("a manifest that is not complete covers its run")
	}
	if err := a.m.Start(ctx, partial, ""); err != maskmanifest.ErrNoOwner {
		t.Errorf("Begin without an owner = %v, want ErrNoOwner", err)
	}
}

// A rendering appended on one replica after another loaded the manifest (a
// token's renewal, widening or resume) is masked on the other from its next
// admission, never only on the replica that minted it.
func TestPG_MaskManifest_AppendOnOneReplicaReachesTheOther(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	k := localKEK(t)
	ctx := context.Background()
	const owner = "alice@example.com"
	run := manifestRun(t, pool, owner)

	a, b := newReplica(t, pool, k), newReplica(t, pool, k)
	dispatched(t, a, run, owner, "first-run-token-value")
	if !b.m.Covered(ctx, run) {
		t.Fatal("B is not covered")
	}
	if err := a.m.Append(ctx, run, []byte("renewed-run-token-value")); err != nil {
		t.Fatalf("append the renewal: %v", err)
	}
	if masks(b, run, "renewed-run-token-value") {
		t.Fatal("B masked the renewal before it was asked again")
	}
	if !b.m.Covered(ctx, run) {
		t.Fatal("B is not covered after the renewal")
	}
	if !masks(b, run, "renewed-run-token-value") {
		t.Error("B does not mask the token renewed on A after its next admission check")
	}
	// The minting replica kept its own copy exact: no reload is needed to cover.
	if !a.m.Covered(ctx, run) || !masks(a, run, "renewed-run-token-value") {
		t.Error("the minting replica lost its own append")
	}
}

// Concurrent appends from two replicas take distinct ordinals: no row is lost
// and none is overwritten.
func TestPG_MaskManifest_ConcurrentAppendsLoseNothing(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	k := localKEK(t)
	ctx := context.Background()
	const owner = "alice@example.com"
	run := manifestRun(t, pool, owner)
	a, b := newReplica(t, pool, k), newReplica(t, pool, k)
	if err := a.m.Start(ctx, run, owner); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i, r := range []replica{a, b, a, b, a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.m.Append(ctx, run, []byte("concurrent-value-"+string(rune('a'+i))), []byte("concurrent-twin-"+string(rune('a'+i)))); err != nil {
				t.Errorf("append %d: %v", i, err)
			}
		}()
	}
	wg.Wait()
	if err := a.m.Complete(ctx, run); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_mask_values WHERE run_id=$1`, run).Scan(&n); err != nil || n != 12 {
		t.Fatalf("value rows = %d (err %v), want 12", n, err)
	}
	fresh := newReplica(t, pool, k)
	if !fresh.m.Covered(ctx, run) {
		t.Fatal("a fresh replica cannot open every concurrently written row")
	}
	for i := range 6 {
		for _, p := range []string{"concurrent-value-", "concurrent-twin-"} {
			if !masks(fresh, run, p+string(rune('a'+i))) {
				t.Errorf("%s%c is not masked after loading", p, 'a'+i)
			}
		}
	}
}

// A value is sealed in the run owner's key domain, bound to its run and ordinal:
// a row moved to another ordinal or run, or opened after its owner's key is
// destroyed, does not open, and the run is then uncovered.
func TestPG_MaskManifest_SealingBindsOwnerRunAndOrdinal(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	k := localKEK(t)
	ctx := context.Background()
	const owner = "alice@example.com"
	run, other := manifestRun(t, pool, owner), manifestRun(t, pool, owner)
	a := newReplica(t, pool, k)
	dispatched(t, a, run, owner, "sealed-value-zero", "sealed-value-one")
	dispatched(t, a, other, owner, "other-run-value")

	rows, err := pool.Query(ctx, `SELECT sealed FROM run_mask_values`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var sealed []byte
		if err := rows.Scan(&sealed); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(sealed, []byte("sealed-value")) || bytes.Contains(sealed, []byte("other-run-value")) {
			t.Fatal("a value row holds its plaintext")
		}
	}
	rows.Close()
	var rowOwner string
	if err := pool.QueryRow(ctx, `SELECT DISTINCT owner FROM run_mask_values WHERE run_id=$1`, run).Scan(&rowOwner); err != nil || rowOwner != owner {
		t.Fatalf("value rows owner = %q (err %v), want the run owner %q", rowOwner, err, owner)
	}

	// Swap the sealed bytes of ordinals 0 and 1.
	if _, err := pool.Exec(ctx, `
		UPDATE run_mask_values v SET sealed = s.sealed FROM
		  (SELECT ordinal, sealed FROM run_mask_values WHERE run_id=$1) s
		WHERE v.run_id=$1 AND v.ordinal = 1 - s.ordinal`, run); err != nil {
		t.Fatal(err)
	}
	if newReplica(t, pool, k).m.Covered(ctx, run) {
		t.Error("a manifest whose rows were swapped between ordinals still opens")
	}

	// A row copied under another run id does not open there.
	if _, err := pool.Exec(ctx, `UPDATE run_mask_values SET sealed = (SELECT sealed FROM run_mask_values WHERE run_id=$1 AND ordinal=0) WHERE run_id=$2 AND ordinal=0`, run, other); err != nil {
		t.Fatal(err)
	}
	if newReplica(t, pool, k).m.Covered(ctx, other) {
		t.Error("a value row copied from another run still opens")
	}

	// Destroying the owner's key leaves a healthy run's rows undecryptable.
	intact := manifestRun(t, pool, owner)
	dispatched(t, a, intact, owner, "custody-follows-the-person")
	if _, err := a.keys.Destroy(ctx, owner, subjectkey.PurposeCred); err != nil {
		t.Fatal(err)
	}
	if newReplica(t, pool, k).m.Covered(ctx, intact) {
		t.Error("a manifest opens after its owner's cred key was destroyed")
	}
}

// The erasure primitive fences every manifest of a subject and deletes their
// value rows in one transaction; every replica then refuses the run, and drops
// what it had loaded. Another subject's manifest is untouched.
func TestPG_MaskManifest_FenceSubjectFencesThenDeletes(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	k := localKEK(t)
	ctx := context.Background()
	const alice, bob = "alice@example.com", "bob@example.com"
	ar1, ar2, br := manifestRun(t, pool, alice), manifestRun(t, pool, alice), manifestRun(t, pool, bob)

	a, b := newReplica(t, pool, k), newReplica(t, pool, k)
	dispatched(t, a, ar1, alice, "alice-first-run-secret")
	dispatched(t, a, ar2, alice, "alice-second-run-secret")
	dispatched(t, a, br, bob, "bob-run-secret-value")
	for _, id := range []uuid.UUID{ar1, ar2, br} {
		if !b.m.Covered(ctx, id) {
			t.Fatalf("B does not cover %s before the fence", id)
		}
	}

	fenced, err := a.m.FenceSubject(ctx, alice)
	if err != nil {
		t.Fatalf("FenceSubject: %v", err)
	}
	if len(fenced) != 2 {
		t.Fatalf("fenced %v, want alice's two runs", fenced)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_mask_values WHERE owner=$1`, alice).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("alice's value rows after the fence = %d (err %v), want 0", rows, err)
	}
	var fencedRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_mask_manifest WHERE owner=$1 AND fenced_at IS NOT NULL`, alice).Scan(&fencedRows); err != nil || fencedRows != 2 {
		t.Fatalf("fenced manifest rows = %d (err %v), want 2", fencedRows, err)
	}

	// Server B, which did not issue the fence, honours it at its next check and
	// drops what it held; A, which did, dropped its own at once.
	for _, r := range []replica{a, b} {
		if r.m.Covered(ctx, ar1) || r.m.Covered(ctx, ar2) {
			t.Error("a fenced run is still covered")
		}
		if masks(r, ar1, "alice-first-run-secret") {
			t.Error("a replica still holds a fenced run's plaintext")
		}
	}
	if err := a.m.Append(ctx, ar1, []byte("minted-after-the-fence")); err != maskmanifest.ErrFenced {
		t.Errorf("Append on a fenced manifest = %v, want ErrFenced", err)
	}
	if err := a.m.Complete(ctx, ar1); err != maskmanifest.ErrFenced {
		t.Errorf("Complete on a fenced manifest = %v, want ErrFenced", err)
	}
	if !b.m.Covered(ctx, br) || !masks(b, br, "bob-run-secret-value") {
		t.Error("another subject's run lost its coverage")
	}
	if again, err := a.m.FenceSubject(ctx, alice); err != nil || len(again) != 2 {
		t.Errorf("a second fence = %v, %v; want it idempotent over the same two runs", again, err)
	}
	if _, err := a.m.FenceSubject(ctx, ""); err != maskmanifest.ErrNoOwner {
		t.Errorf("FenceSubject of the operator namespace = %v, want ErrNoOwner", err)
	}
}

// Watch re-reads Postgres at most once per period and answers false from the
// beat after a fence.
func TestPG_MaskManifest_WatchSeesAFenceWithinOnePeriod(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	k := localKEK(t)
	const owner = "alice@example.com"
	run := manifestRun(t, pool, owner)
	a, b := newReplica(t, pool, k), newReplica(t, pool, k)
	dispatched(t, a, run, owner, "watched-run-secret")

	watch := b.m.Watch(run, 20*time.Millisecond)
	if !watch() {
		t.Fatal("a covered run is not watched as covered")
	}
	if _, err := a.m.FenceSubject(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for watch() {
		if time.Now().After(deadline) {
			t.Fatal("the watch never saw the fence issued by another replica")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
