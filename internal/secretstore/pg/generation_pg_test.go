// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

// The erase generation: a write that began before EraseOwner is refused or
// erased by it, in local and store mode, and a write that begins after still
// lands. Guarded by WARDYN_TEST_PG.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

func beginFor(t *testing.T, s secretstore.Store, owner string) context.Context {
	t.Helper()
	ctx, ok, err := secretstore.BeginWrite(t.Context(), owner, s.For(owner))
	if err != nil || !ok {
		t.Fatalf("BeginWrite(%q) = ok %v, %v", owner, ok, err)
	}
	return ctx
}

func listNames(t *testing.T, s secretstore.Store, owner string) []string {
	t.Helper()
	names, err := s.For(owner).List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func TestGeneration_WriteBegunBeforeEraseIsRefusedAndALaterOneLands(t *testing.T) {
	s, _, _ := newPGStore(t)
	owner := "gen-" + uuid.NewString()
	view := s.For(owner)
	began := beginFor(t, s, owner)

	if err := view.Put(began, "kept", []byte("before-erase-0000")); err != nil {
		t.Fatalf("fenced Put before any erase: %v", err)
	}
	rep, err := secretstore.EraseOwner(t.Context(), s, owner)
	if err != nil || !rep.Fenced || rep.Count != 1 {
		t.Fatalf("EraseOwner = %+v, %v; want fenced, 1 credential", rep, err)
	}
	if err := view.Put(began, "late", []byte("after-erase-00000")); !errors.Is(err, secretstore.ErrGenerationChanged) {
		t.Fatalf("Put that began before the erase = %v, want ErrGenerationChanged", err)
	}
	if got := listNames(t, s, owner); len(got) != 0 {
		t.Fatalf("namespace after the refused write = %v, want empty", got)
	}
	if err := view.Put(beginFor(t, s, owner), "reconnect", []byte("after-erase-00000")); err != nil {
		t.Fatalf("a legitimate reconnect after the erase: %v", err)
	}
	if err := view.Put(t.Context(), "unfenced", []byte("after-erase-00000")); err != nil {
		t.Fatalf("an unfenced Put keeps today's behaviour: %v", err)
	}
	if got := listNames(t, s, owner); !slices.Equal(got, []string{"reconnect", "unfenced"}) {
		t.Fatalf("namespace = %v", got)
	}
	// Another person's erase never fences this one.
	other := "gen-" + uuid.NewString()
	if _, err := secretstore.EraseOwner(t.Context(), s, other); err != nil {
		t.Fatal(err)
	}
	if err := view.Put(beginFor(t, s, owner), "again", []byte("after-erase-00000")); err != nil {
		t.Fatalf("write fenced to one owner refused after another's erase: %v", err)
	}
}

func TestGeneration_LegacyOwnerWithoutACounterRowKeepsWorking(t *testing.T) {
	s, pool, _ := newPGStore(t)
	owner := "legacy-" + uuid.NewString()
	mustPut(t, s, owner, "old", "legacy-value-0000")
	if n := count(t, pool, `SELECT count(*) FROM secret_owner_generations WHERE owned_by=$1`, owner); n != 0 {
		t.Fatalf("an unfenced write created %d counter rows", n)
	}
	gen, err := s.For(owner).(secretstore.Generational).Generation(t.Context())
	if err != nil || gen != 0 {
		t.Fatalf("Generation of a legacy owner = %d, %v; want 0", gen, err)
	}
	if err := s.For(owner).Put(beginFor(t, s, owner), "new", []byte("fenced-value-0000")); err != nil {
		t.Fatalf("fenced Put for a legacy owner: %v", err)
	}
	if got := listNames(t, s, owner); !slices.Equal(got, []string{"new", "old"}) {
		t.Fatalf("namespace = %v", got)
	}
}

func TestGeneration_OperatorNamespaceIsNeverFenced(t *testing.T) {
	s, _, _ := newPGStore(t)
	if _, err := s.For("").(secretstore.Generational).AdvanceGeneration(t.Context()); !errors.Is(err, secretstore.ErrOperatorNamespace) {
		t.Fatalf("AdvanceGeneration of the operator namespace = %v, want ErrOperatorNamespace", err)
	}
	name := "gen-operator-" + uuid.NewString()
	ctx := secretstore.WithIfGeneration(t.Context(), "", 7)
	if err := s.Put(ctx, name, []byte("operator-value-00")); err != nil {
		t.Fatalf("operator Put under a generation: %v", err)
	}
	_ = s.Delete(t.Context(), name)
}

func TestGeneration_CounterHoldsNoCredential(t *testing.T) {
	s, pool, _ := newPGStore(t)
	rows, err := pool.Query(t.Context(), `SELECT column_name FROM information_schema.columns
		WHERE table_name='secret_owner_generations' ORDER BY column_name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	if want := []string{"generation", "owned_by", "updated_at"}; !slices.Equal(cols, want) {
		t.Fatalf("counter columns = %v, want exactly %v", cols, want)
	}
	_ = s
}

// A fenced write that is also revision-guarded checks both.
func TestGeneration_ComposesWithTheRevisionGuard(t *testing.T) {
	s, _, _ := newPGStore(t)
	owner := "gen-rev-" + uuid.NewString()
	view := s.For(owner)
	ctx := beginFor(t, s, owner)
	if err := view.Put(secretstore.WithIfRevision(ctx, ""), "n", []byte("first-value-0000")); err != nil {
		t.Fatal(err)
	}
	if err := view.Put(secretstore.WithIfRevision(ctx, ""), "n", []byte("clobber-value-00")); !errors.Is(err, secretstore.ErrRevisionChanged) {
		t.Fatalf("revision still guards under a generation: %v", err)
	}
	rev, _ := view.(secretstore.Revisioned).Revision(ctx, "n")
	if _, err := secretstore.EraseOwner(t.Context(), s, owner); err != nil {
		t.Fatal(err)
	}
	if err := view.Put(secretstore.WithIfRevision(ctx, rev), "n", []byte("stale-value-0000")); !errors.Is(err, secretstore.ErrGenerationChanged) && !errors.Is(err, secretstore.ErrRevisionChanged) {
		t.Fatalf("stale write after the erase = %v", err)
	}
	if got := listNames(t, s, owner); len(got) != 0 {
		t.Fatalf("namespace = %v, want empty", got)
	}
}

func TestGeneration_StoreModeIsFencedBeforeTheExternalWrite(t *testing.T) {
	base := rekeyDatabase(t)
	ext := newMemExt("vaultkv")
	s := mixedStore(t, base, mustIdentity(t), false, ext, true)
	owner := "gen-ext-" + uuid.NewString()
	began := beginFor(t, s, owner)
	if _, err := secretstore.EraseOwner(t.Context(), s, owner); err != nil {
		t.Fatal(err)
	}
	if err := s.For(owner).Put(began, "n", []byte("external-value-00")); !errors.Is(err, secretstore.ErrGenerationChanged) {
		t.Fatalf("store-mode Put that began before the erase = %v, want ErrGenerationChanged", err)
	}
	ext.mu.Lock()
	left := len(ext.vals)
	ext.mu.Unlock()
	if left != 0 {
		t.Fatalf("a refused write reached the external store: %d values", left)
	}
	if err := s.For(owner).Put(beginFor(t, s, owner), "n", []byte("external-value-00")); err != nil {
		t.Fatalf("reconnect after the erase: %v", err)
	}
}

// TestGeneration_EraseRacingWritersNeverResurrects is the #1511 race: every
// writer began before the erase and keeps writing through it. Whatever the
// interleaving, the erase answers success and the namespace stays empty.
func TestGeneration_EraseRacingWritersNeverResurrects(t *testing.T) {
	s, _, _ := newPGStore(t)
	const writers, puts = 6, 8
	owner := "gen-race-" + uuid.NewString()
	began := make([]context.Context, writers)
	for i := range began {
		began[i] = beginFor(t, s, owner)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var landedAfterRefusal sync.Map
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			refused := false
			for j := range puts {
				err := s.For(owner).Put(began[i], fmt.Sprintf("w%d-%d", i, j), []byte("racing-value-0000"))
				switch {
				case errors.Is(err, secretstore.ErrGenerationChanged):
					refused = true
				case err != nil:
					t.Errorf("writer %d: %v", i, err)
				case refused:
					landedAfterRefusal.Store(i, j)
				}
			}
		}()
	}
	close(start)
	rep, err := secretstore.EraseOwner(t.Context(), s, owner)
	wg.Wait()
	if err != nil {
		t.Fatalf("EraseOwner = %+v, %v; a writer that began earlier must never make it fail", rep, err)
	}
	landedAfterRefusal.Range(func(i, j any) bool {
		t.Errorf("writer %v landed put %v after the erase refused it", i, j)
		return true
	})
	if got := listNames(t, s, owner); len(got) != 0 {
		t.Fatalf("erased namespace holds %v after its writers finished", got)
	}
}

// A write past its check holds the counter row shared until it commits, so the
// erase's advance waits for it and EraseOwner's list then sees the row. Without
// that lock the write could commit after the list and survive the erase.
func TestGeneration_AdvanceWaitsForAWritePastItsCheck(t *testing.T) {
	s, _, _ := newPGStore(t)
	owner := "gen-wait-" + uuid.NewString()
	view := s.For(owner).(*Store)
	// The counter row exists already: the lock under test is the share lock on
	// it, not the insert that creates it.
	gen, err := view.AdvanceGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := beginReadCommitted(t.Context(), holdPool(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	if err := view.checkGeneration(t.Context(), tx, gen); err != nil {
		t.Fatal(err)
	}
	advanced := make(chan error, 1)
	go func() { _, err := view.AdvanceGeneration(t.Context()); advanced <- err }()
	select {
	case err := <-advanced:
		t.Fatalf("AdvanceGeneration returned (%v) while a write past its check was uncommitted", err)
	case <-time.After(300 * time.Millisecond):
	}
	r, err := view.sealRow(t.Context(), owner, "n", []byte("waiting-value-000"))
	if err != nil {
		t.Fatal(err)
	}
	if err := view.putRow(t.Context(), tx, "n", r); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-advanced; err != nil {
		t.Fatal(err)
	}
	if got := listNames(t, s, owner); !slices.Equal(got, []string{"n"}) {
		t.Fatalf("the row committed before the advance returned should be listed, got %v", got)
	}
}
