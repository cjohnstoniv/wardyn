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

	// Wait for flush interval + some slack.
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	if got := int(received.Load()); got != total {
		t.Errorf("received %d events, want %d", got, total)
	}
}

func TestWebhookSink_AuthHeader(t *testing.T) {
	t.Parallel()

	const token = "s3cr3t-b34r3r"
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	sink, err := sinks.NewWebhookSink(sinks.WebhookConfig{
		URL:           srv.URL,
		BearerToken:   token,
		BatchSize:     1,
		FlushInterval: "50ms",
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

	_ = sink.Emit(ctx, makeEvent("auth.test"))
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	want := "Bearer " + token
	if gotAuth != want {
		t.Errorf("Authorization header: got %q, want %q", gotAuth, want)
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
	time.Sleep(500 * time.Millisecond)
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

	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
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

	// Wait until the server has seen all retry attempts for the batch, then a hair
	// more so deliverWithRetry records the drop after the final failed attempt.
	deadline := time.Now().Add(2 * time.Second)
	for attempts.Load() < int32(maxRetries) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)

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
func TestWebhookSink_CloseFlushesAndAwaitsDrain(t *testing.T) {
	t.Parallel()

	var received atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		scanner := bufio.NewScanner(bytes.NewReader(body))
		for scanner.Scan() {
			if len(scanner.Bytes()) > 0 {
				received.Add(1)
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

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
	closeReturned := make(chan struct{})
	go func() {
		if cerr := sink.Close(); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
		close(closeReturned)
	}()

	select {
	case <-closeReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return within 5s (drain goroutine not awaited / blocked)")
	}

	// After Close returns, Run must have exited (Close awaits done).
	select {
	case <-runDone:
	default:
		t.Error("Run goroutine still running after Close returned; Close did not await drain")
	}

	if got := received.Load(); got != total {
		t.Errorf("events flushed by Close: got %d, want %d", got, total)
	}
}
