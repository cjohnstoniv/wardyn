// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_APITokens_Expiry pins the auth-time lookup against the database's own
// clock: a NULL expires_at never expires, a token still before its expiry
// authenticates, one past it is ErrNotFound exactly as a revoked or unknown one
// is, and the mint stamps expires_at = created_at + the requested lifetime.
func TestPG_APITokens_Expiry(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	person := "expiry-" + uuid.NewString()
	mint := func(lifetime *time.Duration) (types.APIToken, string) {
		t.Helper()
		raw := "wdn_" + uuid.NewString()
		tok := types.APIToken{
			ID: uuid.New(), Principal: person, Role: "user", UserType: "standard",
			GroupsTruncated: new(bool), Name: "ci", CreatedAt: time.Now().UTC(),
		}
		if lifetime != nil {
			exp := tok.CreatedAt.Add(*lifetime)
			tok.ExpiresAt = &exp
		}
		row, err := st.CreateAPIToken(ctx, tok, raw)
		if err != nil {
			t.Fatal(err)
		}
		return row, raw
	}
	setExpiry := func(id uuid.UUID, sqlExpr string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE api_tokens SET expires_at = `+sqlExpr+` WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	live := func(raw string) bool {
		t.Helper()
		_, err := st.GetAPITokenByRaw(ctx, raw)
		if errors.Is(err, store.ErrNotFound) {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		return true
	}

	hour := time.Hour
	forever, foreverRaw := mint(nil)
	if forever.ExpiresAt != nil || !live(foreverRaw) {
		t.Fatalf("a token minted with no expiry = %+v (live %v), want expires_at NULL and live", forever, live(foreverRaw))
	}
	timed, timedRaw := mint(&hour)
	if timed.ExpiresAt == nil || timed.ExpiresAt.Sub(timed.CreatedAt) != hour {
		t.Fatalf("minted with a 1h lifetime: expires_at %v created_at %v, want exactly 1h apart", timed.ExpiresAt, timed.CreatedAt)
	}
	if !live(timedRaw) {
		t.Fatal("a token an hour from expiry does not authenticate")
	}

	setExpiry(timed.ID, `now() + interval '1 second'`)
	if !live(timedRaw) {
		t.Error("a token one second before expires_at does not authenticate")
	}
	setExpiry(timed.ID, `now() - interval '1 second'`)
	if live(timedRaw) {
		t.Error("a token one second after expires_at still authenticates")
	}
	if !live(foreverRaw) {
		t.Error("a NULL expires_at stopped authenticating")
	}

	// Every reader: the lists still show an expired token (so its holder can see
	// why it stopped), with the expiry on it.
	listed, err := st.ListAPITokensByPrincipal(ctx, person)
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, l := range listed {
		if l.ID == timed.ID {
			seen = l.ExpiresAt != nil && l.ExpiresAt.Before(time.Now())
		}
	}
	if !seen {
		t.Errorf("the principal's list does not show the expired token with its expires_at: %+v", listed)
	}
}
