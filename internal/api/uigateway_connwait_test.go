// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestUIGateway_AWaitBehindHeldStreamsEndsInTheCapRefusal: behind maxUIConnsPerRun long-lived
// streams (a websocket, an event stream) no connection comes free, so a request waits no longer
// than uiConnWait and then gets the connection-cap 503 it got at once before requests could wait.
// The streams it queued behind are untouched.
func TestUIGateway_AWaitBehindHeldStreamsEndsInTheCapRefusal(t *testing.T) {
	prev := uiConnWait
	uiConnWait = 300 * time.Millisecond
	t.Cleanup(func() { uiConnWait = prev })
	arrived, release := make(chan struct{}, maxUIConnsPerRun), make(chan struct{})
	h := newUIHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/events" {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			arrived <- struct{}{}
			<-release
			return
		}
		_, _ = io.WriteString(w, "sandbox app")
	}))
	cookie := h.openSession()
	streams := make(chan int, maxUIConnsPerRun)
	for range maxUIConnsPerRun {
		go func() { streams <- h.relay("/events", cookie, nil).Code }()
	}
	for range maxUIConnsPerRun {
		<-arrived
	}

	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- h.relay("/ide", cookie, nil) }()
	select {
	case rec := <-answered:
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), reasonUIGatewayConnCapReached) {
			t.Errorf("a request behind %d held streams = %d %s, want 503 %s", maxUIConnsPerRun, rec.Code, rec.Body, reasonUIGatewayConnCapReached)
		}
	case <-time.After(uiConnWait + 5*time.Second):
		t.Errorf("a request behind %d held streams was still waiting %v past the %v bound", maxUIConnsPerRun, 5*time.Second, uiConnWait)
	}
	close(release)
	for range maxUIConnsPerRun {
		if code := <-streams; code != http.StatusOK {
			t.Errorf("a held stream answered %d, want 200", code)
		}
	}
}
