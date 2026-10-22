// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// backdatedRevoke revokes sub with a cut stamped an hour in the past: the cut draws a later sequence than
// any credential issued so far, while its time sits before them, the shape a backward clock step leaves.
func backdatedRevoke(t *testing.T, pool *pgxpool.Pool, sub string) {
	t.Helper()
	if err := store.ExecRevocationCut(context.Background(), pool, `INSERT INTO oidc_session_revocations (sub, revoked_at)
		VALUES ($1, now() - interval '1 hour') ON CONFLICT (sub) DO UPDATE SET revoked_at = EXCLUDED.revoked_at`, sub, types.CLISessionEndRevoked); err != nil {
		t.Fatal(err)
	}
}

func tokenRevSeq(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) int64 {
	t.Helper()
	var seq int64
	if err := pool.QueryRow(context.Background(), `SELECT rev_seq FROM api_tokens WHERE id = $1`, id).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	return seq
}

// TestPG_ServiceTokenIsOrderedBySequence: a service token records the sponsor's revocation sequence, and
// its sponsor check reads it, so a revoke of the sponsor ends it however the two clocks compare.
func TestPG_ServiceTokenIsOrderedBySequence(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	tok := e.mustMint(t, e.sponsor, svcName)
	if seq := tokenRevSeq(t, e.pool, tok.ID); seq <= 0 {
		t.Fatalf("service token rev_seq = %d, want the sequence at mint", seq)
	}
	if got := e.as(t, tok.Token, http.MethodGet, "/api/v1/me", "").Code; got != http.StatusOK {
		t.Fatalf("service GET /me: %d, want 200", got)
	}
	backdatedRevoke(t, e.pool, svcSponsor)
	if got := e.as(t, tok.Token, http.MethodGet, "/api/v1/me", "").Code; got != http.StatusUnauthorized || e.lastAuthFail() != authFailedServiceSponsorInactive {
		t.Fatalf("after the sponsor's revoke: %d (auth.fail %q), want 401 %q", got, e.lastAuthFail(), authFailedServiceSponsorInactive)
	}
}

// TestPG_InvitationTokenIsOrderedBySequence: a token claimed through an invitation records the claimant's
// revocation sequence, so a revoke of the claimant ends it however the two clocks compare.
func TestPG_InvitationTokenIsOrderedBySequence(t *testing.T) {
	e := newInviteEnv(t)
	e.h.srv.cfg.SessionRevocations = &pgTestRevocations{pool: e.pool, st: e.st}
	inv := e.mustInvite(t, `{}`)
	tok := decodeToken(t, e.claim(t, e.pat, inv.ID))
	if seq := tokenRevSeq(t, e.pool, tok.ID); seq <= 0 {
		t.Fatalf("claimed token rev_seq = %d, want the sequence at the claim", seq)
	}
	if got := do(t, e.h.srv, http.MethodGet, "/api/v1/me", tok.Token, "").Code; got != http.StatusOK {
		t.Fatalf("claimed token GET /me: %d, want 200", got)
	}
	backdatedRevoke(t, e.pool, personSub)
	if got := do(t, e.h.srv, http.MethodGet, "/api/v1/me", tok.Token, "").Code; got != http.StatusUnauthorized {
		t.Fatalf("after the claimant's revoke: %d, want 401", got)
	}
}
