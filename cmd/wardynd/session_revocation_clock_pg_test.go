// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// PIN for the finding that the revocation cutoff and the things compared against
// it were stamped on TWO CLOCKS.
//
// oidc_session_revocations.revoked_at comes from Postgres (`VALUES ($1, now())`).
// An SSO cookie's `iat` comes from wardynd's wall clock, and an API token's
// created_at used to. With wardynd AHEAD of the database by d, a credential
// minted BEFORE a revoke carries a timestamp AFTER the cutoff and survives it:
// the admin's "revoke every session for this human" silently does nothing for
// the next d.
//
// The skew is SIMULATED HONESTLY — by giving the adapter an APP CLOCK that runs
// ahead of the database's (pgSessionRevocations.now) and stamping the session's
// `iat` from that same clock, which is exactly what a fast wardynd does: both the
// stamp and the later age measurement come from the one fast clock. Handing the
// adapter a bare issuedAt from the future would NOT model that — the age helper
// clamps a stamp from its own future to zero (db.AppClockAgeMicros), so such a
// stamp reads as "minted just now", which is a different (and impossible) input.
// Nothing here changes the host's clock; it does not have to.
//
// Guarded by WARDYN_TEST_PG; skipped cleanly when unset.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// revocationPool connects to the live Postgres named by WARDYN_TEST_PG and
// migrates it, so oidc_session_revocations exists.
func revocationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping the Postgres-backed revocation-clock test")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to the server WARDYN_TEST_PG names: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestPG_RevokeBeatsASessionStampedByAClockThatRunsFast(t *testing.T) {
	pool := revocationPool(t)
	ctx := context.Background()
	// wardynd's clock is 10 minutes fast: every reading it takes — the `iat` it
	// stamps AND the age it later measures — is 10 minutes ahead of Postgres.
	const skew = 10 * time.Minute
	appClock := func() time.Time { return time.Now().Add(skew) }
	rev := &pgSessionRevocations{pool: pool, now: appClock}

	sub := "auth0|clock-skew-" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM oidc_session_revocations WHERE sub = $1`, sub)
	})

	// The session was minted BEFORE the revoke, in real time — stamped by the
	// fast clock, so its `iat` is 10 minutes in the database's future.
	issuedAt := appClock()
	// "Minted BEFORE the revoke" has to hold by more than the round-trip the
	// age measurement itself costs; the finding is a skew of minutes, so a few
	// ms of daylight between the stamp and the revoke is the honest margin.
	time.Sleep(25 * time.Millisecond)

	// No cutoff yet: nothing is revoked, however skewed the stamp.
	if revoked, err := rev.IsSessionRevoked(ctx, sub, "", issuedAt); err != nil {
		t.Fatalf("IsSessionRevoked before any revoke: %v", err)
	} else if revoked {
		t.Fatal("a session with NO revocation on record read as revoked; every login would be rejected")
	}

	// The admin revokes every session for this human. revoked_at is now(), on
	// the DATABASE's clock — which is 10 minutes BEHIND the session's stamp.
	if err := rev.RevokeSub(ctx, sub); err != nil {
		t.Fatalf("RevokeSub: %v", err)
	}

	revoked, err := rev.IsSessionRevoked(ctx, sub, "", issuedAt)
	if err != nil {
		t.Fatalf("IsSessionRevoked after the revoke: %v", err)
	}
	if !revoked {
		t.Fatalf("a session stamped %s ahead of the database's clock SURVIVED a revoke written after it. The cutoff "+
			"is Postgres's now() and the stamp is wardynd's, so for the length of the skew the admin's \"revoke "+
			"every session for this human\" silently does nothing — the finding, reproduced", skew)
	}

	// The other direction, so this is a rule and not a blanket "always revoked":
	// a session minted AFTER the cutoff — stamped by the same fast clock, in
	// real time after the revoke — still authenticates.
	time.Sleep(25 * time.Millisecond)
	later := appClock()
	if revoked, err := rev.IsSessionRevoked(ctx, sub, "", later); err != nil {
		t.Fatalf("IsSessionRevoked for a session minted after the cutoff: %v", err)
	} else if revoked {
		t.Fatal("a session minted after the revoke, on the same fast clock, read as revoked — the rule became a blanket \"always revoked\"")
	}
}

// TestPG_GlobalRevokeBeatsAFastClockToo covers the revoke-all arm, which writes
// the reserved sentinel row and is the one an operator reaches for in an
// incident.
func TestPG_GlobalRevokeBeatsAFastClockToo(t *testing.T) {
	pool := revocationPool(t)
	ctx := context.Background()
	const skew = 10 * time.Minute
	appClock := func() time.Time { return time.Now().Add(skew) }
	rev := &pgSessionRevocations{pool: pool, now: appClock}

	var restore *time.Time
	var had bool
	row := pool.QueryRow(ctx, `SELECT revoked_at FROM oidc_session_revocations WHERE sub = $1`, globalRevokeSub)
	var prev time.Time
	if err := row.Scan(&prev); err == nil {
		had, restore = true, &prev
	}
	t.Cleanup(func() {
		if had {
			_, _ = pool.Exec(context.Background(),
				`UPDATE oidc_session_revocations SET revoked_at = $2 WHERE sub = $1`, globalRevokeSub, *restore)
			return
		}
		_, _ = pool.Exec(context.Background(), `DELETE FROM oidc_session_revocations WHERE sub = $1`, globalRevokeSub)
	})

	issuedAt := appClock() // minted before the revoke, stamped by the fast clock
	time.Sleep(25 * time.Millisecond)
	if err := rev.RevokeAll(ctx); err != nil {
		t.Fatalf("RevokeAll: %v", err)
	}
	revoked, err := rev.IsSessionRevoked(ctx, "auth0|"+uuid.NewString(), "", issuedAt)
	if err != nil {
		t.Fatalf("IsSessionRevoked after RevokeAll: %v", err)
	}
	if !revoked {
		t.Fatal("a session stamped ahead of the database's clock survived a GLOBAL revoke — the incident-response " +
			"lever, not firing for the length of the skew")
	}
}
