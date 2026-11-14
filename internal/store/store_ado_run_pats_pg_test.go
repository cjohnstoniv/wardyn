// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// TestPG_RunPATs runs the RunPATStore contract against Postgres (migration
// 0102), on a throwaway database so the listings start empty. Guarded by
// WARDYN_TEST_PG; skipped cleanly when unset.
func TestPG_RunPATs(t *testing.T) {
	runPATContract(t, store.NewPG(runsPGPoolIsolated(t)))
}

// An erase deletes a person's settled token rows but keeps one whose revoke
// Azure DevOps refused while its valid_to is ahead, beside the open ones, and
// counts both as left: the token may still work, and the next erase reads the
// row to say so. Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
func TestPG_DeleteSettledRunPATsOfKeepsARefusedToken(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	now := time.Now().UTC()
	insert := func(validTo time.Time, reason, lastError string) uuid.UUID {
		t.Helper()
		run := uuid.New()
		if err := st.InsertRunPAT(ctx, store.RunPAT{RunID: run, AuthorizationID: run, Owner: "oidc:alice", ProviderRowID: "ado",
			Org: "contoso", Scope: "vso.code", ValidTo: validTo}); err != nil {
			t.Fatalf("InsertRunPAT: %v", err)
		}
		if reason != "" {
			if _, err := st.MarkRunPATRevoked(ctx, run, run, reason, lastError); err != nil {
				t.Fatal(err)
			}
		}
		return run
	}
	ahead, past := now.Add(time.Hour), now.Add(-time.Hour)
	refused := insert(ahead, "offboarding", "HTTP 400")
	insert(ahead, "offboarding", "")        // revoked: deleted
	insert(ahead, "expired", "HTTP 503")    // closed as expired: deleted
	insert(past, "offboarding", "HTTP 400") // refused, but past its valid_to: deleted
	insert(ahead, "", "")                   // open: kept

	deleted, left, err := st.DeleteSettledRunPATsOf(ctx, []string{"oidc:alice"})
	if err != nil || deleted != 3 || left != 2 {
		t.Fatalf("DeleteSettledRunPATsOf = %d deleted, %d left, %v; want 3 and 2 (the refused and the open row kept)", deleted, left, err)
	}
	if got, err := st.ListRefusedRunPATs(ctx, "oidc:alice", now); err != nil || len(got) != 1 || got[0].RunID != refused {
		t.Errorf("ListRefusedRunPATs after the erase = %+v, %v; want the refused token still listed", got, err)
	}
}

// The run token sweep's delete takes only what an erase kept of a refused token: a refused row past
// its valid_to, created before its owner's last sign-in end, when that end was an erase. A refused row
// still inside its valid_to, a row revoked or closed as expired, one created after the erase, and one
// of a person who only disconnected, or who has no end, all stay. Guarded by WARDYN_TEST_PG; skipped
// cleanly when unset.
func TestPG_DeleteErasedRefusedRunPATs(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	now := time.Now().UTC()
	insert := func(owner string, validTo time.Time, reason, lastError string) uuid.UUID {
		t.Helper()
		run := uuid.New()
		if err := st.InsertRunPAT(ctx, store.RunPAT{RunID: run, AuthorizationID: run, Owner: owner, ProviderRowID: "ado",
			Org: "contoso", Scope: "vso.code", ValidTo: validTo}); err != nil {
			t.Fatalf("InsertRunPAT: %v", err)
		}
		if _, err := st.MarkRunPATRevoked(ctx, run, run, reason, lastError); err != nil {
			t.Fatal(err)
		}
		return run
	}
	end := func(owner, reason string) {
		t.Helper()
		if err := st.BeginADOSignInEnd(ctx, owner, reason); err != nil {
			t.Fatal(err)
		}
		if err := st.FinishADOSignInEnd(ctx, owner); err != nil {
			t.Fatal(err)
		}
	}
	ahead, past := now.Add(time.Hour), now.Add(-time.Hour)
	gone := []uuid.UUID{
		insert("oidc:alice", past, "offboarding", "HTTP 400"), // the erase's own revoke was refused
		insert("oidc:alice", past, "run_end", "HTTP 403"),     // refused before the erase, which kept it
	}
	kept := []uuid.UUID{
		insert("oidc:alice", ahead, "offboarding", "HTTP 400"), // refused, still inside its valid_to
		insert("oidc:alice", past, "offboarding", ""),          // revoked
		insert("oidc:alice", past, "expired", "HTTP 503"),      // closed as expired
		insert("oidc:bob", past, "run_end", "HTTP 400"),        // bob only disconnected
		insert("oidc:carol", past, "run_end", "HTTP 400"),      // carol's sign-in never ended
	}
	end("oidc:alice", "offboarding")
	end("oidc:bob", "disconnect")
	kept = append(kept, insert("oidc:alice", past, "run_end", "HTTP 400")) // created after the erase

	if n, err := st.DeleteErasedRefusedRunPATs(ctx, "offboarding"); err != nil || n != 2 {
		t.Fatalf("DeleteErasedRefusedRunPATs = %d, %v; want 2, the refused rows the erase kept", n, err)
	}
	for want, runs := range map[int][]uuid.UUID{0: gone, 1: kept} {
		for _, run := range runs {
			if rows, err := st.ListRunPATs(ctx, run); err != nil || len(rows) != want {
				t.Errorf("run %s: %d rows, %v; want %d", run, len(rows), err, want)
			}
		}
	}
}
