// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// CLI sign-in's store half (migration 0198_cli_sign_in). Guarded by WARDYN_TEST_PG like the rest of this
// package's Postgres tests: skipped cleanly when unset, must PASS when set.
package store_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func cliTestKey(t *testing.T) []byte {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

// seedCLISession writes a live CLI session of principal with one access token and returns both and the
// token's plaintext.
func seedCLISession(t *testing.T, st store.PG, principal, email string) (store.CLISessionRow, types.APIToken, string) {
	t.Helper()
	now := time.Now().UTC()
	sess := store.CLISessionRow{ID: uuid.New(), Identity: store.CLIIdentity{Principal: principal, Email: email, Role: "admin", UserType: types.UserTypeStandard, RevSeq: 1},
		IdentityStampedAt: now, Machine: "laptop", Store: types.CLIStoreKeychain, PublicKey: cliTestKey(t), CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
	exp := now.Add(time.Hour)
	raw := "wdn_" + uuid.NewString()
	tok, err := st.CreateCLISession(context.Background(), sess, "wdc_"+uuid.NewString(), types.APIToken{ID: uuid.New(), Principal: principal, Email: email,
		Role: "admin", UserType: types.UserTypeStandard, Name: "CLI: laptop", CreatedAt: now, ExpiresAt: &exp,
		Kind: types.APITokenKindCLIAccess, CLISessionID: &sess.ID}, raw)
	if err != nil {
		t.Fatalf("CreateCLISession: %v", err)
	}
	return sess, tok, raw
}

func TestPG_CLISignIn_RedeemOnceAndPendingCap(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	now := time.Now().UTC()
	newReq := func() store.CLISignIn {
		return store.CLISignIn{ID: uuid.New(), UserCode: "ABCD-EFGH", Challenge: "c", Port: 4711, Machine: "laptop", PublicKey: cliTestKey(t),
			Mode: types.CLISignInModeLoopback, Store: types.CLIStoreKeychain, RequestAddr: "10.0.0.1"}
	}
	si := newReq()
	if ok, err := st.CreateCLISignIn(ctx, si, 2, now); !ok || err != nil {
		t.Fatalf("create = %v, %v", ok, err)
	}
	if ok, err := st.CreateCLISignIn(ctx, newReq(), 2, now); !ok || err != nil {
		t.Fatalf("second create = %v, %v", ok, err)
	}
	if ok, err := st.CreateCLISignIn(ctx, newReq(), 2, now); ok || err != nil {
		t.Fatalf("a create past the pending cap = %v, %v; want refused", ok, err)
	}
	got, err := st.GetCLISignIn(ctx, si.ID)
	if err != nil || !got.ExpiresAt.Equal(now.Add(types.CLISignInRequestLife).Truncate(time.Microsecond)) {
		t.Fatalf("read back = %+v, %v; want it to expire 10 minutes after create", got, err)
	}
	if _, err := st.RedeemCLISignIn(ctx, si.ID, "k", now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("redeem before approval = %v, want ErrNotFound", err)
	}
	truncated := false
	if _, err := st.DecideCLISignIn(ctx, si.ID, store.CLISignInDecision{Decision: types.CLISignInApproved, Now: now, AuthorizedAt: now,
		Code: "k", CodeExpiresAt: now.Add(time.Minute), Approver: store.CLIIdentity{Principal: "sub-a", Role: "user", UserType: "standard",
			Groups: []string{"g"}, GroupsTruncated: &truncated, RevSeq: 7, AuthorityEpoch: 3}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DecideCLISignIn(ctx, si.ID, store.CLISignInDecision{Decision: types.CLISignInDenied, Now: now}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a second decision = %v, want ErrNotFound", err)
	}
	if _, err := st.RedeemCLISignIn(ctx, si.ID, "wrong", now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("redeem with the wrong code = %v, want ErrNotFound", err)
	}
	if _, err := st.RedeemCLISignIn(ctx, si.ID, "k", now.Add(time.Minute)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("redeem at the code's expiry = %v, want ErrNotFound", err)
	}
	red, err := st.RedeemCLISignIn(ctx, si.ID, "k", now.Add(59*time.Second))
	if err != nil || red.Approver.Principal != "sub-a" || red.Approver.RevSeq != 7 || red.Approver.AuthorityEpoch != 3 ||
		len(red.Approver.Groups) != 1 || red.Approver.GroupsTruncated == nil || *red.Approver.GroupsTruncated {
		t.Fatalf("redeem = %+v, %v; want the approver's snapshot", red, err)
	}
	if _, err := st.RedeemCLISignIn(ctx, si.ID, "k", now.Add(59*time.Second)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a second redeem = %v, want ErrNotFound: the code is single use", err)
	}
}

// A rotation is compare-and-set on the generation; an end revokes the session's tokens; and a CLI token
// of an ended session stops authenticating even when nothing revoked it.
func TestPG_CLISession_RotateEndAndTokenLiveness(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	sess, _, raw := seedCLISession(t, st, "sub-a", "a@corp.example")
	if tok, err := st.GetAPITokenByRaw(ctx, raw); err != nil || tok.Kind != types.APITokenKindCLIAccess || tok.CLISessionID == nil || *tok.CLISessionID != sess.ID {
		t.Fatalf("access token = %+v, %v; want a live cli_access token of the session", tok, err)
	}
	now := time.Now().UTC()
	exp := now.Add(time.Hour)
	next := func(raw string) types.APIToken {
		return types.APIToken{ID: uuid.New(), Principal: "sub-a", Role: "admin", UserType: types.UserTypeStandard, Name: "CLI: laptop",
			CreatedAt: now, ExpiresAt: &exp, Kind: types.APITokenKindCLIAccess, CLISessionID: &sess.ID}
	}
	raw2 := "wdn_" + uuid.NewString()
	if _, err := st.RotateCLISession(ctx, sess.ID, 1, "wdc_new", now, next(raw2), raw2); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	got, _ := st.GetCLISession(ctx, sess.ID)
	if got.Generation != 2 || !got.CLISecretMatches("wdc_new") {
		t.Fatalf("after rotate: %+v, want generation 2 holding the new secret", got)
	}
	raw3 := "wdn_" + uuid.NewString()
	if _, err := st.RotateCLISession(ctx, sess.ID, 1, "wdc_again", now, next(raw3), raw3); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a rotate from a spent generation = %v, want ErrConflict", err)
	}
	if _, err := st.GetAPITokenByRaw(ctx, raw3); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the losing rotation's token = %v, want never written", err)
	}

	// An end that leaves a token unrevoked (a writer outside EndCLISession) still ends it.
	if _, err := pool.Exec(ctx, `UPDATE cli_sessions SET ended_at = now(), end_reason = 'signed_out' WHERE id = $1`, sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAPITokenByRaw(ctx, raw2); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a token of an ended session = %v, want ErrNotFound", err)
	}

	other, _, otherRaw := seedCLISession(t, st, "sub-a", "a@corp.example")
	ended, err := st.EndCLISession(ctx, other.ID, "sub-b", types.CLISessionEndSignedOut, now)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("someone else's end = %+v, %v; want ErrNotFound", ended, err)
	}
	if ended, err = st.EndCLISession(ctx, other.ID, "sub-a", types.CLISessionEndSignedOut, now); err != nil || ended.EndReason != types.CLISessionEndSignedOut {
		t.Fatalf("own end = %+v, %v", ended, err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM api_tokens WHERE cli_session_id = $1 AND revoked_at IS NULL`, other.ID); n != 0 {
		t.Errorf("%d live token rows after the end, want 0", n)
	}
	if _, err := st.GetAPITokenByRaw(ctx, otherRaw); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an ended session's token = %v, want ErrNotFound", err)
	}
	if _, err := st.CreateCLIAdminToken(ctx, other.ID, next("x"), "wdn_"+uuid.NewString()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an admin token for an ended session = %v, want ErrNotFound", err)
	}
	if list, err := st.ListCLISessions(ctx, "sub-a", now); err != nil || len(list) != 0 {
		t.Fatalf("live sessions = %+v, %v; want none", list, err)
	}
}

// Every store writer of a revocation cutoff ends the person's CLI sessions in its own transaction: a
// suspension and an identity unbind (cmd/wardynd's census guard pins that each one runs it).
func TestPG_CLISession_StoreCutsEndSessions(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	leaver := signedIn(t, st, "sub-leaver", "leaver@corp.example", leaverOID)
	sess, _, raw := seedCLISession(t, st, "sub-leaver", "leaver@corp.example")
	keep, _, keepRaw := seedCLISession(t, st, "sub-stays", "stays@corp.example")
	if _, err := st.SuspendIdentity(ctx, store.SuspendPlan{IdentityID: leaver.ID, Principal: "sub-leaver", Principals: []string{"sub-leaver"},
		CutoffSubs: []string{"sub-leaver"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetCLISession(ctx, sess.ID); got.EndedAt == nil || got.EndReason != types.CLISessionEndRevoked {
		t.Fatalf("leaver's CLI session after suspension = %+v, want ended as revoked", got)
	}
	if _, err := st.GetAPITokenByRaw(ctx, raw); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("leaver's access token = %v, want ErrNotFound", err)
	}
	if got, _ := st.GetCLISession(ctx, keep.ID); got.EndedAt != nil {
		t.Fatalf("another person's CLI session ended: %+v", got)
	}
	if _, err := st.GetAPITokenByRaw(ctx, keepRaw); err != nil {
		t.Fatalf("another person's access token: %v", err)
	}

	row := signedIn(t, st, "sub-unbound", "unbound@corp.example", "aaaaaaaa-0000-4000-8000-000000000002")
	unbound, _, _ := seedCLISession(t, st, "sub-unbound", "unbound@corp.example")
	// Unbind refuses a principal that holds a live token, so the session's token has expired already.
	if _, err := pool.Exec(ctx, `UPDATE api_tokens SET expires_at = now() - interval '1 second' WHERE cli_session_id = $1`, unbound.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.UnbindIdentity(ctx, row.ID, "sub-unbound"); err != nil {
		t.Fatalf("UnbindIdentity: %v", err)
	}
	if got, _ := st.GetCLISession(ctx, unbound.ID); got.EndedAt == nil || got.EndReason != types.CLISessionEndRevoked {
		t.Fatalf("CLI session after unbind = %+v, want ended as revoked", got)
	}
}

// A browser sign-in re-stamps the person's live CLI sessions as it re-stamps their tokens.
func TestPG_CLISession_LoginRestampsLiveSessions(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	sess, _, _ := seedCLISession(t, st, "sub-a", "a@corp.example")
	if _, err := st.Pool.Exec(ctx, `UPDATE cli_sessions SET identity_stamped_at = now() - interval '2 days' WHERE id = $1`, sess.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.RefreshAPITokenIdentity(ctx, "sub-a", "user", types.UserTypeStandard, []string{"eng"}, false); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetCLISession(ctx, sess.ID)
	if err != nil || got.Identity.Role != "user" || len(got.Identity.Groups) != 1 || time.Since(got.IdentityStampedAt) > time.Minute {
		t.Fatalf("session after a sign-in = %+v, %v; want the new role, groups and a fresh stamp", got, err)
	}
}
