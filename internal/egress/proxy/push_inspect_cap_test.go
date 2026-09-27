// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// gatedPush is a push body that parks at its end until the test lets it go.
// The end is read by applyPushRules' buffering, which runs under the same hold
// of the inspection slot as gitpack.Inspect — confinePush before it reads only
// the command section — so a body parked there is one push inside the slot.
type gatedPush struct {
	r       *bytes.Reader
	atEnd   chan<- struct{}
	proceed <-chan struct{}
	ended   bool
}

func (g *gatedPush) Read(b []byte) (int, error) {
	n, err := g.r.Read(b)
	if err == io.EOF && !g.ended {
		g.ended = true
		g.atEnd <- struct{}{}
		<-g.proceed
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

	if cap(scanSlots) != 1 {
		t.Fatalf("scanSlots admits %d inspections at once, want 1 (maxConcurrentScans)", cap(scanSlots))
	}
	const pushes = 4
	atEnd, proceed := make(chan struct{}), make(chan struct{})
	recs := make([]*httptest.ResponseRecorder, pushes)
	var wg sync.WaitGroup
	for i := range pushes {
		req := mustLocalReq(t, http.MethodPost, "/wardyn/gh/octocat/hello-world/git-receive-pack",
			&gatedPush{r: bytes.NewReader(body), atEnd: atEnd, proceed: proceed})
		recs[i] = httptest.NewRecorder()
		wg.Go(func() { p.ServeHTTP(recs[i], req) })
	}
	// Each push in turn parks at its end; while it does, the slot must be held,
	// and held by it alone. A read of the end outside the slot finds it empty.
	for i := range pushes {
		select {
		case <-atEnd:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d pushes reached the inspection read", i, pushes)
		}
		if held := len(scanSlots); held != 1 {
			t.Errorf("a push read its end with %d inspection slots held, want 1", held)
		}
		proceed <- struct{}{}
	}
	wg.Wait()

	for i, rec := range recs {
		if rec.Code != http.StatusOK {
			t.Errorf("push %d: status %d, want 200 — a queued push is inspected, not refused: %s", i, rec.Code, rec.Body)
		}
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if up.gitHits != pushes {
		t.Errorf("forge saw %d pushes, want %d", up.gitHits, pushes)
	}
}
