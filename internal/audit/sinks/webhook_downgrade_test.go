// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A 307 from an https collector to a plain-http one keeps the method and body, but following it would send
// the batch in cleartext: the redirect is not followed and the batch counts as a failed delivery.
func TestWebhookSink_RedirectDowngradeToHTTPIsNotFollowed(t *testing.T) {
	t.Parallel()
	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(plain.Close)
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/final", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(secure.Close)

	sink, err := NewWebhookSink(WebhookConfig{
		URL: secure.URL, BatchSize: 1, FlushInterval: "30ms", BufferSize: 16, MaxRetries: 2, RetryBaseDelay: "1ms",
	})
	if err != nil {
		t.Fatalf("NewWebhookSink: %v", err)
	}
	// Only the transport is swapped, to trust the test certificate; the redirect policy is the constructor's.
	client := *secure.Client()
	client.Timeout = sink.client.Timeout
	client.CheckRedirect = sink.client.CheckRedirect
	sink.client = &client

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sink.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	if err := sink.Emit(ctx, types.AuditEvent{Action: "downgrade.test"}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for sink.Drops() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if d := sink.Drops(); d != 1 {
		t.Errorf("drops = %d, want 1: the batch must count as a failed delivery", d)
	}
	if n := plainHits.Load(); n != 0 {
		t.Errorf("the plain-http target got %d requests, want none", n)
	}
}
