// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
)

// redirectingCollector answers every POST to its own URL with code, pointing at
// a final collector that records each request it gets.
func redirectingCollector(t *testing.T, code int) (url string, got func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, fmt.Sprintf("%s %d", r.Method, len(b)))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(final.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		http.Redirect(w, r, final.URL+"/final", code)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/ingest", func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func runRedirectSink(t *testing.T, url string) *sinks.WebhookSink {
	t.Helper()
	sink, err := sinks.NewWebhookSink(sinks.WebhookConfig{
		URL: url, BatchSize: 1, FlushInterval: "30ms", BufferSize: 16, MaxRetries: 2, RetryBaseDelay: "1ms",
	})
	if err != nil {
		t.Fatalf("NewWebhookSink: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sink.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	if err := sink.Emit(ctx, makeEvent("redirect.test")); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	return sink
}

// A 301/302/303 would turn the POST into a bodyless GET. The sink must not
// follow it: the collector never received the batch, so it is retried and then
// counted as dropped rather than reported delivered by the redirect target's 2xx.
func TestWebhookSink_MethodChangingRedirectIsNotFollowed(t *testing.T) {
	t.Parallel()
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			t.Parallel()
			url, got := redirectingCollector(t, code)
			sink := runRedirectSink(t, url)
			waitThenSettle(t, 30*time.Millisecond, func() bool { return sink.Drops() != 0 })
			if d := sink.Drops(); d != 1 {
				t.Errorf("drops after a %d = %d, want 1: the batch never reached the collector", code, d)
			}
			if seen := got(); len(seen) != 0 {
				t.Errorf("the %d target was contacted with %v, want it never followed", code, seen)
			}
		})
	}
}

// Unchanged: a 307/308 keeps the method and body, so the sink still follows it
// and the batch is delivered to the target with nothing dropped.
func TestWebhookSink_MethodPreservingRedirectStillDelivers(t *testing.T) {
	t.Parallel()
	for _, code := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			t.Parallel()
			url, got := redirectingCollector(t, code)
			sink := runRedirectSink(t, url)
			waitThenSettle(t, 30*time.Millisecond, func() bool { return len(got()) != 0 })
			seen := got()
			if len(seen) != 1 || seen[0] == "POST 0" || seen[0][:5] != "POST " {
				t.Errorf("the %d target got %v, want one POST carrying the batch", code, seen)
			}
			if d := sink.Drops(); d != 0 {
				t.Errorf("drops after a followed %d = %d, want 0", code, d)
			}
		})
	}
}
