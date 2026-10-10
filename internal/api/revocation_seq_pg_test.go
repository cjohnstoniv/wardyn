// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// stepBack models the host clock stepping back between a credential's issue and a revoke: the cut's
// revoked_at is stamped earlier than every credential issued before it.
const stepBack = 800 * time.Millisecond

// The credentials a session mints record the session's revocation sequence, and a revoke made after a
// backward step of the clock ends each of them, though their issue times read later than the cutoff: the
// session cookie, the API token it minted, the SSH key it registered, the attach ticket it opened and the delegated token a portal holds.
func TestPG_RevokeEndsEveryCredentialIssuedBeforeABackwardClockStep(t *testing.T) {
	var rev *pgTestRevocations
	e := newEntraPeoplePG(t, func(p peoplePG, c *oidc.Config) {
		rev = &pgTestRevocations{pool: p.pool, st: p.st}
		c.Identities, c.Revocations = NewIdentityGate(p.st), rev
	})
	e.h.srv.cfg.SessionRevocations = rev
	e.h.srv.router = e.h.srv.routes()
	ctx := context.Background()

	e.signIn("pairwise-seq", entraTenant, entraObject)
	_, sess := e.callback(t)
	if sess == nil {
		t.Fatal("sign-in refused")
	}
	w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/me/tokens", sess, `{"name":"ci"}`)
	minted := decodeToken(t, w)
	tok, err := e.st.GetAPITokenByRaw(ctx, minted.Token)
	if err != nil {
		t.Fatal(err)
	}
	if tok.RevSeq < 1 {
		t.Fatalf("the minted token recorded rev_seq %d, want the session's sequence", tok.RevSeq)
	}
	if w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/me/ssh-keys", sess,
		`{"name":"laptop","public_key":"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBl3jvXfmZbBd3q5aLKZTv3rIcvKlfz2eYQpuYSGfCPT pm@laptop"}`); w.Code != http.StatusCreated {
		t.Fatalf("register key: %d %s", w.Code, w.Body.String())
	}
	keys, err := e.st.ListSSHKeysByPrincipal(ctx, "pairwise-seq")
	if err != nil || len(keys) != 1 || keys[0].RevSeq != tok.RevSeq {
		t.Fatalf("keys = %+v, %v; want one carrying the session's sequence %d", keys, err, tok.RevSeq)
	}
	run := uuid.New()
	issue, err := e.h.srv.mintIssue(oidc.WithRevocationSeq(ctx, "pairwise-seq", tok.RevSeq), "pairwise-seq", time.Now().UTC())
	if err != nil || issue.Seq != tok.RevSeq {
		t.Fatalf("a credential minted under the session recorded %+v, %v; want its sequence %d", issue, err, tok.RevSeq)
	}
	ticket, err := mintAttachTicket(oidc.WithRevocationSeq(ctx, "pairwise-seq", tok.RevSeq), e.st, run, types.ActorHuman, "pairwise-seq", oidc.RoleUser, issue)
	if err != nil {
		t.Fatal(err)
	}

	d, err := e.st.CreateDelegate(ctx, types.Delegate{ID: uuid.New(), Name: "portal-seq", IdPClientID: "portal-seq-client", Group: "portal-users"},
		newBearer(delegateCredentialPrefix))
	if err != nil {
		t.Fatal(err)
	}
	delegated := newBearer(delegatedTokenPrefix)
	if _, err := e.st.MintDelegatedToken(ctx, types.DelegatedToken{
		ID: uuid.New(), DelegateID: d.ID, Principal: "pairwise-seq", UserType: types.UserTypeStandard,
		Groups: []string{"portal-users"}, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(delegatedTokenTTL), RevSeq: tok.RevSeq,
	}, delegated, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	if w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/me/tokens", sess, ""); w.Code != http.StatusOK {
		t.Fatalf("the session before the revoke: %d", w.Code)
	}
	if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me", delegated, ""); w.Code != http.StatusOK {
		t.Fatalf("the delegated token before the revoke: %d", w.Code)
	}
	time.Sleep(30 * time.Millisecond)
	if _, err := e.pool.Exec(ctx, `INSERT INTO oidc_session_revocations (sub, revoked_at) VALUES ($1, clock_timestamp() - $2::interval)`,
		"pairwise-seq", stepBack.String()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM oidc_session_revocations WHERE sub = 'pairwise-seq'`)
	})
	var cutAt time.Time
	if err := e.pool.QueryRow(ctx, `SELECT revoked_at FROM oidc_session_revocations WHERE sub = 'pairwise-seq'`).Scan(&cutAt); err != nil {
		t.Fatal(err)
	}
	if !tok.CreatedAt.After(cutAt) {
		t.Fatalf("premise: the token's created_at %v does not read after the cut's revoked_at %v", tok.CreatedAt, cutAt)
	}

	if w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/me/tokens", sess, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("the session cookie after the revoke = %d, want 401", w.Code)
	}
	if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me", minted.Token, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("the API token after the revoke = %d, want 401", w.Code)
	}
	if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me", delegated, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("the delegated token after the revoke = %d, want 401", w.Code)
	}
	if status, err := e.h.srv.approverSessionStatus(ctx, e.pool, tok); err != nil || status != oidc.SessionRevoked {
		t.Errorf("the approver re-check of the token after the revoke = %v, %v; want revoked", status, err)
	}
	if reason := e.h.srv.sshKeyRevocationRefusal(ctx, keys[0]); reason == "" {
		t.Error("the SSH key registered before the revoke is still accepted")
	}
	if _, verdict, err := e.h.srv.redeemAttachTicket(ctx, ticket, run); err != nil || verdict != verdictRevoked {
		t.Errorf("the attach ticket minted before the revoke: verdict %v, %v; want %v", verdict, err, verdictRevoked)
	}
}

// A request admitted by one person's credential does not lend its sequence to a credential for anyone else:
// mintIssue reads the current sequence for a principal the admitting credential was not issued to, and
// inherits it for the one it was.
func TestMintIssueInheritsOnlyTheAdmittingCredentialsOwnSequence(t *testing.T) {
	srv := &Server{cfg: Config{SessionRevocations: &seqRevocations{seq: 90}}}
	ctx := oidc.WithRevocationSeq(context.Background(), "alice", 12)
	at := time.Now().UTC()
	if got, err := srv.mintIssue(ctx, "alice", at); err != nil || got.Seq != 12 || !got.At.Equal(at) {
		t.Errorf("own principal: %+v, %v; want the inherited 12", got, err)
	}
	if got, err := srv.mintIssue(ctx, "bob", at); err != nil || got.Seq != 90 {
		t.Errorf("another principal: %+v, %v; want the current sequence 90", got, err)
	}
	if got, err := srv.mintIssue(context.Background(), "alice", at); err != nil || got.Seq != 90 {
		t.Errorf("a legacy admitting credential: %+v, %v; want the current sequence 90", got, err)
	}
	srv.cfg.SessionRevocations = nil
	if got, err := srv.mintIssue(ctx, "alice", at); err != nil || got.Seq != 0 {
		t.Errorf("no revocation store: %+v, %v; want time only", got, err)
	}
	if got := admittedIssue(ctx, "bob", at); got.Seq != 0 {
		t.Errorf("admittedIssue for another principal = %+v, want time only", got)
	}
}

type seqRevocations struct {
	fakeSessionRevocations
	seq int64
}

func (s *seqRevocations) RevocationSeq(context.Context) (int64, error) { return s.seq, nil }
func (s *seqRevocations) IsSessionRevokedSeq(context.Context, string, string, int64) (bool, error) {
	return false, nil
}
