// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

// A provider purge that is writing an owner's connection history and an erase
// of that owner: the history written by the purge does not outlive the erase.
// Guarded by WARDYN_TEST_PG.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

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
	s, pool, _ := newPGStore(t)
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
	tx, err := beginReadCommitted(t.Context(), pool)
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
	s, pool, _ := newPGStore(t)
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
	tx, err := beginReadCommitted(t.Context(), pool)
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
