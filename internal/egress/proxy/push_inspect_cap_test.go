// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// gatedPush is a push body that counts how many bodies are at their end at
// once, holding each there for a moment. The end is read by applyPushRules'
// buffering, which runs under the same hold of the inspection slot as
// gitpack.Inspect — confinePush before it reads only the command section — so
// two bodies at their end together would mean two inspections could run.
type gatedPush struct {
	r            *bytes.Reader
	inside, peak *atomic.Int32
	ended        bool
}

func (g *gatedPush) Read(b []byte) (int, error) {
	n, err := g.r.Read(b)
	if err == io.EOF && !g.ended {
		g.ended = true
		now := g.inside.Add(1)
		for p := g.peak.Load(); now > p && !g.peak.CompareAndSwap(p, now); p = g.peak.Load() {
		}
		time.Sleep(50 * time.Millisecond) // long enough for an uncapped second push to overlap
		g.inside.Add(-1)
	}
	return n, err
}

// TestPushRulesInspectOnePushAtATime pins the concurrency half of #250: one
// inspection's memory is bounded by internal/gitpack's ceilings, and total
// inspection memory is that figure times how many run at once — which the agent
// would choose if nothing capped it. Several pushes sent together are each
// inspected and forwarded, but never two inside the slot at the same time.
func TestPushRulesInspectOnePushAtATime(t *testing.T) {
	up := newGitBrokerUpstream(t, "gh-inst-token")
	p, _ := newGitBrokerProxyWithSpec(t,
		map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv),
		contentRulesSpec(".github/workflows/**"))
	body := recordedPush(t, BranchNSPrefix(p.runID)+"work", map[string]string{"src/app.go": "package main\n"})

	const pushes = 4
	var inside, peak atomic.Int32
	recs := make([]*httptest.ResponseRecorder, pushes)
	var wg sync.WaitGroup
	for i := range pushes {
		req := mustLocalReq(t, http.MethodPost, "/wardyn/gh/octocat/hello-world/git-receive-pack",
			&gatedPush{r: bytes.NewReader(body), inside: &inside, peak: &peak})
		recs[i] = httptest.NewRecorder()
		wg.Go(func() { p.ServeHTTP(recs[i], req) })
	}
	wg.Wait()

	for i, rec := range recs {
		if rec.Code != http.StatusOK {
			t.Errorf("push %d: status %d, want 200 — a queued push is inspected, not refused: %s", i, rec.Code, rec.Body)
		}
	}
	if got := peak.Load(); got != 1 {
		t.Errorf("%d pushes were inside the inspection slot at once, want 1 (maxConcurrentScans)", got)
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if up.gitHits != pushes {
		t.Errorf("forge saw %d pushes, want %d", up.gitHits, pushes)
	}
}
