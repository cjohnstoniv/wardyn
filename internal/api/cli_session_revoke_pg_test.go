// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Guarded by WARDYN_TEST_PG (throwawayBarePGPool): skipped cleanly when unset, must PASS when set.

func cliAccessTokenID(t *testing.T, e *cliEnv, session uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM api_tokens
		WHERE cli_session_id = $1 AND kind = 'cli_access' AND revoked_at IS NULL`, session).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// Revoking a CLI sign-in's access token by id ends the sign-in: the next refresh is refused instead of
// minting a token from the identity the session was stamped with.
func TestPG_CLISession_RevokingTheAccessTokenEndsTheSession(t *testing.T) {
	e := newCLIEnv(t)
	c, g := e.signIn(e.user, nil)
	tokenID := cliAccessTokenID(t, e, g.SessionID)
	if w := e.req(http.MethodDelete, "/api/v1/me/tokens/"+tokenID.String(), "", e.user, "", "", ""); w.Code/100 != 2 {
		t.Fatalf("revoke = %d %s", w.Code, w.Body)
	}
	e.advance(time.Minute)
	wantCLIReason(t, c.refresh(g.SessionID, g.Secret), http.StatusUnauthorized, reasonCLISessionInvalid)
	got, err := e.pg.GetCLISession(context.Background(), g.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got.EndedAt == nil || got.EndReason != types.CLISessionEndRevoked {
		t.Fatalf("session after its access token was revoked = %+v, want ended, reason revoked", got)
	}
	if n := liveSessionTokens(t, e, g.SessionID); n != 0 {
		t.Fatalf("live access tokens of the ended session = %d, want 0", n)
	}
}

// The sweep a People-page demotion or re-type runs (revokeAPITokensFor) ends the person's CLI sign-in too, so
// a refresh cannot mint the type and groups the person no longer has.
func TestPG_CLISession_DemotionSweepEndsTheSession(t *testing.T) {
	e := newCLIEnv(t)
	c, g := e.signIn(e.user, nil)
	if n, err := e.srv.revokeAPITokensFor(context.Background(), "sub-user"); err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v, want 1 token", n, err)
	}
	e.advance(time.Minute)
	wantCLIReason(t, c.refresh(g.SessionID, g.Secret), http.StatusUnauthorized, reasonCLISessionInvalid)
	if got, _ := e.pg.GetCLISession(context.Background(), g.SessionID); got.EndedAt == nil {
		t.Fatalf("session after the sweep = %+v, want it ended", got)
	}
}

// A caller who knows only a session id cannot spend its refresh bucket: failed proofs are not charged to it.
func TestPG_CLISession_BadProofsDoNotSpendTheSessionBucket(t *testing.T) {
	e := newCLIEnv(t)
	c, g := e.signIn(e.user, nil)
	_, stranger, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	route := "/api/v1/cli/sessions/" + g.SessionID.String() + "/refresh"
	for i := 0; i < 12; i++ {
		wantCLIReason(t, c.refreshWith(g.SessionID, g.Secret, c.proofAt(route, g.SessionID, g.Secret, e.clock, stranger)),
			http.StatusUnauthorized, reasonCLISessionInvalid)
	}
	e.advance(time.Minute)
	if w := c.refresh(g.SessionID, g.Secret); w.Code != http.StatusOK {
		t.Fatalf("refresh after 12 bad proofs = %d %s, want 200", w.Code, w.Body)
	}
}

// Fifty refreshes leave the session's first (audited) access token, revoked, and its one live token: the
// tokens a refresh replaces are deleted, so no token list grows with use.
func TestPG_CLISession_RotationLeavesNoDeadRows(t *testing.T) {
	e := newCLIEnv(t)
	c, g := e.signIn(e.user, nil)
	first := cliAccessTokenID(t, e, g.SessionID)
	before, err := e.pg.ListAPITokens(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		e.advance(7 * time.Minute) // under the refresh limiters' rates
		w := c.refresh(g.SessionID, g.Secret)
		if w.Code != http.StatusOK {
			t.Fatalf("refresh %d = %d %s", i+1, w.Code, w.Body)
		}
		g = decodeJSON[types.CLISessionGrant](t, w)
	}
	var rows int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM api_tokens WHERE cli_session_id = $1`, g.SessionID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("api_tokens rows of the session after 50 refreshes = %d, want 2 (the first, revoked, and the live one)", rows)
	}
	var revoked bool
	if err := e.pool.QueryRow(context.Background(), `SELECT revoked_at IS NOT NULL FROM api_tokens WHERE id = $1`, first).Scan(&revoked); err != nil || !revoked {
		t.Fatalf("the session's first token: revoked=%v err=%v, want it kept and revoked", revoked, err)
	}
	after, err := e.pg.ListAPITokens(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("ListAPITokens = %d rows after 50 refreshes, want %d", len(after), len(before)+1)
	}
	if n := liveSessionTokens(t, e, g.SessionID); n != 1 {
		t.Fatalf("live access tokens = %d, want 1", n)
	}
}
