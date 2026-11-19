// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// adminGet is GET /api/v1/policies/default with bearer, from the TCP peer remote: a read the harness, which
// has no store, answers 200.
func adminGet(t *testing.T, srv *Server, remote, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/policies/default", nil)
	r.Host = "127.0.0.1"
	r.RemoteAddr = remote
	r.Header.Set("Authorization", "Bearer "+bearer)
	w := httptest.NewRecorder()
	panicFails(t, srv.Handler()).ServeHTTP(deadlineRecorder{w}, r)
	return w
}

// A peer that guesses the admin bearer wrong is held to enrolBurst guesses and then one every ten seconds:
// the next is refused 429 without being compared, so a right guess among them is refused too until its bucket
// refills; another address is unaffected, and a request that presents the right token never spends the bucket.
func TestAdminAuth_FailedGuessesAreRateLimitedPerPeer(t *testing.T) {
	h := newHarness(t)
	var offset atomic.Int64
	h.srv.cfg.Now = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	const guesser, other = "198.51.100.7:4000", "198.51.100.8:4000"

	for i := range int(enrolBurst) {
		if w := adminGet(t, h.srv, guesser, "wrong-token"); w.Code != http.StatusUnauthorized || errorReason(w) != reasonInvalidAdminToken {
			t.Fatalf("guess %d: %d %s, want 401 %s", i+1, w.Code, w.Body, reasonInvalidAdminToken)
		}
	}
	w := adminGet(t, h.srv, guesser, "wrong-token")
	if w.Code != http.StatusTooManyRequests || errorReason(w) != reasonAdminTokenRateLimited || w.Header().Get("Retry-After") != "10" {
		t.Fatalf("guess past the burst: %d %s Retry-After=%q, want 429 %s with Retry-After 10", w.Code, w.Body, w.Header().Get("Retry-After"), reasonAdminTokenRateLimited)
	}
	if w := adminGet(t, h.srv, guesser, adminToken); w.Code != http.StatusTooManyRequests {
		t.Fatalf("the right token from a peer out of guesses: %d, want 429 (the guess is not compared)", w.Code)
	}
	if w := adminGet(t, h.srv, other, adminToken); w.Code != http.StatusOK {
		t.Fatalf("another address with the right token: %d %s, want 200", w.Code, w.Body)
	}
	if w := adminGet(t, h.srv, other, "wrong-token"); w.Code != http.StatusUnauthorized {
		t.Fatalf("another address's first wrong guess: %d, want 401", w.Code)
	}

	offset.Store(int64(11 * time.Second)) // one token refilled for the guesser
	if w := adminGet(t, h.srv, guesser, adminToken); w.Code != http.StatusOK {
		t.Fatalf("the right token once a guess has refilled: %d %s, want 200", w.Code, w.Body)
	}
	// That success gave its token back: a run of right tokens never empties the bucket.
	for i := range 3 * int(enrolBurst) {
		if w := adminGet(t, h.srv, guesser, adminToken); w.Code != http.StatusOK {
			t.Fatalf("right token %d after the refill: %d, want 200", i+1, w.Code)
		}
	}
}

// A request with no bearer is not a guess and spends nothing.
func TestAdminAuth_MissingBearerIsNotCounted(t *testing.T) {
	h := newHarness(t)
	for range 3 * int(enrolBurst) {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
		r.Host, r.RemoteAddr = "127.0.0.1", "198.51.100.9:4000"
		w := httptest.NewRecorder()
		panicFails(t, h.srv.Handler()).ServeHTTP(deadlineRecorder{w}, r)
		if w.Code != http.StatusUnauthorized || errorReason(w) != reasonMissingBearerToken {
			t.Fatalf("no bearer: %d %s, want 401 %s", w.Code, w.Body, reasonMissingBearerToken)
		}
	}
	if w := adminGet(t, h.srv, "198.51.100.9:4000", "wrong-token"); w.Code != http.StatusUnauthorized {
		t.Fatalf("first wrong guess after many bare requests: %d, want 401", w.Code)
	}
}
