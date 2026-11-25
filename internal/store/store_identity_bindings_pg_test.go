// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/identity/identitytest"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// pgBindings is the store's identity_token_bindings as an identity.TokenBindingStore, as wardynd adapts it.
type pgBindings struct{ pg store.PG }

func (b pgBindings) PutBinding(ctx context.Context, x identity.TokenBinding) error {
	return b.pg.PutTokenBinding(ctx, x)
}
func (b pgBindings) GetBinding(ctx context.Context, jti string) (identity.TokenBinding, error) {
	return b.pg.GetTokenBinding(ctx, jti)
}
func (b pgBindings) PruneBindings(ctx context.Context, cutoff time.Time) (int64, error) {
	return b.pg.PruneTokenBindings(ctx, cutoff)
}

// The Postgres binding store is held to the same contract as the in-memory one.
func TestPG_TokenBindingConformance(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	identitytest.RunBindingConformance(t,
		func(*testing.T) identity.TokenBindingStore { return pgBindings{store.NewPG(pool)} },
		func(t *testing.T) identity.TokenBindingStore {
			broken := runsPGPoolIsolated(t)
			broken.Close()
			return pgBindings{store.NewPG(broken)}
		})
}

// DeleteTokenBindingsOf deletes the rows whose sub or sponsor is the person, compared lower-cased, and no
// other.
func TestPG_DeleteTokenBindingsOf(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	row := func(sub, sponsor string) identity.TokenBinding {
		run := uuid.New()
		b := identity.TokenBinding{JTI: uuid.NewString(), RunID: run, SPIFFEID: "spiffe://wardyn.local/agent-run/" + run.String(),
			Sub: sub, Sponsor: sponsor, Expiry: time.Now().Add(time.Hour)}
		if err := pg.PutTokenBinding(ctx, b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	asSub, asSponsor, other := row("Alice@Example.com", "boss@example.com"), row("bob@example.com", "alice@example.com"), row("bob@example.com", "carol@example.com")
	n, err := pg.DeleteTokenBindingsOf(ctx, "alice@example.com")
	if err != nil || n != 2 {
		t.Fatalf("DeleteTokenBindingsOf = (%d, %v), want 2 rows", n, err)
	}
	for _, gone := range []identity.TokenBinding{asSub, asSponsor} {
		if _, err := pg.GetTokenBinding(ctx, gone.JTI); !errors.Is(err, identity.ErrBindingNotFound) {
			t.Fatalf("row %+v survived its person's erasure: %v", gone, err)
		}
	}
	if _, err := pg.GetTokenBinding(ctx, other.JTI); err != nil {
		t.Fatalf("an unrelated row was deleted: %v", err)
	}
}
