// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// sealDirect seals token as jti for the fixture's run, expiring at expiresAt, without a mint.
func (f *sealFixture) sealDirect(t *testing.T, jti, token string, expiresAt time.Time) {
	t.Helper()
	p, err := f.seal.prepare(t.Context(), f.runID, jti, token, expiresAt)
	if err != nil || p == nil {
		t.Fatalf("prepare = %v, %v", p, err)
	}
	tx, err := NewPgxStore(f.pool).BeginReadCommitted(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.seal.sealTx(t.Context(), tx, f.runID, jti, expiresAt, p); err != nil || !ok {
		_ = tx.Rollback(t.Context())
		t.Fatalf("sealTx = %v, %v", ok, err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// ghServer is a GitHub double for DELETE /installation/token: answer decides each call's status from its token, and
// the double records the tokens it was presented and the most calls it saw at once.
type ghServer struct {
	srv      *httptest.Server
	mu       sync.Mutex
	tokens   []string
	inFlight atomic.Int32
	maxSeen  atomic.Int32
}

func newGHServer(t *testing.T, answer func(token string, n int) int) *ghServer {
	t.Helper()
	g := &ghServer{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || !strings.HasSuffix(r.URL.Path, "/installation/token") {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		cur := g.inFlight.Add(1)
		defer g.inFlight.Add(-1)
		for {
			m := g.maxSeen.Load()
			if cur <= m || g.maxSeen.CompareAndSwap(m, cur) {
				break
			}
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		g.mu.Lock()
		g.tokens = append(g.tokens, token)
		n := 0
		for _, tk := range g.tokens {
			if tk == token {
				n++
			}
		}
		g.mu.Unlock()
		status := answer(token, n)
		if status == 0 {
			<-r.Context().Done() // hang until the client gives up
			return
		}
		w.WriteHeader(status)
		if status >= 300 {
			_, _ = w.Write([]byte(`{"message":"test"}`))
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *ghServer) calls() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.tokens...)
}

// workerBroker is a broker over the fixture's seal whose GitHub hand-backs go to g.
func (f *sealFixture) workerBroker(t *testing.T, g *ghServer) *Broker {
	t.Helper()
	m := newTestGitHubMinter(t, g.srv.URL+"/", 5*time.Second)
	return New(NewPgxStore(f.pool), nil, poolAudit{f.pool}, nil, m).WithTokenSeal(f.seal)
}

type sealRow struct {
	outcome   string
	attempts  int
	sealedLen int
	due       bool
	next      *time.Time
}

func (f *sealFixture) row(t *testing.T, jti string) sealRow {
	t.Helper()
	var r sealRow
	var outcome *string
	if err := f.pool.QueryRow(t.Context(), `SELECT ended_outcome, attempts, length(sealed), revoke_due_at IS NOT NULL, next_attempt_at
		FROM run_github_token_seals WHERE run_id = $1 AND jti = $2`, f.runID, jti).Scan(&outcome, &r.attempts, &r.sealedLen, &r.due, &r.next); err != nil {
		t.Fatalf("read seal row %s: %v", jti, err)
	}
	if outcome != nil {
		r.outcome = *outcome
	}
	return r
}

// The worker presents the queued token to DELETE /installation/token, ends the row handed_back with its copy
// emptied, and audits success; the token is in no audit row and no log line.
func TestPG_GitHubRevokeWorker_RevokesSealedTokenAtTarget(t *testing.T) {
	logs := captureLog(t)
	f := newSealFixture(t, true)
	g := newGHServer(t, func(string, int) int { return http.StatusNoContent })
	const token = "ghs_worker_target_token"
	f.sealDirect(t, "jti-a", token, time.Now().Add(time.Hour))
	if err := f.seal.queue(t.Context(), f.runID, nil); err != nil {
		t.Fatal(err)
	}
	f.workerBroker(t, g).githubRevokePass(t.Context())
	if c := g.calls(); len(c) != 1 || c[0] != token {
		t.Fatalf("GitHub was presented %v, want the sealed token once", c)
	}
	if r := f.row(t, "jti-a"); r.outcome != sealHandedBack || r.sealedLen != 0 || r.due {
		t.Fatalf("row = %+v, want handed_back, emptied, not due", r)
	}
	rows := auditRows(t, f.pool, f.runID, "github_token.revoke")
	if len(rows) != 1 || rows[0]["_outcome"] != "success" || rows[0]["target_outcome"] != targetRevoked || rows[0]["jti"] != "jti-a" ||
		rows[0]["kind"] != "github_token" || rows[0]["attempt"] != float64(1) {
		t.Fatalf("github_token.revoke rows = %v", rows)
	}
	assertNoTokenInAudit(t, f.pool, f.runID, token)
	if strings.Contains(logs.String(), token) {
		t.Fatal("a log line carries the token value")
	}
}

// A 502 then a 204 gives one failure row with retry_at about 30 s on, then a success.
func TestPG_GitHubRevokeWorker_RetriesAFailedDeleteWithBackoff(t *testing.T) {
	f := newSealFixture(t, true)
	g := newGHServer(t, func(_ string, n int) int {
		if n == 1 {
			return http.StatusBadGateway
		}
		return http.StatusNoContent
	})
	f.sealDirect(t, "jti-r", "ghs_retry", time.Now().Add(time.Hour))
	if err := f.seal.queue(t.Context(), f.runID, nil); err != nil {
		t.Fatal(err)
	}
	b := f.workerBroker(t, g)
	before := time.Now()
	b.githubRevokePass(t.Context())
	r := f.row(t, "jti-r")
	if r.outcome != "" || r.attempts != 1 || r.next == nil {
		t.Fatalf("row after the 502 = %+v, want held with attempts 1 and a next try", r)
	}
	if d := r.next.Sub(before); d < 25*time.Second || d > 40*time.Second {
		t.Fatalf("next try %s after the pass, want about 30s", d)
	}
	rows := auditRows(t, f.pool, f.runID, "github_token.revoke")
	if len(rows) != 1 || rows[0]["_outcome"] != "failure" || rows[0]["target_outcome"] != targetFailed || rows[0]["retry_at"] == nil {
		t.Fatalf("github_token.revoke rows = %v, want one failure with retry_at", rows)
	}
	b.githubRevokePass(t.Context())
	if c := g.calls(); len(c) != 1 {
		t.Fatalf("calls before the retry is due = %d, want 1", len(c))
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE run_github_token_seals SET next_attempt_at = now() WHERE run_id = $1`, f.runID); err != nil {
		t.Fatal(err)
	}
	b.githubRevokePass(t.Context())
	if r := f.row(t, "jti-r"); r.outcome != sealHandedBack {
		t.Fatalf("row after the 204 = %+v, want handed_back", r)
	}
	rows = auditRows(t, f.pool, f.runID, "github_token.revoke")
	if len(rows) != 2 || rows[1]["_outcome"] != "success" || rows[1]["attempt"] != float64(2) {
		t.Fatalf("github_token.revoke rows = %v, want a second, successful attempt", rows)
	}
}

// A retry that would fall past the token's expiry is abandoned and audited.
func TestPG_GitHubRevokeWorker_AbandonsAtExpiry(t *testing.T) {
	f := newSealFixture(t, true)
	g := newGHServer(t, func(string, int) int { return http.StatusBadGateway })
	f.sealDirect(t, "jti-x", "ghs_abandon", time.Now().Add(20*time.Second))
	if err := f.seal.queue(t.Context(), f.runID, nil); err != nil {
		t.Fatal(err)
	}
	f.workerBroker(t, g).githubRevokePass(t.Context())
	if r := f.row(t, "jti-x"); r.outcome != sealAbandoned || r.sealedLen != 0 {
		t.Fatalf("row = %+v, want abandoned and emptied", r)
	}
	rows := auditRows(t, f.pool, f.runID, "github_token.revoke")
	if len(rows) != 1 || rows[0]["_outcome"] != "failure" || rows[0]["abandoned"] != true || rows[0]["retry_at"] != nil {
		t.Fatalf("github_token.revoke rows = %v, want one abandoned failure", rows)
	}
}

// An expired row is never called, whether it expired before RevokeRun or before its pass.
func TestPG_GitHubRevoke_ExpiredRowIsNeverCalled(t *testing.T) {
	f := newSealFixture(t, true)
	g := newGHServer(t, func(string, int) int { return http.StatusNoContent })
	got, err := f.b.MintForGrant(t.Context(), callerFor(f.runID), f.grantID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE run_github_token_seals SET expires_at = now() - interval '1 minute' WHERE run_id = $1`, f.runID); err != nil {
		t.Fatal(err)
	}
	if err := f.b.RevokeRun(t.Context(), f.runID); err != nil {
		t.Fatal(err)
	}
	if rows := auditRows(t, f.pool, f.runID, "credential.revoke"); len(rows) != 1 || rows[0]["target_revoke"] != sealExpired {
		t.Fatalf("credential.revoke rows = %v, want target_revoke expired", rows)
	}
	if r := f.row(t, got.JTI); r.outcome != sealExpired || r.sealedLen != 0 {
		t.Fatalf("row = %+v, want ended expired", r)
	}

	f.sealDirect(t, "jti-late", "ghs_expires_in_queue", time.Now().Add(time.Hour))
	if err := f.seal.queue(t.Context(), f.runID, []string{"jti-late"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE run_github_token_seals SET expires_at = now() - interval '1 second' WHERE run_id = $1 AND jti = 'jti-late'`, f.runID); err != nil {
		t.Fatal(err)
	}
	f.workerBroker(t, g).githubRevokePass(t.Context())
	if r := f.row(t, "jti-late"); r.outcome != sealExpired {
		t.Fatalf("row = %+v, want ended expired", r)
	}
	rows := auditRows(t, f.pool, f.runID, "github_token.revoke")
	if len(rows) != 1 || rows[0]["_outcome"] != "success" || rows[0]["target_outcome"] != targetExpired {
		t.Fatalf("github_token.revoke rows = %v, want one expired success", rows)
	}
	if c := g.calls(); len(c) != 0 {
		t.Fatalf("GitHub calls = %d, want 0", len(c))
	}
}

// A pass of 16 hanging calls is bounded with at most 4 in flight; a cancel starts nothing new and writes the row of
// each call in flight.
func TestPG_GitHubRevokeWorker_PassBoundAndCancel(t *testing.T) {
	f := newSealFixture(t, true)
	g := newGHServer(t, func(string, int) int { return 0 })
	for range githubRevokePage {
		f.sealDirect(t, "jti-h"+uuid.NewString(), "ghs_hang_"+uuid.NewString(), time.Now().Add(time.Hour))
	}
	if err := f.seal.queue(t.Context(), f.runID, nil); err != nil {
		t.Fatal(err)
	}
	b := f.workerBroker(t, g)
	start := time.Now()
	b.githubRevokePass(t.Context())
	if d := time.Since(start); d > githubRevokePassBound+refRevokeTimeout+time.Second {
		t.Fatalf("one pass took %s, want within %s", d, githubRevokePassBound+refRevokeTimeout)
	}
	if m := g.maxSeen.Load(); m > githubRevokeInFlight {
		t.Fatalf("calls in flight at once = %d, want at most %d", m, githubRevokeInFlight)
	}

	if _, err := f.pool.Exec(t.Context(), `UPDATE run_github_token_seals SET next_attempt_at = now() WHERE run_id = $1 AND ended_at IS NULL`, f.runID); err != nil {
		t.Fatal(err)
	}
	before := len(g.calls())
	failuresBefore := len(auditRows(t, f.pool, f.runID, "github_token.revoke"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.RunGitHubTokenRevokes(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for g.inFlight.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	inFlight := int(g.inFlight.Load())
	cancel()
	stopped := time.Now()
	select {
	case <-done:
	case <-time.After(refRevokeTimeout + 2*time.Second):
		t.Fatal("the worker did not return after its cancel")
	}
	if d := time.Since(stopped); d > refRevokeTimeout+time.Second {
		t.Fatalf("the worker returned %s after its cancel, want within the call in flight", d)
	}
	started := len(g.calls()) - before
	if started == 0 || started > githubRevokeInFlight || started < inFlight {
		t.Fatalf("calls started by the cancelled pass = %d (in flight at cancel %d), want 1..%d", started, inFlight, githubRevokeInFlight)
	}
	if n := len(auditRows(t, f.pool, f.runID, "github_token.revoke")) - failuresBefore; n != started {
		t.Fatalf("rows written for the cancelled pass = %d, want one per call made (%d)", n, started)
	}
}

// 401 is already_invalid (success, handed back); 404 is unknown (failure, retried); a nil minter calls nothing.
func TestPG_GitHubRevokeWorker_ClassifiesAnswers(t *testing.T) {
	f := newSealFixture(t, true)
	g := newGHServer(t, func(token string, _ int) int {
		if strings.HasSuffix(token, "_401") {
			return http.StatusUnauthorized
		}
		return http.StatusNotFound
	})
	f.sealDirect(t, "jti-401", "ghs_dead_401", time.Now().Add(time.Hour))
	f.sealDirect(t, "jti-404", "ghs_wrong_base_404", time.Now().Add(time.Hour))
	if err := f.seal.queue(t.Context(), f.runID, nil); err != nil {
		t.Fatal(err)
	}
	f.workerBroker(t, g).githubRevokePass(t.Context())
	if r := f.row(t, "jti-401"); r.outcome != sealHandedBack {
		t.Fatalf("401 row = %+v, want handed_back", r)
	}
	if r := f.row(t, "jti-404"); r.outcome != "" || r.attempts != 1 || !r.due {
		t.Fatalf("404 row = %+v, want held for a retry", r)
	}
	byJTI := map[string]map[string]any{}
	for _, r := range auditRows(t, f.pool, f.runID, "github_token.revoke") {
		byJTI[r["jti"].(string)] = r
	}
	if r := byJTI["jti-401"]; r["_outcome"] != "success" || r["target_outcome"] != targetAlreadyInvalid {
		t.Fatalf("401 audit = %v", r)
	}
	if r := byJTI["jti-404"]; r["_outcome"] != "failure" || r["target_outcome"] != targetUnknown || r["retry_at"] == nil {
		t.Fatalf("404 audit = %v", r)
	}
}

// With no GitHub App the worker calls nothing and the row waits for its expiry.
func TestPG_GitHubRevokeWorker_NilMinterCallsNothing(t *testing.T) {
	f := newSealFixture(t, true)
	f.sealDirect(t, "jti-n", "ghs_no_app", time.Now().Add(time.Hour))
	if err := f.seal.queue(t.Context(), f.runID, nil); err != nil {
		t.Fatal(err)
	}
	b := New(NewPgxStore(f.pool), nil, poolAudit{f.pool}, nil, nil).WithTokenSeal(f.seal)
	b.githubRevokePass(t.Context())
	if r := f.row(t, "jti-n"); r.outcome != "" || r.attempts != 0 || r.sealedLen == 0 {
		t.Fatalf("row = %+v, want it held untouched", r)
	}
	if rows := auditRows(t, f.pool, f.runID, "github_token.revoke"); len(rows) != 0 {
		t.Fatalf("github_token.revoke rows = %v, want none", rows)
	}
}

// Two workers never present one row twice.
func TestPG_GitHubRevokeWorker_TwoWorkersOneCall(t *testing.T) {
	f := newSealFixture(t, true)
	g := newGHServer(t, func(string, int) int {
		time.Sleep(50 * time.Millisecond)
		return http.StatusNoContent
	})
	want := map[string]bool{}
	for range 8 {
		tok := "ghs_pair_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		want[tok] = true
		f.sealDirect(t, "jti-"+uuid.NewString(), tok, time.Now().Add(time.Hour))
	}
	if err := f.seal.queue(t.Context(), f.runID, nil); err != nil {
		t.Fatal(err)
	}
	b1 := f.workerBroker(t, g)
	b2 := New(NewPgxStore(f.pool), nil, poolAudit{f.pool}, nil, newTestGitHubMinter(t, g.srv.URL+"/", 5*time.Second)).WithTokenSeal(NewTokenSeal(f.pool, f.keys))
	var wg sync.WaitGroup
	for _, b := range []*Broker{b1, b2} {
		wg.Add(1)
		go func() { defer wg.Done(); b.githubRevokePass(t.Context()) }()
	}
	wg.Wait()
	seen := map[string]int{}
	for _, tok := range g.calls() {
		seen[tok]++
	}
	for tok := range want {
		if seen[tok] != 1 {
			t.Fatalf("token presented %d times, want exactly once (calls %d)", seen[tok], len(g.calls()))
		}
	}
}
