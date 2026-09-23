// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/store"
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

// ─── R-06: the masking recorder's half of B6-F1 ──────────────────────────────

// capturingRecorder keeps whatever the chain hands it.
type capturingRecorder struct {
	mu  sync.Mutex
	got []types.AuditEvent
}

func (c *capturingRecorder) Record(_ context.Context, ev types.AuditEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, ev)
	return nil
}

// TestMaskingRecorderCapsTheTarget pins the OUTER half of B6-F1. The cap is
// placed twice on purpose: store.InsertAuditEvent bounds the DATABASE, and this
// recorder — which is outermost in the chain — is what bounds the SPOOL and the
// SIEM SINKS. Nothing exercised it, so deleting that one line left every gate
// green while a member's 1 MiB path went on reaching the collector.
func TestMaskingRecorderCapsTheTarget(t *testing.T) {
	inner := &capturingRecorder{}
	// reg nil is the documented safe no-op for masking; the cap must still run.
	rec := maskingRecorder{inner: inner}

	long := "/api/v1/policies/" + strings.Repeat("z", 64*1024)
	if err := rec.Record(context.Background(), types.AuditEvent{
		ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorHuman,
		Actor: "sub-member", Action: "authz.denied", Target: long, Outcome: "denied",
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if len(inner.got) != 1 {
		t.Fatalf("inner recorder saw %d events, want 1", len(inner.got))
	}
	got := inner.got[0].Target
	if len(got) > store.MaxAuditTargetLen+len(store.AuditTargetTruncatedMarker) {
		t.Fatalf("the spool and every SIEM sink still receive a %d-byte target", len(got))
	}
	if !strings.HasSuffix(got, store.AuditTargetTruncatedMarker) {
		t.Errorf("a truncated target must say so; got the tail %q", got[max(0, len(got)-40):])
	}

	// NEGATIVE CONTROL: an ordinary target passes through byte-identical, or
	// every audit query an operator wrote against this column stops matching.
	const ordinary = "/api/v1/runs/6f1c9d4e-0000-4000-8000-000000000001"
	if err := rec.Record(context.Background(), types.AuditEvent{
		ID: uuid.New(), Time: time.Now().UTC(), Action: "run.create", Target: ordinary, Outcome: "success",
	}); err != nil {
		t.Fatalf("record ordinary: %v", err)
	}
	if inner.got[1].Target != ordinary {
		t.Errorf("an ordinary target was rewritten: %q", inner.got[1].Target)
	}
}

// ─── R-05: the serve-error exit drains the sinks too ─────────────────────────

// TestServeAndShutdownDrainsSinksOnAServeError pins the half of B6-F3 that
// Appendix A added by name ("add the `errCh` serve-error `fan.Close()` gap").
// The tail that closed the fanout ran only after the SIGNAL path; a
// ListenAndServe error returned above it, so whatever the webhook batcher still
// held went to the garbage collector instead of the SIEM — on precisely the
// exit an operator is most likely to be investigating.
//
// Driven the honest way: bind the port first so ListenAndServe fails
// immediately, emit before calling in, and assert the collector saw it.
func TestServeAndShutdownDrainsSinksOnAServeError(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	// Hold the address so the daemon's own ListenAndServe returns at once.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer busy.Close()
	addr := busy.Addr().String()

	// A long flush interval and a big batch: only Close can deliver this event,
	// so the test cannot pass on a timer that happened to fire.
	cfgJSON := fmt.Sprintf(`{"webhook":{"url":%q,"batch_size":100,"flush_interval":"10m"}}`, collector.URL)
	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fan, err := buildAuditFanout(rootCtx, cfgJSON)
	if err != nil {
		t.Fatalf("buildAuditFanout: %v", err)
	}

	ev := types.AuditEvent{
		ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem,
		Actor: "wardyn/test", Action: "auth.failed", Target: "/api/v1/runs", Outcome: "failure",
	}
	if eerr := fan.Emit(context.Background(), ev); eerr != nil {
		t.Fatalf("emit: %v", eerr)
	}

	empty, no := "", false
	trust := "wardyn.local"
	f := &bootFlags{
		listen: &addr, tlsCert: &empty, tlsKey: &empty,
		tlsTerminated: &no, trustDomain: &trust,
	}
	srv := api.New(api.Config{})
	if serr := serveAndShutdown(rootCtx, f, tlsPosture{}, srv, "none", fan, nil); serr == nil {
		t.Fatal("serveAndShutdown returned nil against an address already in use")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, b := range seen {
		if containsID(b, ev.ID.String()) {
			return
		}
	}
	t.Fatalf("the collector never saw the event: the serve-error return skipped the audit-sink drain "+
		"(received %d batch(es))", len(seen))
}
