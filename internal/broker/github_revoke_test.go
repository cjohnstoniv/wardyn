// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/testutil"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// B11a-F1. mint() talks to GitHub BEFORE the single-use burn, so every arm that
// returns an error AFTER mintKind succeeded is holding a real, live ghs_… with
// contents:write for GitHub's full ~1h — and with no committed credential.mint
// row, that token has no jti, so mintedCredentialsSQL cannot see it and
// RevokeRun cannot reach it. These tests pin that every such door hands the
// token back.
//
// The commit-failure door is the one this fake DB can drive deterministically
// (fakeDB.commitErr); the lost single-use race is pinned against a REAL
// Postgres in TestPG_ConcurrentMint_ExactlyOnceWins, where the losers genuinely
// mint.

// TestMint_CommitFailure_RevokesDiscardedGitHubToken: the tx fails to commit
// after a successful mint. The caller gets the commit error and the token is
// surrendered rather than left live.
func TestMint_CommitFailure_RevokesDiscardedGitHubToken(t *testing.T) {
	b, db, _, gh := newTestBroker(t)
	db.commitErr = errors.New("pgx: connection reset by peer")
	runID := uuid.New()
	spec := githubGrantSpec(t, true)
	gid := seedGrant(db, runID, spec)
	seedApproval(db, runID, gid, spec.Scope, types.ApprovalApproved)

	if _, err := b.MintForGrant(context.Background(), callerFor(runID), gid); err == nil {
		t.Fatal("mint must fail when the tx cannot commit")
	}
	if gh.Calls != 1 {
		t.Fatalf("github minter calls = %d, want 1", gh.Calls)
	}
	if gh.Revoked != 1 {
		t.Fatalf("revoked = %d, want 1 — a minted-then-discarded token is live for ~1h with no jti behind it", gh.Revoked)
	}
	if len(gh.RevokedTokens) != 1 || gh.RevokedTokens[0] != "ghs_fixed" {
		t.Fatalf("revoked tokens = %v, want [ghs_fixed] (the token that was actually minted)", gh.RevokedTokens)
	}
}

// NEGATIVE CONTROL for B11a-F1: the WINNER's token is never revoked. A revoke
// on the success path would hand back the very credential the run is about to
// use.
func TestMint_Success_NeverRevokes(t *testing.T) {
	b, db, _, gh := newTestBroker(t)
	runID := uuid.New()
	spec := githubGrantSpec(t, true)
	gid := seedGrant(db, runID, spec)
	seedApproval(db, runID, gid, spec.Scope, types.ApprovalApproved)

	minted, err := b.MintForGrant(context.Background(), callerFor(runID), gid)
	if err != nil {
		t.Fatalf("MintForGrant: %v", err)
	}
	if minted.Token != "ghs_fixed" {
		t.Fatalf("token = %q, want ghs_fixed", minted.Token)
	}
	if gh.Revoked != 0 {
		t.Fatalf("revoked = %d on the success path, want 0 — the returned token must stay live", gh.Revoked)
	}
}

// A failing Revoke must not change the error the caller already gets: the
// hand-back is best effort, never a new failure mode.
func TestMint_CommitFailure_RevokeErrorDoesNotMaskCallerError(t *testing.T) {
	b, db, _, gh := newTestBroker(t)
	db.commitErr = errors.New("pgx: connection reset by peer")
	gh.RevokeErr = errors.New("github: 500")
	runID := uuid.New()
	spec := githubGrantSpec(t, true)
	gid := seedGrant(db, runID, spec)
	seedApproval(db, runID, gid, spec.Scope, types.ApprovalApproved)

	_, err := b.MintForGrant(context.Background(), callerFor(runID), gid)
	if err == nil || !strings.Contains(err.Error(), "connection reset by peer") {
		t.Fatalf("caller error = %v, want the commit failure (a failed revoke must not replace it)", err)
	}
	if gh.Revoked != 1 {
		t.Fatalf("revoked = %d, want 1 (attempted even though it fails)", gh.Revoked)
	}
}

// A discarded NON-github credential has no issuer-side revoke: an api_key mint
// never leaves the broker and materializes nothing GitHub could hand back, so
// the discard door must not call the GitHub minter at all.
func TestMint_CommitFailure_NonGitHubKind_NoRevokeCall(t *testing.T) {
	b, db, _, gh := newTestBroker(t)
	db.commitErr = errors.New("pgx: connection reset by peer")
	runID := uuid.New()
	scope, _ := json.Marshal(apiKeyScope{
		Host:       "api.example.com",
		Header:     "Authorization",
		Format:     "Bearer %s",
		SecretName: "example-key",
	})
	spec := types.GrantSpec{Kind: types.GrantAPIKey, Scope: scope, RequiresApproval: true, TTLSeconds: 600}
	gid := seedGrant(db, runID, spec)
	seedApproval(db, runID, gid, spec.Scope, types.ApprovalApproved)

	if _, err := b.MintForGrant(context.Background(), callerFor(runID), gid); err == nil {
		t.Fatal("mint must fail when the tx cannot commit")
	}
	if gh.Revoked != 0 {
		t.Fatalf("revoked = %d for an api_key discard, want 0", gh.Revoked)
	}
}

// newTestGitHubMinter builds a real githubMinter wired to srv with a short HTTP
// budget, so the deadline tests below finish in test time rather than the
// production githubClientTimeout.
func newTestGitHubMinter(t *testing.T, baseURL string, budget time.Duration) *githubMinter {
	t.Helper()
	ctx := context.Background()
	store := newMemSecrets()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen rsa: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	_ = store.Put(ctx, "github-app-id", []byte("12345"))
	_ = store.Put(ctx, "github-app-key", pemBytes)
	gm, err := NewGitHubMinter(store, GitHubMinterConfig{AppIDSecret: "github-app-id", PrivateKeySecret: "github-app-key"})
	if err != nil {
		t.Fatalf("NewGitHubMinter: %v", err)
	}
	m := gm.(*githubMinter)
	m.baseURL = baseURL
	m.httpTimeout = budget
	return m
}

// B11a-F2. A never-responding api.github.com must not pin the mint (and with it
// the grant row's FOR UPDATE lock and a pooled connection) for longer than the
// client's own budget. The caller here passes context.Background() ON PURPOSE:
// the residual the finding names is exactly a caller with no deadline of its
// own, so if this returns it is the http.Client Timeout that returned it.
func TestGitHubMinter_MintReturnsInsideClientBudget(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-block // never respond
	}))
	defer srv.Close()
	defer close(block)

	m := newTestGitHubMinter(t, srv.URL+"/", 300*time.Millisecond)

	done := make(chan error, 1)
	go func() {
		_, _, err := m.MintInstallationToken(context.Background(), []string{"acme/widgets"}, map[string]string{"contents": "read"}, time.Hour)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("mint against a never-responding GitHub must fail, not succeed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("mint against a never-responding GitHub never returned — no HTTP deadline on the client")
	}
}

// The client Timeout bounds one round trip, and a cold installByOrg makes two
// (GetRepositoryInstallation, then CreateInstallationToken) — so per-hop
// timeouts alone would let a slow first hop followed by a blackholed second
// hold the grant row's FOR UPDATE lock and a pooled connection for ~2x the
// budget. The mint derives one ctx deadline for the whole call, so the
// in-transaction ceiling is 1x.
//
// The server here is the shape that separates the two: hop 1 answers just under
// the budget, hop 2 never answers. Per-hop-only = ~1.8x; one ceiling = ~1x.
//
// A raw hop-hit count cannot discriminate the two shapes — hop 2's HTTP
// request reaches this fake either way (hop1Hits/hop2Hits are 1/1 under both,
// since hop 1 succeeding at all means some of its own per-hop budget is still
// left for hop 2 to dial). The counters stay as an anti-vacuity floor —
// proving the fixture actually exercised both hops, so the timing assertion
// below is proving something — and the ceiling sits at 1.5x, comfortably
// between the two measured shapes (~500ms with one deadline vs ~900ms
// per-hop) with headroom on both sides.
func TestGitHubMinter_MintCeilingIsOneBudgetNotTwo(t *testing.T) {
	const budget = 500 * time.Millisecond

	var hop1Hits, hop2Hits atomic.Int32
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			hop1Hits.Add(1)
			time.Sleep(budget * 4 / 5) // slow, but inside the budget: hop 1 SUCCEEDS
			_, _ = w.Write([]byte(`{"id": 42}`))
		default:
			hop2Hits.Add(1)
			<-block // hop 2 never answers
		}
	}))
	// Defers run LIFO, so close(block) runs BEFORE srv.Close(): httptest's Close
	// waits for outstanding handlers, and the blocked one only returns when the
	// channel closes. The other order deadlocks until the package test timeout.
	defer srv.Close()
	defer close(block)

	m := newTestGitHubMinter(t, srv.URL+"/", budget)

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		_, _, err := m.MintInstallationToken(context.Background(), []string{"acme/widgets"}, map[string]string{"contents": "read"}, time.Hour)
		if err == nil {
			t.Error("mint whose second hop never answers must fail")
		}
		done <- time.Since(start)
	}()

	select {
	case elapsed := <-done:
		// Anti-vacuity: both hops must have actually been dialed, or the
		// timing assertion below is proving nothing.
		if got := hop1Hits.Load(); got != 1 {
			t.Fatalf("hop1 (GetRepositoryInstallation) hits = %d, want 1", got)
		}
		if got := hop2Hits.Load(); got != 1 {
			t.Fatalf("hop2 (CreateInstallationToken) hits = %d, want 1", got)
		}
		// 1.5x sits comfortably below the ~1.8x per-hop-only shape and
		// comfortably above the ~1.0x one-ceiling shape.
		if ceiling := budget * 3 / 2; elapsed > ceiling {
			t.Fatalf("mint took %s, want under %s — the whole mint must share ONE ceiling, not one per round trip (two hops = 2x the lock hold)", elapsed, ceiling)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("mint never returned")
	}
}

// NEGATIVE CONTROL for B11a-F2: a merely SLOW GitHub still mints. The deadline
// is a ceiling on a hung server, not a latency budget that fails ordinary
// round trips.
func TestGitHubMinter_SlowGitHubStillMints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Second)
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			_, _ = w.Write([]byte(`{"id": 42}`))
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_slow_ok","expires_at":"` + testutil.FutureRFC3339(24) + `"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	m := newTestGitHubMinter(t, srv.URL+"/", 5*time.Second)
	tok, _, err := m.MintInstallationToken(context.Background(), []string{"acme/widgets"}, map[string]string{"contents": "read"}, time.Hour)
	if err != nil {
		t.Fatalf("a 1s-delayed mint must succeed: %v", err)
	}
	if tok != "ghs_slow_ok" {
		t.Fatalf("token = %q, want ghs_slow_ok", tok)
	}
}

// The production Revoke really issues DELETE /installation/token authenticated
// As the discarded token — the same call ruleset.go makes for its probe token.
func TestGitHubMinter_Revoke_DeletesInstallationToken(t *testing.T) {
	var gotMethod, gotPath, gotAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod.Store(r.Method)
		gotPath.Store(r.URL.Path)
		gotAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	m := newTestGitHubMinter(t, srv.URL+"/", 5*time.Second)
	if err := m.Revoke(context.Background(), "ghs_discarded"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if gotMethod.Load() != http.MethodDelete {
		t.Fatalf("method = %v, want DELETE", gotMethod.Load())
	}
	if p, _ := gotPath.Load().(string); !strings.HasSuffix(p, "/installation/token") {
		t.Fatalf("path = %q, want .../installation/token", p)
	}
	if a, _ := gotAuth.Load().(string); a != "Bearer ghs_discarded" {
		t.Fatalf("authorization = %q, want the discarded token itself", a)
	}
}

// An already-cancelled caller ctx is the arm that MATTERS (a timed-out mint is
// exactly when a live token would otherwise linger), so WithoutCancel must keep
// the hand-back working; and an empty token is a no-op, never a request.
func TestGitHubMinter_Revoke_SurvivesCancelledCallerCtx(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	m := newTestGitHubMinter(t, srv.URL+"/", 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Revoke(ctx, "ghs_discarded"); err != nil {
		t.Fatalf("Revoke with a cancelled caller ctx: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("revoke requests = %d, want 1 (WithoutCancel)", got)
	}
	if err := m.Revoke(context.Background(), ""); err != nil {
		t.Fatalf("Revoke(\"\"): %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("revoke requests = %d after an empty token, want still 1 (no-op)", got)
	}
}
