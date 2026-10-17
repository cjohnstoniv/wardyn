// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

// The order inside EraseOwner: a write that lands after the generation advance
// and before the list. Guarded by WARDYN_TEST_PG.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// advanceHookStore runs onAdvance once, right after the first generation
// advance of the owner's view has committed and before EraseOwner lists.
type advanceHookStore struct {
	*Store
	onAdvance func()
}

func (h advanceHookStore) For(owner string) secretstore.Store {
	return &advanceHookView{Store: h.Store.For(owner).(*Store), onAdvance: h.onAdvance}
}

type advanceHookView struct {
	*Store
	onAdvance func()
}

func (v *advanceHookView) AdvanceGeneration(ctx context.Context) (int64, error) {
	gen, err := v.Store.AdvanceGeneration(ctx)
	if f := v.onAdvance; f != nil {
		v.onAdvance = nil
		f()
	}
	return gen, err
}

// A write that began before the advance is refused as stale; one that begins
// after it lands and is erased by the list that follows. Neither is left
// behind, and the erase does not have to be repeated.
func TestEraseOwner_WriteBetweenAdvanceAndListIsRefusedOrErased(t *testing.T) {
	s, _, _ := newPGStore(t)
	owner := "erase-order-" + uuid.NewString()
	if err := s.For(owner).Put(t.Context(), "old", []byte("existing-value-00")); err != nil {
		t.Fatal(err)
	}
	early := beginFor(t, s, owner)
	var earlyErr, lateErr error
	hooked := advanceHookStore{Store: s, onAdvance: func() {
		earlyErr = s.For(owner).Put(early, "early", []byte("early-value-0000"))
		late, ok, err := secretstore.BeginWrite(t.Context(), owner, s.For(owner))
		if err != nil || !ok {
			t.Errorf("BeginWrite after the advance = ok %v, %v", ok, err)
			return
		}
		lateErr = s.For(owner).Put(late, "late", []byte("late-value-00000"))
	}}
	rep, err := secretstore.EraseOwner(t.Context(), hooked, owner)
	if err != nil || !rep.Fenced {
		t.Fatalf("EraseOwner = %+v, %v; want a fenced success with nothing left to erase again", rep, err)
	}
	if !errors.Is(earlyErr, secretstore.ErrGenerationChanged) {
		t.Fatalf("Put begun before the advance = %v, want ErrGenerationChanged", earlyErr)
	}
	if lateErr != nil {
		t.Fatalf("Put begun after the advance = %v, want it to land and be erased", lateErr)
	}
	if rep.Count != 2 {
		t.Fatalf("EraseOwner deleted %d credentials, want 2 (the old one and the late write)", rep.Count)
	}
	if got := listNames(t, s, owner); len(got) != 0 {
		t.Fatalf("namespace after the erase = %v, want empty", got)
	}
}

// A write that lands after the list and before the second advance, from a writer that began after the
// first advance, is not in the list the deletes use: the re-list finds it, and the erase says to run it again.
func TestEraseOwner_WriteBetweenListAndSecondAdvanceIsReported(t *testing.T) {
	s, _, _ := newPGStore(t)
	owner := "erase-order-" + uuid.NewString()
	if err := s.For(owner).Put(t.Context(), "old", []byte("existing-value-00")); err != nil {
		t.Fatal(err)
	}
	var lateErr error
	hooked := listHookStore{Store: s, onList: func() {
		late, ok, err := secretstore.BeginWrite(t.Context(), owner, s.For(owner))
		if err != nil || !ok {
			t.Errorf("BeginWrite after the list = ok %v, %v", ok, err)
			return
		}
		lateErr = s.For(owner).Put(late, "late", []byte("late-value-00000"))
	}}
	_, err := secretstore.EraseOwner(t.Context(), hooked, owner)
	if err == nil || !strings.Contains(err.Error(), "erase again") {
		t.Fatalf("EraseOwner = %v; want it to ask for the erase to run again", err)
	}
	if lateErr != nil {
		t.Fatalf("Put begun after the first advance = %v, want it to land", lateErr)
	}
	if got := listNames(t, s, owner); len(got) != 1 || got[0] != "late" {
		t.Fatalf("namespace after the first erase = %v, want only the late write", got)
	}
	if rep, err := secretstore.EraseOwner(t.Context(), s, owner); err != nil || rep.Count != 1 {
		t.Fatalf("second EraseOwner = %+v, %v; want the late write erased", rep, err)
	}
}
