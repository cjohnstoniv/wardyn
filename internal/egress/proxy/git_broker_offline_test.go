// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

func offlineCounts(up *gitBrokerUpstream) (mints, hits int) {
	up.mu.Lock()
	defer up.mu.Unlock()
	return up.mintCalls, up.gitHits
}

// TestGitBrokerOfflineBound: the GitHub App lane stops adding its token once the proxy has gone 30 minutes
// without renewing its run token, mints nothing while it refuses, and serves the kept token after the next
// renewal.
func TestGitBrokerOfflineBound(t *testing.T) {
	up := newGitBrokerUpstream(t, "ghs_offline")
	p, buf := newGitBrokerProxy(t, map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv))
	const path = "/wardyn/gh/octocat/hello-world.git/info/refs?service=git-upload-pack"
	get := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, path, nil))
		return rec
	}

	if rec := get(); rec.Code != http.StatusOK {
		t.Fatalf("(a) status %d, want 200: %s", rec.Code, rec.Body)
	}
	if mints, _ := offlineCounts(up); mints != 1 {
		t.Fatalf("(a) %d mints, want 1", mints)
	}

	p.runToken.atMs = time.Now().Add(-29 * time.Minute).UnixMilli()
	if rec := get(); rec.Code != http.StatusOK {
		t.Fatalf("(b) status %d, want 200 at 29 minutes: %s", rec.Code, rec.Body)
	}
	mints, hitsB := offlineCounts(up)
	if mints != 1 {
		t.Fatalf("(b) %d mints, want 1", mints)
	}

	p.runToken.atMs = time.Now().Add(-30 * time.Minute).UnixMilli()
	rec := get()
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "has not renewed its run token") {
		t.Fatalf("(c) status %d body %q, want 502 saying the run token was not renewed", rec.Code, rec.Body)
	}
	mints, hitsC := offlineCounts(up)
	if mints != 1 || hitsC != hitsB {
		t.Fatalf("(c) %d mints, %d forge hits (was %d), want 1 and no new hit", mints, hitsC, hitsB)
	}
	if d := findDecision(t, buf, ruleSourceGit); d.Decision != egress.Deny {
		t.Fatalf("(c) last %s row %+v, want deny", ruleSourceGit, d)
	}

	p.runToken.Set("RUNTOK2")
	if rec := get(); rec.Code != http.StatusOK {
		t.Fatalf("(d) status %d, want 200 after a renewal: %s", rec.Code, rec.Body)
	}
	if mints, _ := offlineCounts(up); mints != 1 {
		t.Fatalf("(d) %d mints, want 1: the kept token is served, nothing is minted again", mints)
	}
}

// TestPATBrokerOfflineBound: the git_pat PAT broker takes the same gate through brokeredToken.
func TestPATBrokerOfflineBound(t *testing.T) {
	up := newPATBrokerUpstream(t, "T", "oauth2")
	p, _ := newPATBrokerProxy(t, map[string]PATGrant{"gitlab.com": {GrantID: uuid.New()}}, upstreamAddr(up.srv))
	const path = "/wardyn/git/gitlab.com/org/repo.git/info/refs?service=git-upload-pack"
	get := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, path, nil))
		return rec
	}

	if rec := get(); rec.Code != http.StatusOK {
		t.Fatalf("(a) status %d, want 200: %s", rec.Code, rec.Body)
	}
	if mints, _ := offlineCounts(up); mints != 1 {
		t.Fatalf("(a) %d mints, want 1", mints)
	}

	p.runToken.atMs = time.Now().Add(-30 * time.Minute).UnixMilli()
	rec := get()
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "has not renewed its run token") {
		t.Fatalf("(b) status %d body %q, want 502 saying the run token was not renewed", rec.Code, rec.Body)
	}
	if mints, _ := offlineCounts(up); mints != 1 {
		t.Fatalf("(b) %d mints, want 1", mints)
	}
}

// TestBrokeredTokenOfflineMintsNothing covers the GitHub CLI door, which calls brokeredToken directly.
func TestBrokeredTokenOfflineMintsNothing(t *testing.T) {
	up := newGitBrokerUpstream(t, "ghs_cli")
	p, _ := newGitBrokerProxy(t, map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv))
	grant := uuid.New()
	wire := func(string) string { return gitBrokerUsername }

	p.runToken.atMs = time.Now().Add(-31 * time.Minute).UnixMilli()
	if _, _, err := p.brokeredToken(context.Background(), grant, wire); !errors.Is(err, errBrokerOffline) {
		t.Fatalf("err %v, want errBrokerOffline", err)
	}
	if mints, _ := offlineCounts(up); mints != 0 {
		t.Fatalf("%d mints while offline, want 0", mints)
	}

	p.runToken.Set("RUNTOK2")
	tok, _, err := p.brokeredToken(context.Background(), grant, wire)
	if err != nil || tok != "ghs_cli" {
		t.Fatalf("token %q err %v after a renewal, want ghs_cli and no error", tok, err)
	}
	if mints, _ := offlineCounts(up); mints != 1 {
		t.Fatalf("%d mints after a renewal, want 1", mints)
	}
}

// TestRenewMaxIntervalHalfOfflineBound: a proxy that reaches the organisation renews twice inside the bound.
func TestRenewMaxIntervalHalfOfflineBound(t *testing.T) {
	max := defaultRenewerTuning().maxInterval
	if max != 15*time.Minute || 2*max > brokerOfflineBound {
		t.Fatalf("maxInterval %v, want 15m and at most half of %v", max, brokerOfflineBound)
	}
}

func TestTokenSourceQuiet(t *testing.T) {
	ts := newTokenSource("a")
	if q := ts.quiet(time.Now()); q >= 5*time.Second {
		t.Fatalf("fresh source quiet %v, want under 5s", q)
	}
	if q := (*tokenSource)(nil).quiet(time.Now()); q != 0 {
		t.Fatalf("nil source quiet %v, want 0", q)
	}
	ts.atMs = time.Now().Add(-10 * time.Minute).UnixMilli()
	if q := ts.quiet(time.Now()); q < 10*time.Minute || q >= 10*time.Minute+5*time.Second {
		t.Fatalf("quiet %v, want 10m to 10m5s", q)
	}
	ts.Set("b")
	if q := ts.quiet(time.Now()); q >= 5*time.Second {
		t.Fatalf("quiet after Set %v, want under 5s", q)
	}
}

// TestBrokeredTokenEngineIgnoresOfflineBound: the via_org engine has no renewer, so its token source ages;
// past the bound it still mints, while a runner-side proxy with the same quiet refuses.
func TestBrokeredTokenEngineIgnoresOfflineBound(t *testing.T) {
	wire := func(string) string { return gitBrokerUsername }
	for _, tc := range []struct {
		name   string
		engine bool
	}{{"engine", true}, {"runner proxy", false}} {
		t.Run(tc.name, func(t *testing.T) {
			up := newGitBrokerUpstream(t, "ghs_engine")
			p, _ := newGitBrokerProxy(t, map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv))
			p.engine = tc.engine
			p.runToken.atMs = time.Now().Add(-45 * time.Minute).UnixMilli()
			tok, _, err := p.brokeredToken(context.Background(), uuid.New(), wire)
			if tc.engine {
				if err != nil || tok != "ghs_engine" {
					t.Fatalf("token %q err %v, want ghs_engine and no error", tok, err)
				}
				if mints, _ := offlineCounts(up); mints != 1 {
					t.Fatalf("%d mints, want 1", mints)
				}
				return
			}
			if !errors.Is(err, errBrokerOffline) {
				t.Fatalf("err %v, want errBrokerOffline", err)
			}
			if mints, _ := offlineCounts(up); mints != 0 {
				t.Fatalf("%d mints, want 0", mints)
			}
		})
	}
}
