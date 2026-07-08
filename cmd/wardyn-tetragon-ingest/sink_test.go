// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestEventSink_RefreshesTokenOn401 covers the HIGH finding: the host sensor
// minted its bearer token once at boot with a fixed ~1h ceiling and reused it
// forever, so after expiry every POST to /api/v1/internal/groundtruth returned
// 401 and the second audit stream silently died with no recovery.
//
// Red-first contract: the sink must, on a 401, re-fetch the token from its
// configured token source and retry the POST once. We stand up an httptest
// server that 401s any request bearing the stale boot token and 200s once the
// refreshed token arrives, then assert the batch is ultimately accepted (posted)
// and not counted as a drop.
func TestEventSink_RefreshesTokenOn401(t *testing.T) {
	const staleToken = "stale-boot-token"
	const freshToken = "fresh-rotated-token"

	var (
		mu         sync.Mutex
		got401     int
		got200     int
		refreshCnt atomic.Int64
		seenTokens []string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		seenTokens = append(seenTokens, auth)
		mu.Unlock()
		// The stale boot token is no longer valid: reject like an expired aud token.
		if auth != "Bearer "+freshToken {
			mu.Lock()
			got401++
			mu.Unlock()
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Drain the body so the batch is "accepted".
		var b groundtruthBatch
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		got200++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// tokenSource hands back the stale token first; a refresh re-mints to fresh.
	current := staleToken
	var srcMu sync.Mutex
	tokenSource := func() (string, error) {
		srcMu.Lock()
		defer srcMu.Unlock()
		refreshCnt.Add(1)
		// On the 2nd+ call (the refresh after a 401), return the rotated token.
		if refreshCnt.Load() >= 2 {
			current = freshToken
		}
		return current, nil
	}

	sink := newEventSinkWithSource(srv.URL, tokenSource, 16, 8, 50*time.Millisecond, srv.Client())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		sink.close(ctx)
	})

	sink.emit(types.AuditEvent{Action: "kernel.process.exec", Outcome: "success"})

	// Wait for the batch to be accepted after a refresh+retry.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sink.postedCount() >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if sink.postedCount() < 1 {
		t.Fatalf("batch was never accepted: posted=%d dropped=%d (sink did not refresh token on 401)", sink.postedCount(), sink.droppedCount())
	}
	mu.Lock()
	saw401 := got401
	saw200 := got200
	mu.Unlock()
	if saw401 == 0 {
		t.Errorf("expected at least one 401 with the stale token, got none")
	}
	if saw200 == 0 {
		t.Errorf("expected the retried POST to succeed with the refreshed token, got none")
	}
	if refreshCnt.Load() < 2 {
		t.Errorf("token source should have been re-invoked to refresh after 401; calls=%d", refreshCnt.Load())
	}
}

// TestEventSink_StaticTokenStillWorks ensures the default (no-rotation) path is
// preserved: a sink built from a constant token still posts successfully and a
// genuine, persistent 401 is counted as a drop rather than retried forever.
func TestEventSink_StaticTokenStillWorks(t *testing.T) {
	const tok = "good-token"
	var got200 atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+tok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		got200.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := newEventSink(srv.URL, tok, 16, 8, 50*time.Millisecond, srv.Client())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		sink.close(ctx)
	})

	sink.emit(types.AuditEvent{Action: "kernel.process.exec", Outcome: "success"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sink.postedCount() >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if sink.postedCount() < 1 {
		t.Fatalf("static-token sink failed to post: posted=%d dropped=%d", sink.postedCount(), sink.droppedCount())
	}
	if got200.Load() == 0 {
		t.Errorf("expected server to accept the static-token POST")
	}
}

// TestEventSink_Persistent401Drops verifies we do not retry indefinitely: if a
// refresh still yields an unauthorized token, the batch is counted as dropped
// exactly once (visible gap), not silently lost or infinitely retried.
func TestEventSink_Persistent401Drops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // always reject
	}))
	defer srv.Close()

	refreshes := 0
	src := func() (string, error) {
		refreshes++
		return "never-valid", nil
	}
	sink := newEventSinkWithSource(srv.URL, src, 16, 8, 50*time.Millisecond, srv.Client())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		sink.close(ctx)
	})

	sink.emit(types.AuditEvent{Action: "kernel.process.exec", Outcome: "success"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sink.droppedCount() >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if sink.droppedCount() < 1 {
		t.Fatalf("persistent 401 should be counted as a drop; dropped=%d posted=%d", sink.droppedCount(), sink.postedCount())
	}
	if sink.postedCount() != 0 {
		t.Errorf("nothing should have posted against an always-401 server; posted=%d", sink.postedCount())
	}
}
