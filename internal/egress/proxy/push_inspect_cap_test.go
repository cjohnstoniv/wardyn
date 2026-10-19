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
// The end is read by applyPushRules' buffering, which runs before the
// inspection slot is taken — confinePush before it reads only the command
// section — so a body parked there holds bytes, never the slot.
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

// A push body is read before the inspection slot is taken, so several pushes
// whose bodies are still arriving are all buffered at once, and a slow one
// holds no other push back. Each is then inspected and forwarded.
func TestPushRulesReadBeforeTheInspectionSlot(t *testing.T) {
	up := newGitBrokerUpstream(t, "gh-inst-token")
	p, _ := newGitBrokerProxyWithSpec(t,
		map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv),
		contentRulesSpec(".github/workflows/**"))
	body := recordedPush(t, BranchNSPrefix(p.runID)+"work", map[string]string{"src/app.go": "package main\n"})

	const pushes = 4
	atEnd, proceed := make(chan struct{}), make(chan struct{})
	recs := make([]*httptest.ResponseRecorder, pushes)
	var wg sync.WaitGroup
	for i := range pushes {
		req := mustLocalReq(t, http.MethodPost, "/wardyn/gh/octocat/hello-world/git-receive-pack",
			&gatedPush{r: bytes.NewReader(body), atEnd: atEnd, proceed: proceed})
		// git declares the length of a push smaller than its post buffer, and
		// a declared length is what the buffer budget charges up front.
		req.ContentLength = int64(len(body))
		recs[i] = httptest.NewRecorder()
		wg.Go(func() { p.ServeHTTP(recs[i], req) })
	}
	// Every push parks at its end before any is let go.
	for i := range pushes {
		select {
		case <-atEnd:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d pushes reached the end of their body at once: a parked read holds the others back", i, pushes)
		}
	}
	close(proceed)
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
