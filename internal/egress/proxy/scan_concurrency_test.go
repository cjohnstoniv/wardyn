// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// blockingBody is a request body whose first Read parks until release is
// closed, so a test can hold one scan inside scanBufferedBody and observe
// whether a second one proceeds beside it.
type blockingBody struct {
	entered chan struct{}
	release chan struct{}
	done    bool
}

func (b *blockingBody) Read(p []byte) (int, error) {
	if !b.done {
		select {
		case b.entered <- struct{}{}:
		default:
		}
		<-b.release
		b.done = true
		return copy(p, []byte(`{"payload":"ok"}`)), nil
	}
	return 0, io.EOF
}

// TestScanBufferedBodyBoundsConcurrentBuffering pins F074: the number of request
// bodies buffered+extracted AT ONCE has to be bounded, because the extractor's
// live-heap amplification (~5.3x, measured, and independent of the detector set)
// against a 32 MiB per-body cap does not fit twice inside the proxy sidecar's
// hard 256 MiB cgroup ceiling. Measured before the fix: one in-cap 30 MiB body
// peaked at ~158 MiB of live heap, two concurrent at ~274 MiB — over the cap,
// and an OOM-killed proxy sidecar takes the run's only network path with it. The
// agent inside the sandbox chooses both the sizes and the concurrency, and
// nothing else in internal/egress/proxy limits either (no LimitListener, no
// semaphore, no rate limiter; the servers set timeouts only).
//
// Asserted structurally rather than by sampling the heap, so it cannot flake:
// while one body is parked inside the scan path, a second must not get in.
func TestScanBufferedBodyBoundsConcurrentBuffering(t *testing.T) {
	p := newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(types.RunPolicySpec{AllowAllEgress: true}),
		Sink:     &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 8)},
		Scanner:  forwardScanEngine(t, "alert"),
		Resolver: publicResolver{},
	})
	noop := func(egress.Decision, string, *egress.ScanSummary) {}

	held := &blockingBody{entered: make(chan struct{}, 1), release: make(chan struct{})}
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		req := httptest.NewRequest(http.MethodPost, "http://connector.test/a", held)
		p.scanBufferedBody(httptest.NewRecorder(), req, contentscan.ChannelGeneric, "read body", noop)
	}()

	select {
	case <-held.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first body never reached the scan path")
	}

	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		req := httptest.NewRequest(http.MethodPost, "http://connector.test/b",
			strings.NewReader(`{"payload":"small"}`))
		p.scanBufferedBody(httptest.NewRecorder(), req, contentscan.ChannelGeneric, "read body", noop)
	}()

	select {
	case <-secondDone:
		close(held.release)
		<-firstDone
		t.Fatal("a second body was buffered and scanned while the first was still parked inside the " +
			"scan path: concurrent inspection is unbounded, so the sandbox — which chooses both the " +
			"body sizes and the concurrency — sets the proxy sidecar's peak heap against a hard cgroup cap")
	case <-time.After(300 * time.Millisecond):
	}

	close(held.release)
	for _, done := range []chan struct{}{firstDone, secondDone} {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("a queued scan never completed after the slot was released — waiting must not deadlock")
		}
	}
}

// TestScanSlotWaitIsContextBoundAndFailsClosed pins the OTHER half of F074's
// bound: the slot maxConcurrentScans hands out has to be taken through a wait
// that ENDS, and ending has to mean Deny.
//
// maxConcurrentScans is 1 and the slot is held across io.ReadAll of a
// SANDBOX-controlled body, and nothing above this call bounds that read: the
// agent-facing listener that serves handlePlain and /wardyn/llm/* sets
// ReadTimeout 0 (streaming bodies and CONNECT tunnels need it — NewServer,
// internal/egress/proxy/server.go); only the inner MITM server carries one. So a
// single slow-loris POST holding the process-wide slot would park every other
// inspected request of the run in the semaphore send FOREVER — each retaining a
// goroutine and a socket in a 256 MiB sidecar, the same retention class F079 is
// about, now reachable through the inspection path and triggerable by the
// untrusted sandbox.
//
// Bounded, the wait must fail CLOSED (a Deny + an error response), like the
// read-error arm above it: a request that could not be inspected is never
// forwarded unscanned. Driven with an already-cancelled request context, which
// is the client-went-away half of the same arm and needs no wall-clock sleep.
func TestScanSlotWaitIsContextBoundAndFailsClosed(t *testing.T) {
	p := newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(types.RunPolicySpec{AllowAllEgress: true}),
		Sink:     &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 8)},
		Scanner:  forwardScanEngine(t, "alert"),
		Resolver: publicResolver{},
	})
	noop := func(egress.Decision, string, *egress.ScanSummary) {}

	// Hold the single slot with a body parked mid-read, exactly as a slow-loris
	// POST from the sandbox does.
	held := &blockingBody{entered: make(chan struct{}, 1), release: make(chan struct{})}
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		req := httptest.NewRequest(http.MethodPost, "http://connector.test/slow", held)
		p.scanBufferedBody(httptest.NewRecorder(), req, contentscan.ChannelGeneric, "read body", noop)
	}()
	select {
	case <-held.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first body never reached the scan path")
	}
	defer func() {
		close(held.release)
		<-firstDone
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "http://connector.test/queued",
		strings.NewReader(`{"payload":"queued"}`)).WithContext(ctx)
	rec := httptest.NewRecorder()

	type outcome struct {
		blocked  bool
		decision egress.Decision
		emitted  bool
	}
	got := make(chan outcome, 1)
	go func() {
		var o outcome
		_, _, o.blocked = p.scanBufferedBody(rec, req, contentscan.ChannelGeneric, "read body",
			func(d egress.Decision, _ string, _ *egress.ScanSummary) { o.decision, o.emitted = d, true })
		got <- o
	}()

	select {
	case o := <-got:
		if !o.blocked {
			t.Fatalf("a queued request whose context was already cancelled was NOT refused "+
				"(blocked=%v, status=%d, body=%q): the scan-slot wait must fail CLOSED, or a "+
				"body that could not be inspected rides out unscanned", o.blocked, rec.Code, rec.Body.String())
		}
		if !o.emitted || o.decision != egress.Deny {
			t.Fatalf("decision emitted for the abandoned wait = %q (emitted=%v), want %q — "+
				"the read-error arm beside it denies, and so must this one",
				o.decision, o.emitted, egress.Deny)
		}
		if rec.Code != http.StatusBadGateway {
			t.Errorf("status = %d, want %d for a scan slot the request could not wait for",
				rec.Code, http.StatusBadGateway)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a queued request whose context was already cancelled is STILL blocked in the " +
			"scan-slot send: the wait is neither context-aware nor deadlined, so one slow-loris " +
			"POST from the sandbox parks every other inspected request of the run indefinitely, " +
			"each holding a goroutine and a socket")
	}
}
