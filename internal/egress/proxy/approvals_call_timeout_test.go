// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A control plane that accepts the connection and never answers delays a request's raise or re-check
// by approvalCallTimeout, not by the client's 130 s forward ceiling.
func TestApprovalResolveBoundsAStalledControlPlane(t *testing.T) {
	old := approvalCallTimeout
	approvalCallTimeout = 100 * time.Millisecond
	t.Cleanup(func() { approvalCallTimeout = old })

	release := make(chan struct{})
	cp := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(cp.Close)
	t.Cleanup(func() { close(release) })

	t.Run("raise", func(t *testing.T) {
		ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), &http.Client{Timeout: 5 * time.Second})
		start := time.Now()
		res := ap.Resolve(context.Background(), "new.example.com")
		if d := time.Since(start); d > 3*time.Second {
			t.Fatalf("raise held the request for %v, want about %v", d, approvalCallTimeout)
		}
		if res.State != apPending {
			t.Fatalf("state = %v, want apPending (fail closed on a raise that did not answer)", res.State)
		}
	})
	t.Run("re-check keeps the cached grant", func(t *testing.T) {
		ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), &http.Client{Timeout: 5 * time.Second})
		id := uuid.New()
		ap.mu.Lock()
		ap.hosts["egress.test"] = &hostApproval{state: apApproved, approvalID: id, lastPoll: time.Now().Add(-2 * approvalTTL)}
		ap.mu.Unlock()
		start := time.Now()
		res := ap.Resolve(context.Background(), "egress.test")
		if d := time.Since(start); d > 3*time.Second {
			t.Fatalf("re-check held the request for %v, want about %v", d, approvalCallTimeout)
		}
		if res.State != apApproved {
			t.Fatalf("state = %v, want apApproved (a transient poll error leaves the state unchanged)", res.State)
		}
	})
}
