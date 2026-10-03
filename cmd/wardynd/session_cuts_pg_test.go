// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// A session cut ends browser sessions and the grants issued under one, and nothing else: an API token or an
// SSH key (SessionStatus with no epoch) is not read against it, which is what lets a group removal keep the
// tokens whose group snapshot does not hold the group.
//
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.
func TestPGSessionRevocations_CutSessionsLeavesTokensAndKeys(t *testing.T) {
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed session-cut test")
	}
	ctx := t.Context()
	pool, err := connectAndMigrate(ctx, dsn, "", 30*time.Second, 60*time.Second, false)
	if err != nil {
		t.Fatalf("connectAndMigrate: %v", err)
	}
	defer pool.Close()
	rev := &pgSessionRevocations{pool: pool, now: func() time.Time { return pgNow(t, pool) }}
	sub := fmt.Sprintf("sub-cut-probe-%d", time.Now().UnixNano())
	issued := time.Now().UTC().Add(-time.Hour)

	if err := rev.CutSessions(ctx, sub); err != nil {
		t.Fatalf("CutSessions: %v", err)
	}
	if st, err := rev.SessionStatus(ctx, sub, "", issued, 0); err != nil || st == oidc.SessionLive {
		t.Errorf("a session cookie issued before the cut = (%v, %v), want it cut", st, err)
	}
	if revoked, err := rev.IsSessionRevoked(ctx, sub, "", issued); err != nil || !revoked {
		t.Errorf("IsSessionRevoked for a grant issued before the cut = (%v, %v), want true", revoked, err)
	}
	if st, err := rev.SessionStatus(ctx, sub, "", issued, -1); err != nil || st != oidc.SessionLive {
		t.Errorf("an API token or SSH key created before the cut = (%v, %v), want it live", st, err)
	}
	if st, err := rev.SessionStatus(ctx, sub, "", time.Now().UTC().Add(time.Hour), 0); err != nil || st != oidc.SessionLive {
		t.Errorf("a session issued after the cut = (%v, %v), want it live", st, err)
	}
	// A revocation still ends the token too.
	if err := rev.RevokeSub(ctx, sub); err != nil {
		t.Fatalf("RevokeSub: %v", err)
	}
	if st, err := rev.SessionStatus(ctx, sub, "", issued, -1); err != nil || st != oidc.SessionRevoked {
		t.Errorf("a token created before a RevokeSub = (%v, %v), want revoked", st, err)
	}
}
