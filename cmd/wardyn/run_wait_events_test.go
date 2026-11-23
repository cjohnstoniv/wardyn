// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// eventsServer answers the run read from state(), the audit reads with an empty list, and the event stream with
// events (nil: a 404, a server without the feed).
func eventsServer(t *testing.T, state func() types.RunState, events http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/events"):
			if events == nil {
				http.NotFound(w, r)
				return
			}
			events(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/v1/audit"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(types.AgentRun{State: state()})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A state event on the run's stream wakes the poll at once: with a 10 s poll interval the wait still ends well inside
// its 5 s budget.
func TestWaitForRun_AStateEventWakesThePoll(t *testing.T) {
	setWaitPollInterval(t, 10*time.Second)
	var done atomic.Bool
	srv := eventsServer(t, func() types.RunState {
		if done.Load() {
			return types.RunCompleted
		}
		return types.RunRunning
	}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(100 * time.Millisecond)
		done.Store(true)
		_, _ = io.WriteString(w, "data: {\"id\":1,\"type\":\"ended\",\"state\":\"COMPLETED\"}\n\n")
		w.(http.Flusher).Flush()
	})
	c := &sdk.Client{BaseURL: srv.URL, HTTPClient: &http.Client{}}
	start := time.Now()
	if err := waitForRun(context.Background(), io.Discard, c, uuid.New(), 5*time.Second); err != nil {
		t.Fatalf("waitForRun = %v; want nil", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("the wait took %v; want the event to wake the poll well inside 3 s", took)
	}
}

// A server with no event stream leaves the poll as it was.
func TestWaitForRun_AServerWithoutEventsStillPolls(t *testing.T) {
	setWaitPollInterval(t, time.Millisecond)
	var reads atomic.Int32
	srv := eventsServer(t, func() types.RunState {
		if reads.Add(1) <= 2 {
			return types.RunRunning
		}
		return types.RunCompleted
	}, nil)
	c := &sdk.Client{BaseURL: srv.URL, HTTPClient: &http.Client{}}
	if err := waitForRun(context.Background(), io.Discard, c, uuid.New(), 5*time.Second); err != nil {
		t.Fatalf("waitForRun = %v; want nil", err)
	}
}

// The stream is closed before waitForRun returns.
func TestWaitForRun_TheEventStreamEndsWithTheWait(t *testing.T) {
	setWaitPollInterval(t, time.Millisecond)
	opened, closed := make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	srv := eventsServer(t, func() types.RunState {
		select { // the run reads COMPLETED only once the stream is open
		case <-opened:
			return types.RunCompleted
		case <-time.After(2 * time.Second):
			return types.RunRunning
		}
	}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		if once.CompareAndSwap(false, true) {
			close(opened)
		}
		<-r.Context().Done()
		close(closed)
	})
	c := &sdk.Client{BaseURL: srv.URL, HTTPClient: &http.Client{}}
	if err := waitForRun(context.Background(), io.Discard, c, uuid.New(), 5*time.Second); err != nil {
		t.Fatalf("waitForRun = %v; want nil", err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("the event stream was still open a second after waitForRun returned")
	}
}
