// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// holdGitUpstream is a control plane whose mint signals on minted and then either answers 409 with an approval that
// stays pending, or (hang) never answers until the request ends.
func holdGitUpstream(t *testing.T, hang bool) (*httptest.Server, chan struct{}, *atomic.Int32) {
	t.Helper()
	minted := make(chan struct{}, 8)
	var mints atomic.Int32
	approval := uuid.NewString()
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/internal/credentials/mint":
			mints.Add(1)
			minted <- struct{}{}
			if hang {
				select {
				case <-r.Context().Done():
				case <-release:
				}
				return
			}
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"approval_id":"`+approval+`"}`)
		case strings.HasPrefix(r.URL.Path, "/api/v1/internal/approvals/"):
			_, _ = io.WriteString(w, `{"id":"`+approval+`","state":"PENDING"}`)
		default:
			http.Error(w, "no forge in this test", http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // runs before srv.Close, which waits for a hung handler
	return srv, minted, &mints
}

// holdCtx is a request context with a via_org hold budget of ceiling (0: none, as a sidecar's), ended by cancel.
func holdCtx(ceiling time.Duration) (context.Context, context.CancelFunc) {
	ctx := withHoldBudget(httptest.NewRequest(http.MethodGet, "/", nil), ceiling).Context()
	return context.WithCancel(ctx)
}

type brokered struct {
	tok string
	err error
	at  time.Duration
}

// startBrokered runs brokeredToken in a goroutine and answers on the returned channel.
func startBrokered(p *Proxy, ctx context.Context, grant uuid.UUID) <-chan brokered {
	out := make(chan brokered, 1)
	start := time.Now()
	go func() {
		tok, _, err := p.brokeredToken(ctx, grant, func(string) string { return gitBrokerUsername })
		out <- brokered{tok, err, time.Since(start)}
	}()
	return out
}

func recvBrokered(t *testing.T, ch <-chan brokered, limit time.Duration, what string) brokered {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(limit):
		t.Fatalf("%s: still waiting after %s", what, limit)
		return brokered{}
	}
}

// TestGitBrokerHoldBudgetBoundsTheWaitForTheTurn: a request on a via_org engine, queued behind another's
// acquisition of the same approval-gated grant, spends its own hold budget waiting and leaves at it, not at the
// operator's approval timeout. A sidecar request (no budget) still waits the approval timeout.
func TestGitBrokerHoldBudgetBoundsTheWaitForTheTurn(t *testing.T) {
	orig := gitApprovalPollInterval
	gitApprovalPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { gitApprovalPollInterval = orig })
	const (
		ceiling = 200 * time.Millisecond
		margin  = 250 * time.Millisecond
	)
	t.Setenv(envGitApprovalTimeout, "1h")
	grant := uuid.New()
	srv, minted, mints := holdGitUpstream(t, false)
	p, _ := newGitBrokerProxy(t, map[string]uuid.UUID{"octocat/hello-world": grant}, upstreamAddr(srv))

	first, cancelFirst := holdCtx(0)
	defer cancelFirst()
	firstDone := startBrokered(p, first, grant)
	<-minted // the first caller holds the turn and is waiting on its approval

	t.Run("a hold budget ends the wait at its ceiling", func(t *testing.T) {
		ctx, cancel := holdCtx(ceiling)
		defer cancel()
		r := recvBrokered(t, startBrokered(p, ctx, grant), ceiling+margin, "queued caller with a hold budget")
		if r.err == nil || !strings.Contains(r.err.Error(), "waiting for another request's Git credential") {
			t.Errorf("queued caller = %q, %v, want the wait refusal", r.tok, r.err)
		}
		if r.at < ceiling-50*time.Millisecond {
			t.Errorf("left after %s, before its %s ceiling", r.at, ceiling)
		}
	})

	t.Run("a spent budget refuses a queued caller at once", func(t *testing.T) {
		ctx, cancel := holdCtx(time.Nanosecond)
		defer cancel()
		_ = holdFor(ctx, time.Hour) // starts the hold clock
		r := recvBrokered(t, startBrokered(p, ctx, grant), margin, "queued caller with a spent budget")
		if r.err == nil || !strings.Contains(r.err.Error(), "waiting for another request's Git credential") {
			t.Errorf("queued caller = %q, %v, want the wait refusal", r.tok, r.err)
		}
	})

	t.Run("without a hold budget the caller still waits", func(t *testing.T) {
		ctx, cancel := holdCtx(0)
		ch := startBrokered(p, ctx, grant)
		select {
		case r := <-ch:
			t.Fatalf("sidecar caller returned after %s with %q, %v; want it still waiting at ceiling+margin", r.at, r.tok, r.err)
		case <-time.After(ceiling + margin):
		}
		cancel()
		if r := recvBrokered(t, ch, margin, "sidecar caller after cancel"); r.err == nil {
			t.Errorf("cancelled sidecar caller got a token %q", r.tok)
		}
	})

	if n := mints.Load(); n != 1 {
		t.Errorf("%d mints, want 1: queued callers must not mint while the first holds the grant", n)
	}
	cancelFirst()
	recvBrokered(t, firstDone, margin, "first caller after cancel")
}

// TestGitBrokerHoldBudgetBoundsTheMint: a request that takes a free turn spends only what is left of its hold
// budget on the mint, and with none left asks for nothing.
func TestGitBrokerHoldBudgetBoundsTheMint(t *testing.T) {
	const (
		ceiling = 200 * time.Millisecond
		margin  = 250 * time.Millisecond
	)
	t.Setenv(envGitApprovalTimeout, "1h")
	grant := uuid.New()
	srv, minted, mints := holdGitUpstream(t, true)
	p, _ := newGitBrokerProxy(t, map[string]uuid.UUID{"octocat/hello-world": grant}, upstreamAddr(srv))

	t.Run("a mint that outlasts the remainder ends at the ceiling", func(t *testing.T) {
		ctx, cancel := holdCtx(ceiling)
		defer cancel()
		ch := startBrokered(p, ctx, grant)
		<-minted
		r := recvBrokered(t, ch, ceiling+margin, "hanging mint with a hold budget")
		if r.err == nil {
			t.Errorf("hanging mint returned a token %q", r.tok)
		}
	})

	t.Run("a spent budget does not mint", func(t *testing.T) {
		before := mints.Load()
		ctx, cancel := holdCtx(time.Nanosecond)
		defer cancel()
		_ = holdFor(ctx, time.Hour)
		r := recvBrokered(t, startBrokered(p, ctx, grant), margin, "mint with a spent budget")
		if r.err == nil {
			t.Errorf("spent budget returned a token %q", r.tok)
		}
		if n := mints.Load(); n != before {
			t.Errorf("%d mints with a spent budget, want none", n-before)
		}
	})
}

// TestGitBrokerCachedTokenIgnoresASpentBudget: the cached path is served at once with the budget spent, whether
// the turn is free or another request holds it.
func TestGitBrokerCachedTokenIgnoresASpentBudget(t *testing.T) {
	grant := uuid.New()
	srv, _, mints := holdGitUpstream(t, false)
	p, _ := newGitBrokerProxy(t, map[string]uuid.UUID{"octocat/hello-world": grant}, upstreamAddr(srv))
	e := &gitTokEntry{turn: make(chan struct{}, 1), token: "cached-token", username: "u"}
	p.gitTokMu.Lock()
	p.gitTokens[grant] = e
	p.gitTokMu.Unlock()

	ctx, cancel := holdCtx(time.Nanosecond)
	defer cancel()
	_ = holdFor(ctx, time.Hour)
	if r := recvBrokered(t, startBrokered(p, ctx, grant), time.Second, "cached token"); r.err != nil || r.tok != "cached-token" {
		t.Fatalf("cached token = %q, %v, want it served", r.tok, r.err)
	}

	e.turn <- struct{}{} // another request owns the turn: waiting for it is refused, not served
	r := recvBrokered(t, startBrokered(p, ctx, grant), time.Second, "cached token behind a held turn")
	<-e.turn
	if r.err == nil || !strings.Contains(r.err.Error(), "waiting for another request's Git credential") {
		t.Errorf("behind a held turn = %q, %v, want the wait refusal", r.tok, r.err)
	}
	if n := mints.Load(); n != 0 {
		t.Errorf("%d mints, want none", n)
	}
}
