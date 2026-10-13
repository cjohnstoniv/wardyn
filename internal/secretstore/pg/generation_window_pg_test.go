// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

// The window inside EraseOwner: after its first advance and before its
// deletes. Guarded by WARDYN_TEST_PG.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// listHookStore runs onList once, inside the first List of the owner's view:
// the point in EraseOwner after it advanced the generation and before it
// deletes anything.
type listHookStore struct {
	*Store
	onList func()
}

func (h listHookStore) For(owner string) secretstore.Store {
	return &listHookView{Store: h.Store.For(owner).(*Store), onList: h.onList}
}

type listHookView struct {
	*Store
	onList func()
}

func (v *listHookView) List(ctx context.Context) ([]string, error) {
	names, err := v.Store.List(ctx)
	if f := v.onList; f != nil {
		v.onList = nil
		f()
	}
	return names, err
}

// A read-then-write writer that begins inside the erase window reads the
// advanced generation and a credential the erase then deletes. Its Put, made
// after EraseOwner returned, must not put that credential back: the erase
// advances the generation again once its deletes are done.
func TestGeneration_WriteBegunInsideTheEraseWindowCannotResurrect(t *testing.T) {
	s, _, _ := newPGStore(t)
	owner := "gen-window-" + uuid.NewString()
	if err := s.For(owner).Put(t.Context(), "tok", []byte("secret-to-erase-0")); err != nil {
		t.Fatal(err)
	}
	var began context.Context
	var read []byte
	hooked := listHookStore{Store: s, onList: func() {
		ctx, ok, err := secretstore.BeginWrite(t.Context(), owner, s.For(owner))
		if err != nil || !ok {
			t.Errorf("BeginWrite inside the window = ok %v, %v", ok, err)
			return
		}
		began = ctx
		read, err = s.For(owner).Get(ctx, "tok")
		if err != nil {
			t.Errorf("read inside the window: %v", err)
		}
	}}
	rep, err := secretstore.EraseOwner(t.Context(), hooked, owner)
	if err != nil || !rep.Fenced {
		t.Fatalf("EraseOwner = %+v, %v; want a fenced success", rep, err)
	}
	if began == nil || len(read) == 0 {
		t.Fatal("the hook never read the credential")
	}
	if err := s.For(owner).Put(began, "tok", read); !errors.Is(err, secretstore.ErrGenerationChanged) {
		t.Fatalf("write-back from a write begun inside the erase window = %v, want ErrGenerationChanged", err)
	}
	if got := listNames(t, s, owner); len(got) != 0 {
		t.Fatalf("namespace after a fenced erase and a refused write-back = %v, want empty", got)
	}
}
