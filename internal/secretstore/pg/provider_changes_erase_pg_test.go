// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

// A provider purge that is writing an owner's connection history and an erase
// of that owner: the history written by the purge does not outlive the erase.
// Guarded by WARDYN_TEST_PG.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func historyRows(t *testing.T, s *Store, owner string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM provider_connection_changes WHERE owner=$1`, owner).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func purgeOf(oldUID, newUID string) context.Context {
	return secretstore.WithProviderInvalidations(context.Background(), []secretstore.ProviderInvalidation{{
		OldUID: oldUID,
		Change: secretstore.ProviderChange{ProviderID: "prov-" + oldUID, ProviderUID: newUID, Reason: "destination_changed", ChangedAt: time.Now(), NewDestination: "https://new.example.test"},
	}})
}

// The purge has recorded the owner's history and not yet committed when the
// erase starts. The erase's advance waits for the purge, so its later history
// delete removes the row the purge wrote.
func TestPG_EraseWaitsForAProviderPurgeAndRemovesTheHistoryItWrote(t *testing.T) {
	s, _, _ := newPGStore(t)
	owner := "gen-prov-" + uuid.NewString()
	oldUID := uuid.NewString()
	if err := s.For(owner).Put(t.Context(), types.ModelProviderSecretPrefix+oldUID+"-key", []byte("provider-key-0000")); err != nil {
		t.Fatal(err)
	}
	// The counter row exists already: the lock under test is the one on it,
	// not the insert that creates it.
	if _, err := s.For(owner).(*Store).AdvanceGeneration(t.Context()); err != nil {
		t.Fatal(err)
	}
	tx, err := beginReadCommitted(t.Context(), holdPool(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	if err := recordProviderChanges(purgeOf(oldUID, "uid-2"), tx); err != nil {
		t.Fatal(err)
	}
	erased := make(chan error, 1)
	go func() { _, err := secretstore.EraseOwner(t.Context(), s, owner); erased <- err }()
	select {
	case err := <-erased:
		t.Fatalf("EraseOwner returned (%v) while a provider purge held the owner's history uncommitted", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-erased; err != nil {
		t.Fatalf("EraseOwner: %v", err)
	}
	if n := historyRows(t, s, owner); n != 0 {
		t.Fatalf("%d provider history rows of an erased owner survived the purge that raced the erase", n)
	}
}

// The history delete takes the owner's counter row exclusively, so it waits for
// a purge that is still writing and removes what the purge wrote.
func TestPG_DeleteProviderChangesWaitsForAPurgeHoldingTheOwner(t *testing.T) {
	s, _, _ := newPGStore(t)
	owner := "gen-prov-del-" + uuid.NewString()
	oldUID := uuid.NewString()
	if err := s.For(owner).Put(t.Context(), types.ModelProviderSecretPrefix+oldUID+"-key", []byte("provider-key-0000")); err != nil {
		t.Fatal(err)
	}
	// The counter row exists already: the lock under test is the one on it,
	// not the insert that creates it.
	if _, err := s.For(owner).(*Store).AdvanceGeneration(t.Context()); err != nil {
		t.Fatal(err)
	}
	tx, err := beginReadCommitted(t.Context(), holdPool(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	if err := recordProviderChanges(purgeOf(oldUID, "uid-2"), tx); err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() { deleted <- s.For(owner).(*Store).DeleteProviderChanges(t.Context()) }()
	select {
	case err := <-deleted:
		t.Fatalf("DeleteProviderChanges returned (%v) while a purge held the owner", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
	if n := historyRows(t, s, owner); n != 0 {
		t.Fatalf("%d history rows left after the delete that followed the purge", n)
	}
}

func removalOf(oldUID string) secretstore.ProviderInvalidation {
	return secretstore.ProviderInvalidation{OldUID: oldUID, Change: secretstore.ProviderChange{ProviderID: "prov-" + oldUID}}
}

func editOf(oldUID, newUID string) secretstore.ProviderInvalidation {
	return secretstore.ProviderInvalidation{OldUID: oldUID, Change: secretstore.ProviderChange{
		ProviderID: "prov-" + oldUID, ProviderUID: newUID, Reason: "destination_changed", ChangedAt: time.Now(), NewDestination: "https://new.example.test"}}
}

func purgeCtx(inv ...secretstore.ProviderInvalidation) context.Context {
	return secretstore.WithProviderInvalidations(context.Background(), inv)
}

func putProviderKey(t *testing.T, s *Store, owner, uid string) {
	t.Helper()
	if err := s.For(owner).Put(t.Context(), types.ModelProviderSecretPrefix+uid+"-key", []byte("provider-key-0000")); err != nil {
		t.Fatal(err)
	}
}

// holdPool is a pool of its own for a test that keeps a transaction open while the store, on its pool,
// works on the same goroutine: the nested acquire guard refuses that on one pool (WARDYN_TEST_POOL_GUARD).
func holdPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	holder, err := pgxpool.New(t.Context(), os.Getenv("WARDYN_TEST_PG"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(holder.Close)
	return holder
}

// holdCounter takes owner's counter row exclusively, as an erase does, until the returned release runs.
// The hold lives on its own pool.
func holdCounter(t *testing.T, s *Store, owner string) (tx pgx.Tx, release func()) {
	t.Helper()
	if _, err := s.For(owner).(*Store).AdvanceGeneration(t.Context()); err != nil {
		t.Fatal(err)
	}
	tx, err := holdPool(t).Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `SELECT 1 FROM secret_owner_generations WHERE owned_by=$1 FOR UPDATE`, owner); err != nil {
		t.Fatal(err)
	}
	return tx, func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }
}

// A credential that commits after the purge chose its owners and before it wrote
// their history belongs to an owner nothing has locked. The purge writes no history
// for that owner, so an erase of them that finishes before the purge commits cannot
// miss a row.
func TestPG_OwnerAddedBetweenLockAndInsertSurvivesNoErase(t *testing.T) {
	s, _, _ := newPGStore(t)
	a, b := "gen-gap-a-"+uuid.NewString(), "gen-gap-b-"+uuid.NewString()
	oldUID := uuid.NewString()
	putProviderKey(t, s, a, oldUID)
	third, release := holdCounter(t, s, a)
	defer release()
	tx, err := beginReadCommitted(t.Context(), holdPool(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	recorded := make(chan error, 1)
	go func() { recorded <- recordProviderChanges(purgeCtx(editOf(oldUID, "uid-2")), tx) }()
	select {
	case err := <-recorded:
		t.Fatalf("the purge did not wait for the owner's counter row: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	putProviderKey(t, s, b, oldUID) // commits while the purge waits
	if err := third.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-recorded; err != nil {
		t.Fatal(err)
	}
	if _, err := secretstore.EraseOwner(t.Context(), s, b); err != nil {
		t.Fatalf("erase of the owner added in the gap: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := historyRows(t, s, b); n != 0 {
		t.Fatalf("%d history rows of an owner erased while the purge was open survived it", n)
	}
	if n := historyRows(t, s, a); n != 1 {
		t.Fatalf("the locked owner has %d history rows, want 1", n)
	}
}

// A purge that removes one provider and edits another locks every owner it will touch before it
// writes or deletes any history. The erase takes an owner's counter row and then their history, so
// meeting the purge's locks in that order is what keeps the two from deadlocking (40P01).
func TestPG_RemovalThenEditPurgeAndHistoryEraseDoNotDeadlock(t *testing.T) {
	s, pool, _ := newPGStore(t)
	x := "gen-dead-" + uuid.NewString()
	removedUID, editedUID := uuid.NewString(), uuid.NewString()
	putProviderKey(t, s, x, editedUID)
	if _, err := pool.Exec(t.Context(), `INSERT INTO provider_connection_changes (owner, provider_id, provider_uid, reason, changed_at, new_destination)
		VALUES ($1, $2, 'older', 'destination_changed', now(), 'https://old.example.test')`, x, "prov-"+removedUID); err != nil {
		t.Fatal(err)
	}
	erase, release := holdCounter(t, s, x)
	defer release()
	tx, err := beginReadCommitted(t.Context(), holdPool(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	recorded := make(chan error, 1)
	go func() {
		recorded <- recordProviderChanges(purgeCtx(removalOf(removedUID), editOf(editedUID, "uid-2")), tx)
	}()
	select {
	case err := <-recorded:
		t.Fatalf("the purge did not wait for the owner's counter row: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	// The erase now deletes the owner's history. A purge that had already row-locked it would
	// make this wait for a purge that waits for the erase.
	if _, err := erase.Exec(t.Context(), `DELETE FROM provider_connection_changes WHERE owner=$1`, x); err != nil {
		t.Fatalf("the history delete under the counter lock: %v", err)
	}
	if err := erase.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-recorded; err != nil {
		t.Fatalf("the purge: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// Several purges of two invalidations each and erases of the same owners, at once: none fails, and
// none ends in a deadlock.
func TestPG_ProviderPurgesAndErasesNeverDeadlock(t *testing.T) {
	s, pool, _ := newPGStore(t)
	run := uuid.NewString()
	owners := []string{"gen-stress-a-" + run, "gen-stress-b-" + run, "gen-stress-c-" + run, "gen-stress-d-" + run}
	uids := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	seed := func() {
		for _, o := range owners {
			for _, u := range uids {
				putProviderKey(t, s, o, u)
			}
		}
	}
	seed()
	const rounds = 12
	var wg sync.WaitGroup
	for p := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range rounds {
				tx, err := beginReadCommitted(context.Background(), pool)
				if err != nil {
					t.Errorf("purger %d: %v", p, err)
					return
				}
				ctx := purgeCtx(editOf(uids[(p+i)%3], fmt.Sprintf("new-%d-%d", p, i)), removalOf(uids[(p+i+1)%3]))
				err = recordProviderChanges(ctx, tx)
				if err == nil {
					err = tx.Commit(context.Background())
				} else {
					_ = tx.Rollback(context.Background())
				}
				if err != nil {
					t.Errorf("purger %d round %d: %v", p, i, err)
					return
				}
			}
		}()
	}
	for e := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				for _, o := range owners[e*2 : e*2+2] {
					if _, err := secretstore.EraseOwner(context.Background(), s, o); err != nil {
						t.Errorf("eraser %d of %s: %v", e, o, err)
						return
					}
					for _, u := range uids {
						_ = s.For(o).Put(context.Background(), types.ModelProviderSecretPrefix+u+"-key", []byte("provider-key-0000"))
					}
				}
			}
		}()
	}
	wg.Wait()
}

// waitLockWaiters returns once n sessions of the test database are waiting on a lock.
func waitLockWaiters(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fewer than %d sessions waited on a lock", n)
}

// A store-mode fenced Put, an erase and a provider delete take the counter row and the row's advisory
// lock in one order. The delete holds the counter row shared; the erase queues for it exclusively;
// a Put that took the advisory lock before the counter row would sit between them and close the
// cycle (40P01), so it must wait for the counter row before taking the advisory lock.
func TestPG_StoreModeFencedPutEraseAndProviderDeleteDoNotDeadlock(t *testing.T) {
	_, base, _ := newPGStore(t)
	s := mixedStore(t, base, mustIdentity(t), false, newMemExt("vaultkv"), true)
	owner := "gen-ext-dead-" + uuid.NewString()
	uid := uuid.NewString()
	name := types.ModelProviderSecretPrefix + uid + "-key"
	began := beginFor(t, s, owner)

	del, err := holdPool(t).Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = del.Rollback(context.WithoutCancel(t.Context())) }()
	if _, err := del.Exec(t.Context(), `INSERT INTO secret_owner_generations (owned_by) VALUES ($1) ON CONFLICT (owned_by) DO NOTHING`, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := del.Exec(t.Context(), `SELECT 1 FROM secret_owner_generations WHERE owned_by=$1 FOR SHARE`, owner); err != nil {
		t.Fatal(err)
	}
	erased := make(chan error, 1)
	go func() { _, err := secretstore.EraseOwner(context.Background(), s, owner); erased <- err }()
	waitLockWaiters(t, base, 1) // the erase queues for the counter row behind the delete
	put := make(chan error, 1)
	go func() { put <- s.For(owner).Put(began, name, []byte("external-value-00")) }()
	waitLockWaiters(t, base, 2) // the Put reaches whichever lock it takes first and waits there
	deleted := make(chan error, 1)
	go func() { _, err := s.deleteLocked(t.Context(), del, owner, name); deleted <- err }()

	select {
	case err := <-deleted:
		if err != nil {
			t.Fatalf("the provider delete: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the provider delete never got the row's lock")
	}
	if err := del.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-erased; err != nil {
		t.Fatalf("the erase: %v", err)
	}
	if err := <-put; !errors.Is(err, secretstore.ErrGenerationChanged) {
		t.Fatalf("the Put that began before the erase = %v, want ErrGenerationChanged", err)
	}
}

// A provider removal writes no history, but it deletes an owner's old history for the provider, so it
// locks that owner's counter row like an edit: an owner known only by their history is waited for.
func TestPG_RemovalOnlyPurgeLocksAnOwnerKnownByTheirHistory(t *testing.T) {
	s, pool, _ := newPGStore(t)
	x := "gen-removal-" + uuid.NewString()
	removedUID := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO provider_connection_changes (owner, provider_id, provider_uid, reason, changed_at, new_destination)
		VALUES ($1, $2, 'older', 'destination_changed', now(), 'https://old.example.test')`, x, "prov-"+removedUID); err != nil {
		t.Fatal(err)
	}
	erase, release := holdCounter(t, s, x)
	defer release()
	tx, err := beginReadCommitted(t.Context(), holdPool(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	recorded := make(chan error, 1)
	go func() { recorded <- recordProviderChanges(purgeCtx(removalOf(removedUID)), tx) }()
	select {
	case err := <-recorded:
		t.Fatalf("a removal-only purge did not wait for the owner's counter row: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := erase.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-recorded; err != nil {
		t.Fatal(err)
	}
}

// The purge deletes a provider's old history only for the owners it locked. History that lands for an
// owner the purge did not lock, between its choosing owners and its delete, is left to that owner's own
// erase: deleting it unlocked is what the erase's ordering cannot see.
func TestPG_ProviderPurgeLeavesTheHistoryOfAnOwnerItDidNotLock(t *testing.T) {
	s, pool, _ := newPGStore(t)
	a, b := "gen-keep-a-"+uuid.NewString(), "gen-keep-b-"+uuid.NewString()
	oldUID := uuid.NewString()
	putProviderKey(t, s, a, oldUID)
	third, release := holdCounter(t, s, a)
	defer release()
	tx, err := beginReadCommitted(t.Context(), holdPool(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	recorded := make(chan error, 1)
	go func() { recorded <- recordProviderChanges(purgeCtx(editOf(oldUID, "uid-2")), tx) }()
	select {
	case err := <-recorded:
		t.Fatalf("the purge did not wait for the owner's counter row: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	// History for the same provider lands for an owner nothing has locked, while the purge waits.
	if _, err := pool.Exec(t.Context(), `INSERT INTO provider_connection_changes (owner, provider_id, provider_uid, reason, changed_at, new_destination)
		VALUES ($1, $2, 'older', 'destination_changed', now(), 'https://old.example.test')`, b, "prov-"+oldUID); err != nil {
		t.Fatal(err)
	}
	if err := third.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-recorded; err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := historyRows(t, s, b); n != 1 {
		t.Fatalf("the unlocked owner has %d history rows after the purge, want the 1 that landed", n)
	}
}
