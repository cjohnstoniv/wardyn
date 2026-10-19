// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/semaphore"

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

// scanTestProxy is a proxy that inspects every forwarded body.
func scanTestProxy(t *testing.T) *Proxy {
	t.Helper()
	return newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(types.RunPolicySpec{AllowAllEgress: true}),
		Sink:     &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 8)},
		Scanner:  forwardScanEngine(t, "alert"),
		Resolver: publicResolver{},
	})
}

var noopScanEmit = func(egress.Decision, string, *egress.ScanSummary) {}

// scanAsync runs one scanBufferedBody of body on p and closes the returned
// channel once it has returned and released its buffer.
func scanAsync(p *Proxy, url string, body io.Reader) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, url, body)
		_, _, release, _ := p.scanBufferedBody(httptest.NewRecorder(), req, "", contentscan.ChannelGeneric, "read body", noopScanEmit)
		release()
	}()
	return done
}

// A body is read before the scan slot is taken: while one body is parked
// mid-read, as a slow-loris POST from the sandbox does, another request of
// the same engine is read, scanned and returned.
func TestScanBufferedBodyReadsBeforeTheSlot(t *testing.T) {
	p := scanTestProxy(t)
	held := &blockingBody{entered: make(chan struct{}, 1), release: make(chan struct{})}
	firstDone := scanAsync(p, "http://connector.test/slow", held)
	defer func() {
		close(held.release)
		<-firstDone
	}()
	select {
	case <-held.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first body never reached the read")
	}
	select {
	case <-scanAsync(p, "http://connector.test/b", strings.NewReader(`{"payload":"small"}`)):
	case <-time.After(5 * time.Second):
		t.Fatal("a second body waited behind a first one still being read: a slow body holds the scan slot")
	}
}

// TestScanBufferedBodyBoundsConcurrentBuffering: the number of bodies
// extracted at once has to be bounded, because the extractor's live-heap
// amplification (~5.3x, measured, and independent of the detector set)
// against a 32 MiB per-body cap does not fit twice inside the proxy sidecar's
// hard 256 MiB cgroup ceiling: unbounded, one in-cap 30 MiB body peaks at ~158
// MiB of live heap and two concurrent at ~274 MiB — over the cap, and an
// OOM-killed proxy sidecar takes the run's only network path with it. The
// agent inside the sandbox chooses both the sizes and the concurrency.
//
// Asserted structurally rather than by sampling the heap, so it cannot flake:
// while the engine's one slot is held, a read body must not be scanned, and
// it is once the slot frees.
func TestScanBufferedBodyBoundsConcurrentBuffering(t *testing.T) {
	p := scanTestProxy(t)
	if cap(p.scan.init().slots) != maxConcurrentScans || maxConcurrentScans != 1 {
		t.Fatalf("an engine admits %d scans at once, want 1", cap(p.scan.slots))
	}
	p.scan.slots <- struct{}{} // another scan of this engine is in progress
	done := scanAsync(p, "http://connector.test/b", strings.NewReader(`{"payload":"small"}`))
	select {
	case <-done:
		<-p.scan.slots
		t.Fatal("a body was scanned while the engine's one scan slot was held")
	case <-time.After(300 * time.Millisecond):
	}
	<-p.scan.slots
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a queued scan never completed after the slot was released — waiting must not deadlock")
	}
}

// Each engine has its own slot and byte budget: with one engine's slot held,
// as a via_org engine inside wardynd would hold it for one run, another
// engine's request is scanned at once.
func TestScanBudgetIsPerEngine(t *testing.T) {
	busy, other := scanTestProxy(t), scanTestProxy(t)
	busy.scan.init().slots <- struct{}{}
	defer func() { <-busy.scan.slots }()
	select {
	case <-scanAsync(other, "http://connector.test/b", strings.NewReader(`{"payload":"small"}`)):
	case <-time.After(5 * time.Second):
		t.Fatal("one engine's held scan slot held back another engine's scan")
	}
}

// TestScanSlotWaitIsContextBoundAndFailsClosed pins the OTHER half of the
// bound: the slot maxConcurrentScans hands out has to be taken through a wait
// that ENDS, and ending has to mean Deny. The agent-facing listener sets
// ReadTimeout 0 (streaming bodies and CONNECT tunnels need it), so nothing
// above this call bounds how long a scan holding the slot takes.
//
// Bounded, the wait must fail CLOSED (a Deny + an error response), like the
// read-error arm beside it: a request that could not be inspected is never
// forwarded unscanned. Driven with an already-cancelled request context, which
// is the client-went-away half of the same arm and needs no wall-clock sleep.
func TestScanSlotWaitIsContextBoundAndFailsClosed(t *testing.T) {
	p := scanTestProxy(t)
	p.scan.init().slots <- struct{}{}
	defer func() { <-p.scan.slots }()

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
		var release func()
		_, _, release, o.blocked = p.scanBufferedBody(rec, req, "", contentscan.ChannelGeneric, "read body",
			func(d egress.Decision, _ string, _ *egress.ScanSummary) { o.decision, o.emitted = d, true })
		release()
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
			t.Fatalf("decision emitted for the abandoned wait = %q (emitted=%v), want %q", o.decision, o.emitted, egress.Deny)
		}
		if rec.Code != http.StatusBadGateway {
			t.Errorf("status = %d, want %d for a scan slot the request could not wait for", rec.Code, http.StatusBadGateway)
		}
		if !retainedBudgetEmpty(p) {
			t.Error("a refused request left its bytes charged")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a queued request whose context was already cancelled is STILL blocked in the scan-slot wait")
	}
}

// TestScanBufferedBodyHoldsTheBufferBudgetUntilRelease: the byte bound has to
// cover the buffer's LIFETIME, not just the window in which it is extracted
// and scanned. A caller still holds the whole buffered body after
// scanBufferedBody returns, streaming it through RoundTrip, so N requests
// stalled on a slow upstream would otherwise each retain up to maxLLMScanBody.
//
// Two claims, both asserted here: the bytes are still charged after the call
// returns, and a request that cannot be charged fails CLOSED (Deny + 502) like
// every other arm of this function.
func TestScanBufferedBodyHoldsTheBufferBudgetUntilRelease(t *testing.T) {
	p := scanTestProxy(t)
	body := `{"payload":"a body whose bytes stay live until the caller is done with them"}`

	shrinkRetainedBudget(t, p, len(body)) // room for exactly ONE of these at a time

	req := httptest.NewRequest(http.MethodPost, "http://connector.test/a", strings.NewReader(body))
	reader, _, release, blocked := p.scanBufferedBody(httptest.NewRecorder(), req,
		"", contentscan.ChannelGeneric, "read body", noopScanEmit)
	if blocked || reader == nil {
		t.Fatalf("a clean in-cap body must be returned for forwarding (blocked=%v reader=%v)", blocked, reader)
	}
	if p.scan.retained.TryAcquire(1) {
		t.Fatalf("the %d-byte budget has room after scanBufferedBody returned — the buffer is still "+
			"reachable (the caller has not streamed it upstream yet), so all %d bytes must still be charged", len(body), len(body))
	}

	// While it is charged, a second body that would exceed the budget cannot be
	// admitted — and is refused, never forwarded unscanned.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	var denied bool
	req2 := httptest.NewRequest(http.MethodPost, "http://connector.test/b",
		strings.NewReader(body)).WithContext(ctx)
	_, _, release2, blocked2 := p.scanBufferedBody(rec, req2, "", contentscan.ChannelGeneric, "read body",
		func(d egress.Decision, _ string, _ *egress.ScanSummary) { denied = d == egress.Deny })
	release2()
	if !blocked2 || !denied || rec.Code != http.StatusBadGateway {
		t.Fatalf("a body that could not be charged to the retained-bytes budget was NOT refused "+
			"(blocked=%v denied=%v status=%d): the budget wait fails CLOSED, exactly as the scan-slot "+
			"wait does", blocked2, denied, rec.Code)
	}

	// Released, the budget is free again and the next body proceeds.
	release()
	if !retainedBudgetEmpty(p) {
		t.Fatal("bytes still charged after release(), want 0")
	}
	req3 := httptest.NewRequest(http.MethodPost, "http://connector.test/c", strings.NewReader(body))
	_, _, release3, blocked3 := p.scanBufferedBody(httptest.NewRecorder(), req3,
		"", contentscan.ChannelGeneric, "read body", noopScanEmit)
	release3()
	if blocked3 {
		t.Fatal("a body must be admitted once the previous one released its bytes — the budget bounds " +
			"memory, it must not become a one-shot")
	}
}

// shrinkRetainedBudget gives p's engine a budget of n bytes, so the lifetime
// claims can be exercised without allocating tens of MiB.
func shrinkRetainedBudget(t *testing.T, p *Proxy, n int) {
	t.Helper()
	p.scan.init()
	p.scan.limit, p.scan.retained = int64(n), semaphore.NewWeighted(int64(n))
}

// retainedBudgetEmpty reports whether nothing is charged to p's budget.
func retainedBudgetEmpty(p *Proxy) bool {
	b := p.scan.init()
	if !b.retained.TryAcquire(b.limit) {
		return false
	}
	b.retained.Release(b.limit)
	return true
}

// TestScanBudgetReadBodyChargesWaitsAndAdmitsOversize pins the byte budget's
// behaviours: a body is charged before it is read, a charge that does not fit
// waits for a release, the wait is bounded by ctx and fails closed, what the
// read did not use is handed back, and a body larger than the whole budget is
// admitted once the budget is empty instead of waiting forever. A charge with
// room succeeds under an expired ctx — the ctx bounds the wait only.
func TestScanBudgetReadBodyChargesWaitsAndAdmitsOversize(t *testing.T) {
	p := &Proxy{}
	shrinkRetainedBudget(t, p, 100)
	b := &p.scan
	sized := func(n int) io.Reader { return strings.NewReader(strings.Repeat("x", n)) }

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	_, first, err := b.readBody(expired, sized(60), 60, 1000)
	if err != nil {
		t.Fatalf("a charge with room was refused because its ctx had expired: %v", err)
	}
	if _, _, err := b.readBody(expired, sized(60), 60, 1000); !errors.Is(err, errScanBusy) {
		t.Fatalf("a charge that does not fit = %v, want errScanBusy", err)
	}

	got := make(chan error, 1)
	var waiter func()
	go func() {
		var err error
		_, waiter, err = b.readBody(context.Background(), sized(60), 60, 1000)
		got <- err
	}()
	select {
	case <-got:
		t.Fatal("a charge that does not fit returned before any bytes were released")
	case <-time.After(50 * time.Millisecond):
	}
	first()
	first() // idempotent: a second call must not over-credit the budget
	if err := <-got; err != nil {
		t.Fatalf("a waiting charge was not admitted after the bytes it waited on were released: %v", err)
	}
	if b.retained.TryAcquire(41) {
		t.Fatal("release() credited the budget twice")
	}

	// 60 bytes are still charged: an oversize body waits for them, then fits.
	short, cancelShort := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelShort()
	if _, _, err := b.readBody(short, sized(500), 500, 1000); !errors.Is(err, errScanBusy) {
		t.Fatalf("an oversize body beside bytes already charged = %v, want errScanBusy", err)
	}
	waiter()
	buf, release, err := b.readBody(context.Background(), sized(500), 500, 1000)
	if err != nil || len(buf) != 500 {
		t.Fatalf("an oversize body on an empty budget = %d bytes, %v; want all 500 read", len(buf), err)
	}
	release()

	// An undeclared length is charged at the ceiling, then trimmed to the read.
	buf, release, err = b.readBody(context.Background(), sized(10), -1, 1000)
	if err != nil || len(buf) != 10 {
		t.Fatalf("an undeclared 10-byte body = %d bytes, %v", len(buf), err)
	}
	if !b.retained.TryAcquire(90) {
		t.Fatal("an undeclared body kept more than the bytes it read charged")
	}
	b.retained.Release(90)
	release()
	if !retainedBudgetEmpty(p) {
		t.Fatal("bytes still charged after every release")
	}
}
