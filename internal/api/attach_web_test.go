// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestAttachWebOpeningFrameReadsAPromotionInFlight: the opening attach-mode frame
// is decided inside modeMu. An observer promoted while that frame waits for the
// mutex (a promotion frame holds it) is told it is the writer. When the handler
// read canWrite before the lock, that frame said read_only:true, landed after the
// promotion's and greyed out the writer's terminal. The relay handler serves the
// console and `wardyn attach` alike, so this one path covers both clients.
func TestAttachWebOpeningFrameReadsAPromotionInFlight(t *testing.T) {
	srv, _, _, _, run := holderTestServer(t)
	h := &attachHolder{principal: holderSecond}
	served := make(chan *webAttach, 1)
	done := make(chan struct{})
	defer close(done)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		served <- &webAttach{s: srv, c: c, run: run, holder: h, pumpCtx: context.Background()}
		<-done
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.CloseNow() }()
	wa := <-served

	// The observer's opening frame is on its way while a promotion holds modeMu.
	wa.modeMu.Lock()
	sent := make(chan struct{})
	go func() {
		wa.sendMode(ctx)
		close(sent)
	}()
	h.writable.Store(true) // the promotion completes before the opening frame gets the mutex
	wa.modeMu.Unlock()
	<-sent

	if m := readAttachMode(t, c); m.ReadOnly {
		t.Fatal("the opening frame told a promoted writer it is read-only")
	}
}
