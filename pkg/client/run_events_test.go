// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestRunEvents_ResumesAHeldStreamWithoutGaps: the server closes a held
// stream before the run ends; RunEvents must reconnect with the last id it
// delivered, hand fn every event exactly once, and return nil after ended.
func TestRunEvents_ResumesAHeldStreamWithoutGaps(t *testing.T) {
	runID := uuid.New()
	var mu sync.Mutex
	var lastIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/runs/"+runID.String()+"/events" || r.Header.Get("Accept") != "text/event-stream" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		lastIDs = append(lastIDs, r.Header.Get("Last-Event-ID"))
		n := len(lastIDs)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(id int, typ, extra string) {
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: {\"id\":%d,\"type\":%q%s,\"at\":%q}\n\n", id, typ, id, typ, extra,
				time.Now().UTC().Format(time.RFC3339))
		}
		fmt.Fprint(w, ": keepalive\n\n")
		if n == 1 {
			send(1, client.RunEventProvisioning, "")
			send(2, client.RunEventReady, "")
			return // held stream closed before the end
		}
		send(3, client.RunEventEnded, `,"state":"COMPLETED"`)
	}))
	t.Cleanup(srv.Close)

	var got []client.RunEvent
	c := client.New(srv.URL, testToken)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.RunEvents(ctx, runID, 0, func(ev client.RunEvent) error {
		got = append(got, ev)
		return nil
	}); err != nil {
		t.Fatalf("RunEvents: %v", err)
	}
	if len(got) != 3 || got[0].ID != 1 || got[1].ID != 2 || got[2].ID != 3 ||
		got[2].Type != client.RunEventEnded || got[2].State != client.RunCompleted {
		t.Fatalf("events = %+v, want ids 1,2,3 ending in ended{COMPLETED}", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lastIDs) != 2 || lastIDs[0] != "" || lastIDs[1] != "2" {
		t.Fatalf("Last-Event-ID per connection = %q, want [\"\" \"2\"]", lastIDs)
	}
}

// TestRunEvents_NotFoundIsAnAPIError: a caller who cannot read the run gets
// the server's 404 as *APIError, not a silent empty feed.
func TestRunEvents_NotFoundIsAnAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"run not found","reason":"run_not_found"}`)
	}))
	t.Cleanup(srv.Close)
	err := client.New(srv.URL, testToken).RunEvents(context.Background(), uuid.New(), 0,
		func(client.RunEvent) error { t.Fatal("fn called on a 404"); return nil })
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("err = %v, want *APIError 404", err)
	}
}

// TestRunEvents_ANonStreamAnswerIsAnError: a 2xx that is not an event stream
// (a captive portal, an SPA fallback) carries no events and never ends, so
// RunEvents must refuse it rather than reconnect until ctx runs out.
func TestRunEvents_ANonStreamAnswerIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<!doctype html>")
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := client.New(srv.URL, testToken).RunEvents(ctx, uuid.New(), 0,
		func(client.RunEvent) error { t.Fatal("fn called on a non-stream"); return nil })
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a prompt non-stream error", err)
	}
}
