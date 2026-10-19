// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A queue left at shutdown is delivered in BatchSize requests, not as one POST
// a collector with a body cap would refuse whole (#2313).
func TestWebhookSink_DrainFlushesBatchSizeAtATime(t *testing.T) {
	var mu sync.Mutex
	var sizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		sizes = append(sizes, bytes.Count(body, []byte("\n")))
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	w, err := NewWebhookSink(WebhookConfig{URL: srv.URL, BatchSize: 3, FlushInterval: "1h", BufferSize: 16, MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	for range 7 {
		_ = w.Emit(context.Background(), types.AuditEvent{ID: uuid.New(), Action: "drain.test"})
	}
	batch := []types.AuditEvent{}
	w.drainAndFlush(&batch, func(c context.Context) {
		if len(batch) > 0 {
			w.deliverWithRetry(c, batch)
			batch = batch[:0]
		}
	})
	mu.Lock()
	defer mu.Unlock()
	if len(sizes) != 3 || sizes[0] != 3 || sizes[1] != 3 || sizes[2] != 1 {
		t.Fatalf("request sizes = %v, want [3 3 1]", sizes)
	}
}
