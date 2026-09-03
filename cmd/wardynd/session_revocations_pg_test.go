// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// A revocation is a CUTOFF, not a flag — and that is the property that makes
// the security-admin tier's reach over a super admin acceptable rather than a
// lockout primitive (docs/OPERATIONS.md's security-admin section; the rationale
// on handleRevokeSessions in internal/api/sessions.go). It was asserted in
// three places in prose and pinned in none: pgSessionRevocations had no test at
// all, so `!issuedAt.After(cutoff)` could have been written `true` and every
// package stayed green while "sign in again to recover" quietly became false.
//
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.
// Run: WARDYN_TEST_PG="postgres://wardyn:wardyn@127.0.0.1:55434/wardyn?sslmode=disable" \
//        go test ./cmd/wardynd/... -run TestPGSessionRevocations

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func TestPGSessionRevocations_CutoffIsATimestampNotAFlag(t *testing.T) {
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed session-revocation test")
	}
	ctx := t.Context()
	pool, err := connectAndMigrate(ctx, dsn, "", 30*time.Second, 60*time.Second)
	if err != nil {
		t.Fatalf("connectAndMigrate: %v", err)
	}
	defer pool.Close()
	rev := &pgSessionRevocations{pool: pool}

	// A sub unique to this run: the table is keyed on sub and shared with any
	// other test using this database, so nothing here may collide with — or
	// clean up after — anyone else. The global (revoke-all) row is deliberately
	// NOT exercised for the same reason; its comparison is the same MAX.
	sub := fmt.Sprintf("sub-revocation-probe-%d", time.Now().UnixNano())

	// Before any revoke: nothing on record, so nothing is revoked. This is the
	// arm that must not read a SQL NULL as a zero-time cutoff.
	old := time.Now().UTC().Add(-time.Hour)
	if revoked, err := rev.IsSessionRevoked(ctx, sub, old); err != nil || revoked {
		t.Fatalf("IsSessionRevoked before any revoke = (%v, %v), want (false, nil) — a sub with no row must not read as revoked", revoked, err)
	}

	if err := rev.RevokeSub(ctx, sub); err != nil {
		t.Fatalf("RevokeSub: %v", err)
	}

	// The session held at revoke time is cut.
	if revoked, err := rev.IsSessionRevoked(ctx, sub, old); err != nil || !revoked {
		t.Fatalf("IsSessionRevoked for a session issued BEFORE the cutoff = (%v, %v), want (true, nil) — the revoke did not bite", revoked, err)
	}

	// THE RECOVERY, and the whole point: signing in again mints a session
	// issued AFTER the cutoff, and that session is not revoked. No operator
	// action, no second call, no row to clear. Flip the comparison to a flag
	// ("any row means revoked") and this is the assertion that reddens — while
	// the one above still passes, which is why both are here.
	fresh := time.Now().UTC().Add(time.Minute)
	if revoked, err := rev.IsSessionRevoked(ctx, sub, fresh); err != nil || revoked {
		t.Fatalf("IsSessionRevoked for a session issued AFTER the cutoff = (%v, %v), want (false, nil) — "+
			"a revoke would be a permanent lockout instead of a cutoff, and a security admin could hold a super admin out", revoked, err)
	}

	// A second revoke moves the cutoff LATER (idempotent, not additive), so a
	// session minted by the re-login above is cut again — the operator's answer
	// to "they signed straight back in". `between` stands for that re-login: a
	// real timestamp taken after the first cutoff, not a far-future one, since
	// only a real one can be overtaken by the second revoke.
	time.Sleep(20 * time.Millisecond)
	between := time.Now().UTC()
	if revoked, err := rev.IsSessionRevoked(ctx, sub, between); err != nil || revoked {
		t.Fatalf("a session minted just after the revoke = (%v, %v), want (false, nil) — the re-login did not clear the cutoff", revoked, err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := rev.RevokeSub(ctx, sub); err != nil {
		t.Fatalf("second RevokeSub: %v", err)
	}
	if revoked, err := rev.IsSessionRevoked(ctx, sub, between); err != nil || !revoked {
		t.Fatalf("after a second revoke, the re-login session = (%v, %v), want (true, nil) — the cutoff did not move, so a repeat revoke is a no-op", revoked, err)
	}

	// A pre-D16 cookie carries no issued-at. It has no reliable timestamp to
	// compare, so it fails CLOSED the moment any cutoff exists — see
	// oidc.SessionRevocations' doc.
	if revoked, err := rev.IsSessionRevoked(ctx, sub, time.Time{}); err != nil || !revoked {
		t.Fatalf("IsSessionRevoked for a zero issued-at = (%v, %v), want (true, nil) — a cookie with no iat must fail closed", revoked, err)
	}

	// And a DIFFERENT principal is untouched: the cutoff is per-sub, so
	// revoking one human never logs out another.
	other := sub + "-bystander"
	if revoked, err := rev.IsSessionRevoked(ctx, other, old); err != nil || revoked {
		t.Fatalf("IsSessionRevoked for an unrelated sub = (%v, %v), want (false, nil)", revoked, err)
	}
}
