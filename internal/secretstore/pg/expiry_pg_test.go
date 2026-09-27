// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// A Put under WithExpiry records expires_at; a Put without it clears it, so a
// replace always describes the value it wrote.
func TestPG_PutRecordsAndClearsExpiry(t *testing.T) {
	s, pool, _ := newPGStore(t)
	ctx := context.Background()
	owner, name := "exp-"+uuid.NewString(), "sign-in"
	t.Cleanup(func() { _ = s.For(owner).Delete(ctx, name) })
	at := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)

	expiry := func() *time.Time {
		var got *time.Time
		if err := pool.QueryRow(ctx, `SELECT expires_at FROM secrets WHERE owned_by=$1 AND name=$2`, owner, name).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if err := s.For(owner).Put(secretstore.WithExpiry(ctx, at), name, []byte("blob-v1")); err != nil {
		t.Fatal(err)
	}
	if got := expiry(); got == nil || !got.Equal(at) {
		t.Fatalf("expires_at = %v, want %v", got, at)
	}
	if err := s.For(owner).Put(ctx, name, []byte("blob-v2")); err != nil {
		t.Fatal(err)
	}
	if got := expiry(); got != nil {
		t.Fatalf("expires_at after a Put without an expiry = %v, want NULL", got)
	}
}

// DeleteExpired deletes a row whose expiry has passed and reports it, and
// keeps rows with no expiry, with one still ahead, and one renewed past it.
func TestPG_DeleteExpiredDeletesOnlyLapsedRows(t *testing.T) {
	s, pool, _ := newPGStore(t)
	ctx := context.Background()
	owner := "exp-" + uuid.NewString()
	st := s.For(owner)
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	seed := []struct {
		name string
		ctx  context.Context
	}{
		{"lapsed", secretstore.WithExpiry(ctx, past)},
		{"renewed", secretstore.WithExpiry(ctx, past)},
		{"ahead", secretstore.WithExpiry(ctx, future)},
		{"no-expiry", ctx},
	}
	for _, w := range seed {
		if err := st.Put(w.ctx, w.name, []byte("value-"+w.name)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Delete(ctx, w.name) })
	}
	// A renewal replaces the expiry it was stored with.
	if err := st.Put(secretstore.WithExpiry(ctx, future), "renewed", []byte("value-renewed-2")); err != nil {
		t.Fatal(err)
	}

	gone, err := s.DeleteExpired(ctx)
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	var mine []string
	for _, e := range gone {
		if e.Owner == owner {
			mine = append(mine, e.Name)
			if e.ExpiresAt.IsZero() {
				t.Errorf("expired row %q reported without its expiry", e.Name)
			}
		}
	}
	if len(mine) != 1 || mine[0] != "lapsed" {
		t.Fatalf("DeleteExpired removed %v of this owner's rows, want [lapsed]", mine)
	}
	if _, err := st.Get(ctx, "lapsed"); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("Get(lapsed) after the sweep = %v, want ErrNotFound", err)
	}
	for _, keep := range []string{"renewed", "ahead", "no-expiry"} {
		if _, err := st.Get(ctx, keep); err != nil {
			t.Fatalf("Get(%s) after the sweep = %v, want it kept", keep, err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM secrets WHERE owned_by=$1`, owner).Scan(&n); err != nil || n != 3 {
		t.Fatalf("rows left = %d (%v), want 3", n, err)
	}
}
