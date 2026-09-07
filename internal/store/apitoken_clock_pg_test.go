// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// PIN for the storage half of the two-clocks finding: api_tokens.created_at is
// written on the DATABASE's clock, back-dated by the request's own age, so it is
// comparable with oidc_session_revocations.revoked_at — which Postgres stamps.
//
// Two properties, and they pull against each other, which is why both are here:
//
//   - THE CLOCK (this finding). A created_at bound straight from wardynd's clock
//     can land AFTER a cutoff written before it, so a token minted before a
//     revoke survives the revoke.
//   - THE ADMISSION TIME (F143). The API stamps CreatedAt when the request is
//     ADMITTED, before it reads the body, precisely so a caller who holds a mint
//     request open across POST /sessions/revoke cannot land a created_at after
//     the cutoff. A plain now() would fix the first and re-open the second.
//
// Guarded by WARDYN_TEST_PG; skipped cleanly when unset.

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_CreateAPITokenWritesCreatedAtOnTheDatabaseClock(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	dbNow := func() time.Time {
		t.Helper()
		var now time.Time
		if err := pool.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
			t.Fatalf("read the database clock: %v", err)
		}
		return now
	}

	mint := func(name string, createdAt time.Time) types.APIToken {
		t.Helper()
		out, err := st.CreateAPIToken(ctx, types.APIToken{
			ID:        uuid.New(),
			Principal: "auth0|" + uuid.NewString(),
			Role:      "member",
			Name:      name,
			CreatedAt: createdAt,
		}, "wdn_"+uuid.NewString())
		if err != nil {
			t.Fatalf("CreateAPIToken(%s): %v", name, err)
		}
		t.Cleanup(func() { _, _ = st.RevokeAPIToken(context.Background(), out.ID, "", time.Now().UTC()) })
		return out
	}

	// (1) THE FINDING. wardynd's clock is 10 minutes fast, so the admission
	// stamp is 10 minutes in the database's future. The row must NOT be.
	const skew = 10 * time.Minute
	before := dbNow()
	fast := mint("clock-ahead", time.Now().Add(skew))
	after := dbNow()
	if fast.CreatedAt.After(after) {
		t.Errorf("created_at = %s, which is AFTER the database's own clock (%s). A token born in the future of the "+
			"clock the revocation cutoff is stamped from survives a revoke written after it — for the length of the "+
			"skew, \"revoke every session for this human\" does nothing", fast.CreatedAt, after)
	}
	if fast.CreatedAt.Before(before.Add(-time.Minute)) {
		t.Errorf("created_at = %s, well before the mint began (%s); the back-dating is subtracting something other "+
			"than the request's age", fast.CreatedAt, before)
	}

	// (2) F143, NOT RE-OPENED. A request admitted 30s ago — a caller holding the
	// body open — must keep its ADMISSION time, not pick up the insert time, or
	// a mint held across a revoke lands after the cutoff.
	const held = 30 * time.Second
	admitted := time.Now().Add(-held)
	slow := mint("held-open", admitted)
	age := dbNow().Sub(slow.CreatedAt)
	if age < held-5*time.Second {
		t.Errorf("a request admitted %s ago produced a created_at only %s old. It has picked up the INSERT time "+
			"instead of the admission time, which is F143: a caller who holds the mint body open across "+
			"POST /sessions/revoke gets a token stamped after the cutoff", held, age)
	}
	if age > held+time.Minute {
		t.Errorf("a request admitted %s ago produced a created_at %s old; the age arithmetic is off by more than "+
			"any plausible round trip", held, age)
	}

	// (3) The ordinary case is ordinary: an as-of-now stamp lands at as-of-now.
	fresh := mint("fresh", time.Now())
	if d := dbNow().Sub(fresh.CreatedAt); d < 0 || d > time.Minute {
		t.Errorf("a freshly-stamped token's created_at is %s away from the database's clock, want within a minute", d)
	}
}
