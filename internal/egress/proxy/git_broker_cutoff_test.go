// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const cutoffPath = "/wardyn/gh/octocat/hello-world.git/info/refs?service=git-upload-pack"

// setQuiet makes p's run token d old, under the source's lock (a request may be reading it).
func setQuiet(p *Proxy, d time.Duration) {
	p.runToken.mu.Lock()
	p.runToken.atMs = time.Now().Add(-d).UnixMilli()
	p.runToken.mu.Unlock()
}

func cutoffGet(t *testing.T, p *Proxy) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, cutoffPath, nil))
	return rec
}

// TestGitBrokerOfflineBoundAfterTheGrantWait: a request that waited behind another request's acquisition of the
// same grant, and crossed the 30 minutes while it waited, gets no credential; the next renewal serves the kept
// token with no second mint.
func TestGitBrokerOfflineBoundAfterTheGrantWait(t *testing.T) {
	up := newGitBrokerUpstream(t, "ghs_wait")
	grant := uuid.New()
	p, _ := newGitBrokerProxy(t, map[string]uuid.UUID{"octocat/hello-world": grant}, upstreamAddr(up.srv))
	if rec := cutoffGet(t, p); rec.Code != http.StatusOK {
		t.Fatalf("(a) status %d, want 200: %s", rec.Code, rec.Body)
	}
	p.gitTokMu.Lock()
	e := p.gitTokens[grant]
	p.gitTokMu.Unlock()
	_, hits := offlineCounts(up)

	setQuiet(p, 29*time.Minute)
	e.turn <- struct{}{} // another request's acquisition holds the grant
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- cutoffGet(t, p) }()
	time.Sleep(100 * time.Millisecond) // the request is queued behind the holder by now
	setQuiet(p, 30*time.Minute)
	<-e.turn
	rec := <-done
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "has not renewed its run token") {
		t.Fatalf("(b) status %d body %q, want 502: the wait crossed the bound", rec.Code, rec.Body)
	}
	if mints, h := offlineCounts(up); mints != 1 || h != hits {
		t.Fatalf("(b) %d mints, %d forge hits (was %d), want 1 and no new hit", mints, h, hits)
	}
	p.runToken.Set("RUNTOK2")
	if rec := cutoffGet(t, p); rec.Code != http.StatusOK {
		t.Fatalf("(c) status %d after a renewal, want 200: %s", rec.Code, rec.Body)
	}
	if mints, _ := offlineCounts(up); mints != 1 {
		t.Fatalf("(c) %d mints, want 1: the kept token is served", mints)
	}
}

// gatedResolver answers as publicResolver, but once armed it holds the next github.com lookup until released.
type gatedResolver struct {
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (g *gatedResolver) LookupIP(host string) ([]net.IP, error) {
	if host == githubHost && g.armed.CompareAndSwap(true, false) {
		close(g.entered)
		<-g.release
	}
	return publicResolver{}.LookupIP(host)
}

// TestGitBrokerOfflineBoundAfterTheVet: the token came from the cache inside the 30 minutes, the vet of
// github.com finished outside them, and the request is refused before anything is forwarded.
func TestGitBrokerOfflineBoundAfterTheVet(t *testing.T) {
	up := newGitBrokerUpstream(t, "ghs_vet")
	gate := &gatedResolver{entered: make(chan struct{}), release: make(chan struct{})}
	buf := &bytes.Buffer{}
	p := newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{}),
		Sink:            &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
		Resolver:        gate,
		Dial:            redirectDial(upstreamAddr(up.srv)),
		ControlPlaneURL: "https://wardynd.test:8080",
		RunToken:        newTokenSource("RUNTOK"),
		TLSClientConfig: testInsecureTLSConfig,
		ControlTLS:      testInsecureTLSConfig,
		GitGrants:       map[string]uuid.UUID{"octocat/hello-world": uuid.New()},
	})
	if rec := cutoffGet(t, p); rec.Code != http.StatusOK {
		t.Fatalf("(a) status %d, want 200: %s", rec.Code, rec.Body)
	}
	_, hits := offlineCounts(up)
	// The proxy reuses an answer for dnsCacheTTL: the vet below looks github.com up again once that has passed,
	// which an empty cache over the same resolver stands for.
	p.res = newCachedResolver(gate, time.Now)

	setQuiet(p, 29*time.Minute)
	gate.armed.Store(true)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- cutoffGet(t, p) }()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never looked github.com up")
	}
	setQuiet(p, 30*time.Minute)
	close(gate.release)
	rec := <-done
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "has not renewed its run token") {
		t.Fatalf("(b) status %d body %q, want 502: the vet crossed the bound", rec.Code, rec.Body)
	}
	if mints, h := offlineCounts(up); mints != 1 || h != hits {
		t.Fatalf("(b) %d mints, %d forge hits (was %d), want 1 and no new hit", mints, h, hits)
	}
	if d := findDecision(t, buf, ruleSourceGit); d.Decision != egress.Deny {
		t.Fatalf("(b) last %s row %+v, want deny", ruleSourceGit, d)
	}
}

// pendingApprovalUpstream is a control plane whose every mint waits on an approval that stays pending.
func pendingApprovalUpstream(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var mints atomic.Int32
	approval := uuid.NewString()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/internal/credentials/mint":
			mints.Add(1)
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"approval_id":"`+approval+`"}`)
		case strings.HasPrefix(r.URL.Path, "/api/v1/internal/approvals/"):
			_, _ = io.WriteString(w, `{"id":"`+approval+`","state":"PENDING"}`)
		default:
			http.Error(w, "no forge in this test", http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &mints
}

// TestGitBrokerQueuedFetchSpendsItsOwnBudget: two fetches share a grant whose approval never comes. The second,
// queued behind the first, ends at its own budget rather than the first's budget plus a fresh one, leaves at once
// when its request is cancelled, and never mints while the first holds the grant.
func TestGitBrokerQueuedFetchSpendsItsOwnBudget(t *testing.T) {
	orig := gitApprovalPollInterval
	gitApprovalPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { gitApprovalPollInterval = orig })
	const budget = time.Second
	t.Setenv(envGitApprovalTimeout, budget.String())
	srv, mints := pendingApprovalUpstream(t)
	p, _ := newGitBrokerProxy(t, map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(srv))

	fetch := func(ctx context.Context) (int, time.Duration) {
		start := time.Now()
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, cutoffPath, nil).WithContext(ctx))
		return rec.Code, time.Since(start)
	}
	waitMints := func(n int32) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); mints.Load() < n; time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%d mints, want %d: the first fetch never asked for the credential", mints.Load(), n)
			}
		}
	}

	t.Run("a queued fetch ends at its own budget", func(t *testing.T) {
		first := make(chan struct{})
		go func() { _, _ = fetch(context.Background()); close(first) }()
		waitMints(1)
		time.Sleep(300 * time.Millisecond)
		code, took := fetch(context.Background())
		if code == http.StatusOK || took > budget+350*time.Millisecond {
			t.Errorf("queued fetch = %d after %v, want a refusal within its own %v", code, took, budget)
		}
		<-first
	})

	t.Run("a queued fetch leaves when its request ends", func(t *testing.T) {
		before := mints.Load()
		first := make(chan struct{})
		go func() { _, _ = fetch(context.Background()); close(first) }()
		waitMints(before + 1)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		code, took := fetch(ctx)
		if code == http.StatusOK || took > 500*time.Millisecond {
			t.Errorf("cancelled fetch = %d after %v, want a refusal soon after its 100ms", code, took)
		}
		if n := mints.Load(); n != before+1 {
			t.Errorf("%d mints while the first fetch held the grant, want %d", n, before+1)
		}
		<-first
	})
}
