// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// makeEvent returns a minimal AuditEvent with a unique ID.
func makeEvent(action string) types.AuditEvent {
	return types.AuditEvent{
		ID:        uuid.New(),
		Time:      time.Now().UTC(),
		ActorType: types.ActorSystem,
		Actor:     "test",
		Action:    action,
		Outcome:   "success",
	}
}

// waitThenSettle polls every 5ms, for up to 2s, until cond holds, then waits
// one settle window more. The poll replaces a fixed sleep sized to "the flush
// interval plus some slack", which guessed how fast the machine is. The settle
// window is what lets the exact-count assertion after it see an over-count (a
// duplicate delivery, a re-POST after a 200, a second drop): the poll alone
// returns the moment the count is first reached. Callers pass their sink's
// FlushInterval.
func waitThenSettle(t *testing.T, settle time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(settle)
}

// collectBatches reads from a channel and counts total events received across
// all HTTP requests to the httptest server.
func TestWebhookSink_Batching(t *testing.T) {
	t.Parallel()

	var received atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify content-type.
		if ct := r.Header.Get("Content-Type"); ct != "application/x-ndjson" {
			t.Errorf("unexpected Content-Type: %q", ct)
		}
		body, _ := io.ReadAll(r.Body)
		scanner := bufio.NewScanner(bytes.NewReader(body))
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var ev types.AuditEvent
			if err := json.Unmarshal(line, &ev); err != nil {
				t.Errorf("unmarshal event: %v", err)
				continue
			}
			received.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	sink, err := sinks.NewWebhookSink(sinks.WebhookConfig{
		URL:           srv.URL,
		BatchSize:     5,
		FlushInterval: "50ms",
		BufferSize:    64,
		MaxRetries:    1,
	})
	if err != nil {
		t.Fatalf("NewWebhookSink: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sink.Run(ctx)
		close(done)
	}()

	// Emit 12 events: this should produce at least 2 batches (5+5+2 or 5+7).
	const total = 12
	for i := 0; i < total; i++ {
		if err := sink.Emit(ctx, makeEvent("test.batch")); err != nil {
			t.Fatalf("Emit: %v", err)
		}
	}

	waitThenSettle(t, 50*time.Millisecond, func() bool { return int(received.Load()) >= total })
	cancel()
	<-done

	if got := int(received.Load()); got != total {
		t.Errorf("received %d events, want %d", got, total)
	}
}

// The bearer is a replayable SIEM credential resent on every POST, so it is
// refused on a plaintext URL. Tokenless http:// keeps working. (This replaces a
// round-trip assertion on the Authorization header: with the gate in place a
// token can no longer be pointed at an httptest plaintext server at all.)
func TestWebhookSink_BearerRequiresHTTPS(t *testing.T) {
	t.Parallel()

	const token = "s3cr3t-b34r3r"
	tests := []struct {
		name    string
		url     string
		token   string
		wantErr bool
	}{
		{"http with bearer refused", "http://siem.internal/ingest", token, true},
		{"schemeless with bearer refused", "siem.internal/ingest", token, true},
		{"https with bearer ok", "https://siem.internal/ingest", token, false},
		{"http without bearer ok", "http://siem.internal/ingest", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := sinks.NewWebhookSink(sinks.WebhookConfig{URL: tt.url, BearerToken: tt.token})
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewWebhookSink(%q, token set: %v) error = %v, want error: %v",
					tt.url, tt.token != "", err, tt.wantErr)
			}
		})
	}
}

func TestWebhookSink_RetryOnServerError(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		// Fail first two attempts, succeed on the third.
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	sink, err := sinks.NewWebhookSink(sinks.WebhookConfig{
		URL:            srv.URL,
		BatchSize:      1,
		FlushInterval:  "50ms",
		MaxRetries:     3,
		RetryBaseDelay: "10ms",
	})
	if err != nil {
		t.Fatalf("NewWebhookSink: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sink.Run(ctx)
		close(done)
	}()

	_ = sink.Emit(ctx, makeEvent("retry.test"))
	waitThenSettle(t, 50*time.Millisecond, func() bool { return attempts.Load() >= 3 })
	cancel()
	<-done

	if got := int(attempts.Load()); got != 3 {
		t.Errorf("expected 3 delivery attempts, got %d", got)
	}
}

func TestWebhookSink_DropCounterOnOverflow(t *testing.T) {
	t.Parallel()

	// Server that never accepts connections (we want drops, not delivery).
	// Use a very small buffer so overflow happens immediately.
	sink, err := sinks.NewWebhookSink(sinks.WebhookConfig{
		// Unreachable: we don't need delivery for this test.
		URL:           "http://127.0.0.1:1", // port 1 is reserved, will fail fast
		BatchSize:     10,
		FlushInterval: "1h", // never flush via ticker
		BufferSize:    2,    // very small so we overflow easily
		MaxRetries:    1,
	})
	if err != nil {
		t.Fatalf("NewWebhookSink: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sink.Run(ctx)
		close(done)
	}()

	// Emit more events than the buffer can hold without any reader draining it.
	// We emit 10 events; buffer size=2 so at least 8 should be dropped (the
	// first 2 may be enqueued before the background goroutine starts draining).
	for i := 0; i < 10; i++ {
		_ = sink.Emit(ctx, makeEvent("overflow.test"))
	}

	cancel()
	<-done

	if drops := sink.Drops(); drops == 0 {
		t.Error("expected non-zero drop counter after overflow; got 0")
	}
}

// TestWebhookSink_DropCounterOnRetryExhaustion covers the finding that a batch
// lost after exhausting all delivery retries was logged but never counted, so
// the loss was invisible via Drops(). The httptest server always returns 500 so
// every attempt fails; after MaxRetries the events in the batch must be counted
// as drops.
func TestWebhookSink_DropCounterOnRetryExhaustion(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusInternalServerError) // always fail to force retries
	}))
	t.Cleanup(srv.Close)

	const maxRetries = 3
	sink, err := sinks.NewWebhookSink(sinks.WebhookConfig{
		URL:            srv.URL,
		BatchSize:      1, // one event per batch so a single Emit triggers delivery
		FlushInterval:  "30ms",
		BufferSize:     16,
		MaxRetries:     maxRetries,
		RetryBaseDelay: "1ms",
	})
	if err != nil {
		t.Fatalf("NewWebhookSink: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sink.Run(ctx)
		close(done)
	}()

	// A single event forms one batch; all retries fail, so that one event is lost.
	if err := sink.Emit(ctx, makeEvent("retry.exhaust")); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// deliverWithRetry records the drop only after the final failed attempt, so
	// a nonzero count already means every retry has been spent.
	waitThenSettle(t, 30*time.Millisecond, func() bool { return sink.Drops() != 0 })

	if drops := sink.Drops(); drops != 1 {
		t.Errorf("drop counter after retry exhaustion: got %d, want 1", drops)
	}

	cancel()
	<-done
}

// TestWebhookSink_CloseFlushesAndAwaitsDrain covers the finding that on shutdown
// the webhook drain goroutine was never awaited and the final batch was never
// flushed. Close() must (a) block until the background flusher has exited and
// (b) deliver the last buffered batch. The Run ctx is left live so this exercises
// the Close()/stop path specifically.
//
// The await is pinned by holding the final delivery open inside the handler
// rather than by looking at whether Run's goroutine has been scheduled. Only the
// former is ordered: the handler runs strictly before the POST completes, which
// runs before Run's deferred close of its done channel, which is what Close
// blocks on. So while the handler is held, a Close that awaits the drain cannot
// have returned. Sampling the Run goroutine instead is unordered — Run signals
// done from inside Run, so any statement after Run returns may still be pending
// when Close returns, and on a loaded box it is.
func TestWebhookSink_CloseFlushesAndAwaitsDrain(t *testing.T) {
	t.Parallel()

	var received atomic.Int32
	// inFlight reports that the drain's POST has reached the server; the handler
	// then parks on releaseDelivery so the delivery stays unfinished until the
	// test says otherwise.
	inFlight := make(chan struct{})
	releaseDelivery := make(chan struct{})
	signalInFlight := sync.OnceFunc(func() { close(inFlight) })
	release := sync.OnceFunc(func() { close(releaseDelivery) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		scanner := bufio.NewScanner(bytes.NewReader(body))
		for scanner.Scan() {
			if len(scanner.Bytes()) > 0 {
				received.Add(1)
			}
		}
		signalInFlight()
		<-releaseDelivery
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	// Runs before srv.Close (cleanups are LIFO) so a failed assertion cannot
	// leave the handler parked and srv.Close waiting on it.
	t.Cleanup(release)

	sink, err := sinks.NewWebhookSink(sinks.WebhookConfig{
		URL:           srv.URL,
		BatchSize:     100,  // large so the batch is not flushed by size
		FlushInterval: "1h", // large so the ticker never fires during the test
		BufferSize:    64,
		MaxRetries:    1,
	})
	if err != nil {
		t.Fatalf("NewWebhookSink: %v", err)
	}

	runDone := make(chan struct{})
	go func() {
		sink.Run(context.Background()) // live ctx: only Close should stop Run
		close(runDone)
	}()

	const total = 7
	for i := 0; i < total; i++ {
		if err := sink.Emit(context.Background(), makeEvent("close.flush")); err != nil {
			t.Fatalf("Emit: %v", err)
		}
	}

	// Nothing should have been delivered yet (batch not full, ticker not fired).
	if got := received.Load(); got != 0 {
		t.Fatalf("events delivered before Close: got %d, want 0", got)
	}

	// Close must flush the final partial batch and block until Run has returned.
	var closeErr error
	closeReturned := make(chan struct{})
	go func() {
		closeErr = sink.Close()
		close(closeReturned)
	}()

	select {
	case <-inFlight:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not deliver the final batch within 5s")
	}

	// The batch is on the wire and unanswered, so the drain is demonstrably
	// still running. A Close that awaits it must still be blocked.
	select {
	case <-closeReturned:
		t.Error("Close returned while the final batch was still in flight; Close did not await drain")
	default:
	}

	release()

	select {
	case <-closeReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return within 5s (drain goroutine not awaited / blocked)")
	}
	if closeErr != nil {
		t.Errorf("Close: %v", closeErr)
	}

	// Close awaits Run's return, so Run's goroutine finishes without further
	// prompting; waiting for it (rather than sampling it) keeps the assertion
	// off the scheduler.
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run goroutine did not return after Close")
	}

	if got := received.Load(); got != total {
		t.Errorf("events flushed by Close: got %d, want %d", got, total)
	}
}
