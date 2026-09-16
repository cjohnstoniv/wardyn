// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"
)

// TestAWSSSORefresh_AStillValidTokenNeverQueuesBehindAStalledRenewal is B2-F6.
//
// Dispatch is synchronous with POST /runs and needsRefresh fires a whole skew
// window (10 min) ahead of expiry, so the common case is several people
// launching runs against a token that still works. Against an unresponsive
// SSO-OIDC endpoint the in-flight renewal costs 2 x awsSSORefreshTimeout + the
// retry delay before it gives up and serves the token it holds — and every
// other create queued behind the owner lock used to pay that same bill again,
// serially, inside its own create request.
//
// The fake endpoint PARKS the first renewal, so the assertion is about
// concurrency and not about a wall clock: the second dispatch must come back
// with the still-valid token while the first is still inside CreateToken.
func TestAWSSSORefresh_AStillValidTokenNeverQueuesBehindAStalledRenewal(t *testing.T) {
	s, _, _ := ssoRefreshServer(t)
	// Five minutes of validity left: inside the skew window (so a renewal is
	// attempted) but NOT expired (so the token still works and this caller has
	// nothing to gain by waiting).
	blob := putAWSSSOBlob(t, s, awsSSOTestFixedNow.Add(5*time.Minute))

	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		once.Do(func() { close(started) })
		<-release
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "slow_down"})
	})

	first := make(chan struct{})
	go func() {
		defer close(first)
		_, _ = s.refreshAWSSSOBlob(context.Background(), awsSSOScope{}, blob)
	}()
	<-started // the first flight is now parked inside CreateToken

	type result struct {
		blob awsSSOBlob
		msg  string
	}
	second := make(chan result, 1)
	go func() {
		b, msg := s.refreshAWSSSOBlob(context.Background(), awsSSOScope{}, blob)
		second <- result{b, msg}
	}()

	select {
	case got := <-second:
		if got.msg != "" {
			t.Errorf("refusal = %q; nothing failed for THIS dispatch, it holds a working token", got.msg)
		}
		if got.blob.AccessToken != blob.AccessToken {
			t.Errorf("access token = %q, want the still-valid stored one", got.blob.AccessToken)
		}
	case <-time.After(3 * time.Second):
		t.Error("the second dispatch queued behind the stalled renewal: N creates pay N times the " +
			"unresponsive endpoint's budget, serially, inside POST /runs")
	}

	close(release)
	<-first
}

// TestAWSSSORefresh_AnExpiredTokenStillWaitsForTheRenewal is B2-F6's negative
// control and the reason the skip is conditional: a caller whose token has
// actually expired has everything to gain by waiting for the flight already
// under way, so it still takes the blocking lock.
func TestAWSSSORefresh_AnExpiredTokenStillWaitsForTheRenewal(t *testing.T) {
	s, _, blob := ssoRefreshServer(t) // the fixture's token expired a minute ago
	if !blob.expired(awsSSOTestFixedNow) {
		t.Fatal("fixture no longer carries an expired access token")
	}
	fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken": "fresh-access-token-abcdefghij", "expiresIn": 3600,
		})
	})
	unlock := s.lockAWSSSOOwner("")

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.refreshAWSSSOBlob(context.Background(), awsSSOScope{}, blob)
	}()
	select {
	case <-done:
		t.Fatal("an EXPIRED token skipped the in-flight renewal instead of waiting for it — " +
			"that caller has no working credential to serve")
	case <-time.After(200 * time.Millisecond):
		// Still blocked on the lock, which is what this arm must do.
	}
	unlock()
	<-done
}

// TestAWSSSORefresh_ATokenTooCloseToExpiryStillWaits is R2's pin on B2-F6's
// fast path.
//
// refreshAWSSSOBlob early-returns unless needsRefresh, so EVERY caller that
// reaches the single-flight is already inside awsSSORefreshSkew. Keying the fast
// path on "not expired" alone therefore let a token with SECONDS left take it:
// the second dispatch was served a credential that lapses mid-run, where before
// B2-F6 it queued and got the renewed pair. Renewing a whole skew window ahead
// of expiry exists precisely to keep a freshly dispatched run off that edge.
func TestAWSSSORefresh_ATokenTooCloseToExpiryStillWaits(t *testing.T) {
	s, _, _ := ssoRefreshServer(t)
	// Twenty seconds left: not expired, and nowhere near enough to start a run on.
	blob := putAWSSSOBlob(t, s, awsSSOTestFixedNow.Add(20*time.Second))

	started, release := make(chan struct{}), make(chan struct{})
	var startOnce, releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	// Registered BEFORE the parked flight is launched: a failing assertion below
	// must not leave the fake endpoint's handler blocked forever.
	t.Cleanup(releaseAll)
	fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		startOnce.Do(func() { close(started) })
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken": "fresh-access-token-abcdefghij", "expiresIn": 3600,
		})
	})

	first := make(chan struct{})
	go func() {
		defer close(first)
		_, _ = s.refreshAWSSSOBlob(context.Background(), awsSSOScope{}, blob)
	}()
	<-started // the first flight is parked inside CreateToken

	second := make(chan awsSSOBlob, 1)
	go func() {
		b, _ := s.refreshAWSSSOBlob(context.Background(), awsSSOScope{}, blob)
		second <- b
	}()
	select {
	case got := <-second:
		releaseAll()
		<-first
		t.Fatalf("the second dispatch was served the near-expiry token (%q, expiring at %s) instead of waiting for the "+
			"renewal already in flight — the run starts on a credential that lapses under it",
			got.AccessToken, got.ExpiresAt.Format(time.RFC3339))
	case <-time.After(200 * time.Millisecond):
		// Still blocked on the owner lock, which is what this arm must do.
	}

	releaseAll()
	<-first
	if got := <-second; got.AccessToken != "fresh-access-token-abcdefghij" {
		t.Errorf("access token = %q; the waiting dispatch must be served the RENEWED pair", got.AccessToken)
	}
}
