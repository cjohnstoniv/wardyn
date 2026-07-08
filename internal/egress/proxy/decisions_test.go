// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// TestDecisionSinkEmitCloseRace locks down E3: emit() must never send on a
// closed channel. Before the fix emit() checked closed, released the lock, then
// sent — so a concurrent close() could close the channel between the check and
// the send, panicking. The non-blocking send now runs UNDER s.mu, mutually
// exclusive with close()'s close(ch). Run under `go test -race`; pre-fix this
// panics (crashing the test binary), post-fix it passes.
func TestDecisionSinkEmitCloseRace(t *testing.T) {
	log := decisionLog(egress.Request{Host: "x.test"}, egress.Allow, "policy:allowed")
	for iter := 0; iter < 50; iter++ {
		s := &decisionSink{ch: make(chan egress.DecisionLog, 4)} // out=nil: mirror is a no-op
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for range s.ch { // drain so close() can complete
			}
		}()

		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 50; j++ {
					s.emit(log) // must never panic on send-to-closed
				}
			}()
		}
		_ = s.close(context.Background()) // races the in-flight emits
		wg.Wait()
	}
}

func TestDecisionSinkPostsAndMirrors(t *testing.T) {
	var got atomic.Int32
	var lastAuth atomic.Value
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/internal/decisions") {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		lastAuth.Store(r.Header.Get("Authorization"))
		var d egress.DecisionLog
		_ = json.NewDecoder(r.Body).Decode(&d)
		got.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer cp.Close()

	buf := &bytes.Buffer{}
	s := newDecisionSink(cp.URL, "run-tok", 16, cp.Client(), buf)

	log := decisionLog(egress.Request{RunID: uuid.New(), Host: "x.test", Method: "GET"}, egress.Allow, "policy:allowed")
	s.emit(log)

	// Stdout mirror is synchronous.
	if !strings.Contains(buf.String(), `"decision":"allow"`) {
		t.Fatalf("decision not mirrored to stdout: %q", buf.String())
	}

	// Drain async post.
	if err := s.close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got.Load() != 1 {
		t.Fatalf("posted %d decisions, want 1", got.Load())
	}
	if a, _ := lastAuth.Load().(string); a != "Bearer run-tok" {
		t.Fatalf("auth header = %q", a)
	}
}

func TestDecisionSinkDropsOnBackpressure(t *testing.T) {
	// A server that blocks forever fills the worker; emits beyond buffer drop.
	block := make(chan struct{})
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer cp.Close()
	defer close(block)

	buf := &bytes.Buffer{}
	s := newDecisionSink(cp.URL, "tok", 2, cp.Client(), buf)

	// Emit far more than the buffer; the worker is stuck on the blocking POST.
	for i := 0; i < 100; i++ {
		s.emit(decisionLog(egress.Request{Host: "x.test"}, egress.Deny, "policy:denied"))
	}
	// Give the worker a moment to pull one item.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && s.droppedCount() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if s.droppedCount() == 0 {
		t.Fatalf("expected drops under backpressure, got 0")
	}
	// All 100 should be mirrored to stdout regardless of drops.
	if n := strings.Count(buf.String(), "\n"); n != 100 {
		t.Fatalf("mirrored lines = %d, want 100", n)
	}
}

// TestDecisionSinkReportsDroppedSummary locks down FIX #18: when decisions are
// dropped on backpressure, a synthetic egress.decisions.dropped audit event must
// reach the control plane BEFORE shutdown (piggybacked on the next flush) — not
// only surface as a shutdown-time counter.
func TestDecisionSinkReportsDroppedSummary(t *testing.T) {
	var summaries atomic.Int32
	var lastSource atomic.Value
	var released atomic.Bool
	block := make(chan struct{})
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var d egress.DecisionLog
		_ = json.NewDecoder(r.Body).Decode(&d)
		if strings.HasPrefix(d.RuleSource, "egress.decisions.dropped:") {
			summaries.Add(1)
			lastSource.Store(d.RuleSource)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		// Real decision posts block until released so the buffer overflows.
		if !released.Load() {
			<-block
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer cp.Close()

	buf := &bytes.Buffer{}
	s := newDecisionSink(cp.URL, "tok", 2, cp.Client(), buf)

	for i := 0; i < 200; i++ {
		s.emit(decisionLog(egress.Request{Host: "x.test"}, egress.Deny, "policy:denied"))
	}
	// Wait for drops to accrue while the worker is wedged on the blocking POST.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && s.droppedCount() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if s.droppedCount() == 0 {
		t.Fatal("expected drops under backpressure, got 0")
	}

	// Release the worker; it should piggyback a dropped-summary on the next flush.
	released.Store(true)
	close(block)

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && summaries.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if summaries.Load() == 0 {
		t.Fatal("no egress.decisions.dropped summary posted before shutdown")
	}
	if src, _ := lastSource.Load().(string); !strings.HasPrefix(src, "egress.decisions.dropped:") {
		t.Fatalf("summary rule_source = %q, want egress.decisions.dropped:<n>", src)
	}
	_ = s.close(context.Background())
}

func TestDecisionSinkEmitAfterCloseDoesNotPanic(t *testing.T) {
	buf := &bytes.Buffer{}
	s := newDecisionSink("http://127.0.0.1:0", "tok", 4, &http.Client{Timeout: time.Second}, buf)
	if err := s.close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Must not panic on send-to-closed-channel; mirror still works.
	s.emit(decisionLog(egress.Request{Host: "x.test"}, egress.Deny, "policy:denied"))
	if !strings.Contains(buf.String(), "x.test") {
		t.Fatalf("post-close emit did not mirror")
	}
}

// TestDroppedSummary_FailedPostDoesNotBurnDelta proves the M4 fix: reportDropped
// advances *reported ONLY when the summary actually lands. A wedged control plane is
// the very cause of the drops, so a failed summary post must leave the delta pending
// (retried next tick/flush) instead of silently discarding the drop count.
func TestDroppedSummary_FailedPostDoesNotBurnDelta(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "cp wedged", http.StatusInternalServerError)
	}))
	defer down.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer up.Close()

	// post() itself must report delivery success/failure.
	sDown := newDecisionSink(down.URL, "tok", 2, down.Client(), &bytes.Buffer{})
	defer func() { _ = sDown.close(context.Background()) }()
	if err := sDown.post(droppedSummaryLog(3)); err == nil {
		t.Fatal("post to a 500 control plane must return an error")
	}
	// Failed summary => delta NOT burned.
	sDown.dropped.Store(3)
	var reported uint64
	sDown.reportDropped(&reported)
	if reported != 0 {
		t.Fatalf("failed summary must not advance reported, got %d", reported)
	}

	sUp := newDecisionSink(up.URL, "tok", 2, up.Client(), &bytes.Buffer{})
	defer func() { _ = sUp.close(context.Background()) }()
	if err := sUp.post(droppedSummaryLog(3)); err != nil {
		t.Fatalf("post to a healthy control plane must succeed, got %v", err)
	}
	// Delivered summary => delta advances.
	sUp.dropped.Store(3)
	var reported2 uint64
	sUp.reportDropped(&reported2)
	if reported2 != 3 {
		t.Fatalf("delivered summary must advance reported to 3, got %d", reported2)
	}
}
