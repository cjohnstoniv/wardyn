// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package identitytest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
)

// RunBindingConformance holds an identity.TokenBindingStore to its contract, so the in-memory and
// Postgres stores cannot drift apart. newStore may return a store over shared state: every case works on
// fresh random jtis and runs. newBroken returns a store whose backend fails every call.
func RunBindingConformance(t *testing.T, newStore, newBroken func(t *testing.T) identity.TokenBindingStore) {
	ctx := context.Background()
	binding := func(exp time.Time) identity.TokenBinding {
		run := uuid.New()
		return identity.TokenBinding{JTI: uuid.NewString(), RunID: run, SPIFFEID: "spiffe://wardyn.local/agent-run/" + run.String(),
			Sub: "Alice@Example.com", Sponsor: "sponsor@example.com", OperatorOwned: true, Expiry: exp.UTC().Truncate(time.Second)}
	}

	t.Run("put_then_get_round_trips", func(t *testing.T) {
		s := newStore(t)
		b := binding(time.Now().Add(time.Hour))
		if err := s.PutBinding(ctx, b); err != nil {
			t.Fatalf("PutBinding: %v", err)
		}
		got, err := s.GetBinding(ctx, b.JTI)
		if err != nil || !got.Expiry.Equal(b.Expiry) {
			t.Fatalf("GetBinding = (%+v, %v), want %+v", got, err, b)
		}
		got.Expiry = b.Expiry
		if got != b {
			t.Fatalf("GetBinding = %+v, want %+v", got, b)
		}
	})

	t.Run("missing_jti_is_typed_not_found", func(t *testing.T) {
		if _, err := newStore(t).GetBinding(ctx, uuid.NewString()); !errors.Is(err, identity.ErrBindingNotFound) {
			t.Fatalf("GetBinding of an unknown jti = %v, want ErrBindingNotFound", err)
		}
	})

	t.Run("second_put_of_a_jti_is_an_error", func(t *testing.T) {
		s := newStore(t)
		b := binding(time.Now().Add(time.Hour))
		if err := s.PutBinding(ctx, b); err != nil {
			t.Fatalf("PutBinding: %v", err)
		}
		c := b
		c.Sponsor = "someone-else@example.com"
		if err := s.PutBinding(ctx, c); err == nil {
			t.Fatal("a second put of one jti must be refused, never an overwrite")
		}
		if got, err := s.GetBinding(ctx, b.JTI); err != nil || got.Sponsor != b.Sponsor {
			t.Fatalf("after the refused put GetBinding = (%+v, %v), want the first row", got, err)
		}
	})

	t.Run("broken_store_errors", func(t *testing.T) {
		_, err := newBroken(t).GetBinding(ctx, uuid.NewString())
		if err == nil || errors.Is(err, identity.ErrBindingNotFound) {
			t.Fatalf("GetBinding on a failing store = %v, want an ordinary error", err)
		}
	})

	t.Run("prune_deletes_rows_past_the_cutoff_only", func(t *testing.T) {
		s := newStore(t)
		now := time.Now()
		cutoff := now.Add(-identity.RunTokenLapseAfter)
		old, live := binding(cutoff.Add(-time.Minute)), binding(cutoff.Add(time.Minute))
		for _, b := range []identity.TokenBinding{old, live} {
			if err := s.PutBinding(ctx, b); err != nil {
				t.Fatalf("PutBinding: %v", err)
			}
		}
		if _, err := s.PruneBindings(ctx, cutoff); err != nil {
			t.Fatalf("PruneBindings: %v", err)
		}
		if _, err := s.GetBinding(ctx, old.JTI); !errors.Is(err, identity.ErrBindingNotFound) {
			t.Fatalf("a row past its lapse survived the prune: %v", err)
		}
		if _, err := s.GetBinding(ctx, live.JTI); err != nil {
			t.Fatalf("the prune deleted a row inside its lapse: %v", err)
		}
	})
}
