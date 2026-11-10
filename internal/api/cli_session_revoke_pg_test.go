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

	"github.com/cjohnstoniv/wardyn/internal/store"
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

// sweepRaceStore runs afterList once, right after a revoke sweep has read its snapshot of a person's tokens.
type sweepRaceStore struct {
	store.PG
	afterList func()
}

func (s *sweepRaceStore) ListAPITokensByIdentity(ctx context.Context, name string) ([]types.APIToken, error) {
	toks, err := s.PG.ListAPITokensByIdentity(ctx, name)
	if f := s.afterList; f != nil {
		s.afterList = nil
		f()
	}
	return toks, err
}

// A refresh that lands between the demotion sweep's snapshot and its revoke replaces (first rotation: revokes;
// later ones: deletes) the access token the snapshot holds. The sweep still ends the session by the id the
// snapshot carries, so the next refresh is refused and the token the racing refresh minted is dead.
func TestPG_CLISession_DemotionSweepEndsASessionThatRefreshedMeanwhile(t *testing.T) {
	for name, earlier := range map[string]int{"first rotation": 0, "later rotation": 1} {
		t.Run(name, func(t *testing.T) {
			e := newCLIEnv(t)
			c, g := e.signIn(e.user, nil)
			refresh := func() {
				t.Helper()
				e.advance(7 * time.Minute)
				w := c.refresh(g.SessionID, g.Secret)
				if w.Code != http.StatusOK {
					t.Fatalf("refresh = %d %s", w.Code, w.Body)
				}
				g = decodeJSON[types.CLISessionGrant](t, w)
			}
			for i := 0; i < earlier; i++ {
				refresh()
			}
			raced := g
			e.srv.cfg.Store = &sweepRaceStore{PG: e.pg, afterList: func() {
				refresh()
				raced = g
			}}
			if n, err := e.srv.revokeAPITokensFor(context.Background(), "sub-user"); err != nil || n != 1 {
				t.Fatalf("sweep = %d, %v, want 1 token and no error", n, err)
			}
			got, err := e.pg.GetCLISession(context.Background(), g.SessionID)
			if err != nil || got.EndedAt == nil || got.EndReason != types.CLISessionEndRevoked {
				t.Fatalf("session after the sweep = %+v, %v; want ended, reason revoked", got, err)
			}
			if n := liveSessionTokens(t, e, g.SessionID); n != 0 {
				t.Fatalf("live access tokens after the sweep = %d, want 0 (the racing refresh's token included)", n)
			}
			if code, _ := e.me(raced.AccessToken); code != http.StatusUnauthorized {
				t.Fatalf("the racing refresh's access token = %d as a bearer, want 401", code)
			}
			e.advance(time.Minute)
			wantCLIReason(t, c.refresh(g.SessionID, g.Secret), http.StatusUnauthorized, reasonCLISessionInvalid)
		})
	}
}

// A sweep that reaches the session while a rotation holds its row lock waits for the lock, then ends the
// session: it does not read the token row the rotation is about to replace, and does not roll back.
func TestPG_CLISession_DemotionSweepWaitsOnTheSessionLock(t *testing.T) {
	e := newCLIEnv(t)
	c, g := e.signIn(e.user, nil)
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM cli_sessions WHERE id = $1 FOR UPDATE`, g.SessionID); err != nil {
		t.Fatal(err)
	}
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := e.srv.revokeAPITokensFor(ctx, "sub-user")
		done <- result{n, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND datname = current_database()`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the sweep never waited on the session lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case r := <-done:
		t.Fatalf("the sweep finished while the session was locked: %+v", r)
	default:
	}
	// The rotation's side of the lock: the session moves on a generation, then the lock is released.
	if _, err := tx.Exec(ctx, `UPDATE cli_sessions SET generation = generation + 1 WHERE id = $1`, g.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.err != nil || r.n != 1 {
		t.Fatalf("sweep = %d, %v, want 1 token and no error", r.n, r.err)
	}
	if got, _ := e.pg.GetCLISession(ctx, g.SessionID); got.EndedAt == nil || got.EndReason != types.CLISessionEndRevoked {
		t.Fatalf("session after the sweep = %+v, want ended, reason revoked", got)
	}
	e.advance(time.Minute)
	wantCLIReason(t, c.refresh(g.SessionID, g.Secret), http.StatusUnauthorized, reasonCLISessionInvalid)
}
