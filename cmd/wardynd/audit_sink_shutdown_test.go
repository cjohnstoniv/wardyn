// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestAuditFanoutSurvivesRootCtxCancellation pins B6-F3 (= B12b-F1).
//
// SIGTERM cancels rootCtx. The webhook sink's Run loop rode that context, so it
// drained and RETURNED before httpSrv.Shutdown had finished — up to 15 seconds
// of in-flight handlers still emitting audit events, plus
// FlushAuthFailedStreak's summary row, all enqueued into a 4096-slot buffer
// with no reader left. fan.Close() then found `done` already closed and
// returned immediately, so those events were never delivered to the SIEM and
// the drop was not even counted. Postgres (the system of record) still got the
// rows — this is SIEM-delivery loss at shutdown, which is exactly the window an
// operator investigating a shutdown cares about.
//
// The flusher now runs on context.WithoutCancel(rootCtx) and stops on
// Fanout.Close → WebhookSink.Close, which drains and is bounded by the client
// timeout.
func TestAuditFanoutSurvivesRootCtxCancellation(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfgJSON := fmt.Sprintf(`{"webhook":{"url":%q,"batch_size":1,"flush_interval":"10ms"}}`, srv.URL)
	rootCtx, cancel := context.WithCancel(context.Background())
	fan, err := buildAuditFanout(rootCtx, cfgJSON)
	if err != nil {
		t.Fatalf("buildAuditFanout: %v", err)
	}
	if fan == nil {
		t.Fatal("no fanout built from a one-sink config")
	}

	// SIGTERM: rootCtx dies while the HTTP server is still shutting down.
	cancel()

	// A handler that was still in flight emits. Before the fix this landed in a
	// buffer nobody was reading.
	ev := types.AuditEvent{
		ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem,
		Actor: "wardyn/test", Action: "auth.failed", Target: "/api/v1/runs", Outcome: "failure",
		Data: json.RawMessage(`{"reason":"in_flight_at_shutdown"}`),
	}
	if eerr := fan.Emit(context.Background(), ev); eerr != nil {
		t.Fatalf("emit: %v", eerr)
	}

	if cerr := fan.Close(); cerr != nil {
		t.Fatalf("close: %v", cerr)
	}

	mu.Lock()
	defer mu.Unlock()
	var delivered bool
	for _, b := range seen {
		if len(b) > 0 && containsID(b, ev.ID.String()) {
			delivered = true
		}
	}
	if !delivered {
		t.Fatalf("the collector never saw the event emitted after rootCtx was cancelled — "+
			"the flusher stopped before the server did; received %d batch(es)", len(seen))
	}
}

func containsID(body, id string) bool {
	for i := 0; i+len(id) <= len(body); i++ {
		if body[i:i+len(id)] == id {
			return true
		}
	}
	return false
}

// TestAuditFanoutCloseIsBoundedAgainstAWedgedCollector is B6-F3's negative
// control: running the flusher past rootCtx must not turn shutdown into a hang.
// Close drains under the sink's own bounded context, so a collector that never
// answers costs the client timeout and no more.
func TestAuditFanoutCloseIsBoundedAgainstAWedgedCollector(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-block
		w.WriteHeader(http.StatusOK)
	}))
	// LIFO: release the wedged handler BEFORE httptest.Server.Close waits on it,
	// or the cleanup deadlocks on the very thing this test wedges on purpose.
	defer srv.Close()
	defer close(block)

	cfgJSON := fmt.Sprintf(`{"webhook":{"url":%q,"batch_size":1,"flush_interval":"10ms","max_retries":0}}`, srv.URL)
	rootCtx, cancel := context.WithCancel(context.Background())
	fan, err := buildAuditFanout(rootCtx, cfgJSON)
	if err != nil {
		t.Fatalf("buildAuditFanout: %v", err)
	}
	cancel()
	_ = fan.Emit(context.Background(), types.AuditEvent{
		ID: uuid.New(), Time: time.Now().UTC(), Action: "auth.failed", Outcome: "failure",
	})

	done := make(chan error, 1)
	go func() { done <- fan.Close() }()
	select {
	case <-done:
	// The bound is the sink's own client.Timeout (15s) for the one in-flight
	// delivery attempt, plus slack. What must NOT happen is an unbounded wait:
	// running the flusher past rootCtx would be a bad trade if it turned
	// shutdown into a hang.
	case <-time.After(45 * time.Second):
		t.Fatal("fan.Close() did not return against a wedged collector — shutdown would hang")
	}
}
