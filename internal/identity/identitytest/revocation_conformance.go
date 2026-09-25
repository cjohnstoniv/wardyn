// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package identitytest

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
)

// RunRevocationConformance holds an identity.RevocationStore to the kill-switch
// contract, so the in-memory store the provider tests run against and the
// Postgres store wardynd runs against cannot drift apart. newStore may return a
// store over shared state: every case works on fresh random run ids and jtis.
func RunRevocationConformance(t *testing.T, newStore func(t *testing.T) identity.RevocationStore) {
	ctx := context.Background()

	isRevoked := func(t *testing.T, s identity.RevocationStore, jti string, runID uuid.UUID) bool {
		t.Helper()
		revoked, err := s.IsRevoked(ctx, jti, runID)
		if err != nil {
			t.Fatalf("IsRevoked(%q, %s): %v", jti, runID, err)
		}
		return revoked
	}

	t.Run("nothing_revoked_reads_live", func(t *testing.T) {
		s := newStore(t)
		if isRevoked(t, s, uuid.NewString(), uuid.New()) {
			t.Fatal("a jti and run nobody revoked read as revoked")
		}
	})

	t.Run("revoke_run_cascades_to_every_jti_of_that_run_only", func(t *testing.T) {
		s := newStore(t)
		run, other := uuid.New(), uuid.New()
		if err := s.RevokeRun(ctx, run); err != nil {
			t.Fatalf("RevokeRun: %v", err)
		}
		// Tokens the store has never heard of: the cascade must not depend on
		// enumerating minted jtis, which is the whole point of a run-level mark.
		for _, jti := range []string{uuid.NewString(), uuid.NewString(), ""} {
			if !isRevoked(t, s, jti, run) {
				t.Fatalf("jti %q of a revoked run still verifies: the kill switch did not cascade", jti)
			}
		}
		if jti := uuid.NewString(); isRevoked(t, s, jti, other) {
			t.Fatalf("revoking run %s also revoked jti %q of unrelated run %s", run, jti, other)
		}
	})

	t.Run("revoke_jti_revokes_that_token_only", func(t *testing.T) {
		s := newStore(t)
		run := uuid.New()
		jti, sibling := uuid.NewString(), uuid.NewString()
		if err := s.RevokeJTI(ctx, jti, run); err != nil {
			t.Fatalf("RevokeJTI: %v", err)
		}
		if !isRevoked(t, s, jti, run) {
			t.Fatal("a revoked jti still verifies")
		}
		if isRevoked(t, s, sibling, run) {
			t.Fatal("revoking one jti revoked its run's other tokens: a jti revoke is not a run revoke")
		}
	})

	t.Run("revocations_are_idempotent", func(t *testing.T) {
		s := newStore(t)
		run, jti := uuid.New(), uuid.NewString()
		for i := 0; i < 2; i++ {
			if err := s.RevokeJTI(ctx, jti, run); err != nil {
				t.Fatalf("RevokeJTI #%d: %v", i+1, err)
			}
			if err := s.RevokeRun(ctx, run); err != nil {
				t.Fatalf("RevokeRun #%d: %v", i+1, err)
			}
		}
		if !isRevoked(t, s, jti, run) || !isRevoked(t, s, uuid.NewString(), run) {
			t.Fatal("a repeated revoke un-revoked the token or the run")
		}
	})
}
