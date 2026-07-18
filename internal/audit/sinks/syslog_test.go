// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// This file uses package sinks (white-box) so it can access unexported helpers.
package sinks

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// TestSyslogSink_Construction verifies that NewSyslogSink can connect to the
// local syslog daemon when one is available. The test is skipped gracefully on
// systems without a local syslog socket (CI containers, macOS without syslogd).
func TestSyslogSink_Construction(t *testing.T) {
	if !syslogAvailable() {
		t.Skip("local syslog socket not available on this platform")
	}

	s, err := NewSyslogSink("", "")
	if err != nil {
		t.Fatalf("NewSyslogSink (local): %v", err)
	}
	if s.Name() != "syslog" {
		t.Errorf("Name(): got %q, want %q", s.Name(), "syslog")
	}
	_ = s.Close()
}

// TestSyslogSink_Emit verifies that Emit writes an event without error when
// local syslog is available.
func TestSyslogSink_Emit(t *testing.T) {
	if !syslogAvailable() {
		t.Skip("local syslog socket not available on this platform")
	}

	s, err := NewSyslogSink("", "")
	if err != nil {
		t.Fatalf("NewSyslogSink: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ev := types.AuditEvent{
		ID:        uuid.New(),
		Time:      time.Now().UTC(),
		ActorType: types.ActorSystem,
		Actor:     "test",
		Action:    "syslog.test",
		Outcome:   "success",
	}
	if err := s.Emit(context.Background(), ev); err != nil {
		t.Errorf("Emit: %v", err)
	}
}

// TestSyslogSink_RemoteEmitDoesNotBlockOnHungCollector is the regression test
// for the HIGH finding: a remote (tcp) syslog collector that accepts the
// connection but never reads must NOT be able to block the Emit caller. Emit is
// reached synchronously from request handlers via Fanout.Emit, so blocking here
// stalls the API request path.
//
// Setup: a TCP listener that accepts the dial then never reads from the socket.
// The background writer's first write may succeed into the kernel send buffer,
// but once that fills (or the write times out) the bounded queue backs up. The
// test floods Emit with far more events than the buffer can hold and asserts
// that every Emit returns within a tight bound and that the drop/timeout
// counter advances — proving the caller is never parked on the hung collector.
//
// RED-FIRST: against the previous implementation Emit called s.w.Info directly
// (no buffer, no timeout), so once the collector's socket buffer filled, Emit
// would block on the TCP write and this test would hang (and never increment a
// drop counter).
func TestSyslogSink_RemoteEmitDoesNotBlockOnHungCollector(t *testing.T) {
	// Listener that accepts connections but never reads — simulates a hung
	// remote collector. We hold the accepted conns open (without reading) so the
	// peer's send buffer eventually fills and writes block.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	stallDone := make(chan struct{})
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			// Never read; just hold the connection open until the test ends.
			go func(c net.Conn) {
				<-stallDone
				_ = c.Close()
			}(conn)
		}
	}()
	t.Cleanup(func() { close(stallDone) })

	s, err := NewSyslogSink("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("NewSyslogSink (tcp, hung collector): %v", err)
	}
	t.Cleanup(func() {
		// Close in a goroutine with a timeout so a wedged drain can't hang the
		// test process; Drops() is already asserted below.
		closed := make(chan struct{})
		go func() { _ = s.Close(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
		}
	})

	// Flood Emit with many more events than the bounded buffer can hold while
	// the background writer is stalled on the hung collector. Each Emit must
	// return promptly (non-blocking enqueue / drop on overflow).
	const events = syslogBufferSize * 4
	perEmitBudget := 200 * time.Millisecond
	overall := time.Now()
	for i := 0; i < events; i++ {
		start := time.Now()
		if err := s.Emit(context.Background(), makeSyslogEvent("syslog.hung")); err != nil {
			t.Fatalf("Emit returned error: %v", err)
		}
		if elapsed := time.Since(start); elapsed > perEmitBudget {
			t.Fatalf("Emit blocked for %s on hung collector (want < %s): request path not bounded",
				elapsed, perEmitBudget)
		}
	}
	if total := time.Since(overall); total > 10*time.Second {
		t.Fatalf("flooding %d Emits took %s; caller is being blocked by hung collector", events, total)
	}

	// With a hung collector and an overflowing buffer, events must be dropped or
	// time out and counted — never silently discarded and never blocking.
	deadline := time.Now().Add(3 * time.Second)
	for s.Drops() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if drops := s.Drops(); drops == 0 {
		t.Errorf("expected non-zero drop/timeout counter with hung collector; got 0")
	}
}

// TestSyslogSink_RemoteWriteTimeoutCounts verifies the single-write timeout
// path: even with a generous buffer, a write that the collector never drains
// must be bounded by syslogWriteTimeout and counted as a drop, so the writer
// cannot wedge permanently on one TCP write.
func TestSyslogSink_RemoteWriteTimeoutCounts(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	stallDone := make(chan struct{})
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				<-stallDone
				_ = c.Close()
			}(conn)
		}
	}()
	t.Cleanup(func() { close(stallDone) })

	s, err := NewSyslogSink("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("NewSyslogSink: %v", err)
	}
	t.Cleanup(func() {
		closed := make(chan struct{})
		go func() { _ = s.Close(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
		}
	})

	// Send enough events to fill the kernel send buffer and force at least one
	// write to block past syslogWriteTimeout, which must be counted.
	for i := 0; i < syslogBufferSize*4; i++ {
		_ = s.Emit(context.Background(), makeSyslogEvent("syslog.timeout"))
	}

	// The write timeout is syslogWriteTimeout; wait a bit longer than that for a
	// timeout to register.
	deadline := time.Now().Add(syslogWriteTimeout + 3*time.Second)
	for s.Drops() == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if s.Drops() == 0 {
		t.Errorf("expected drops from overflow/write-timeout on hung collector; got 0")
	}
}

// wedgedWriter is a syslogWriter whose Info blocks until released, simulating a
// wedged local syslog daemon / a full /dev/log datagram peer buffer.
type wedgedWriter struct{ release chan struct{} }

func (w *wedgedWriter) Info(string) error { <-w.release; return nil }
func (w *wedgedWriter) Close() error      { return nil }

// TestSyslogSink_LocalSocketEmitDoesNotBlockOnWedgedDaemon is the 
// regression: the LOCAL-socket path (Network=="") must not block Emit when the
// syslog daemon wedges. Emit is reached synchronously from request handlers via
// Fanout.Emit, so a blocked local write stalls the API request path (and the
// kill cascade) even after the PG write already succeeded.
//
// RED-FIRST: against the previous implementation the local path called
// s.w.Info directly with no buffer/timeout, so once the daemon wedged Emit would
// block on the write and this test would hang. Routing the local socket through
// the same bounded async buffer as the remote path fixes it.
func TestSyslogSink_LocalSocketEmitDoesNotBlockOnWedgedDaemon(t *testing.T) {
	ww := &wedgedWriter{release: make(chan struct{})}
	// Network=="" is the local /dev/log transport — the path that was synchronous.
	s := newSyslogSinkWith(ww, "", "")
	t.Cleanup(func() {
		close(ww.release) // unblock parked write goroutines so drain/Close is fast
		done := make(chan struct{})
		go func() { _ = s.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})

	const events = syslogBufferSize * 2
	perEmitBudget := 200 * time.Millisecond
	for i := 0; i < events; i++ {
		start := time.Now()
		if err := s.Emit(context.Background(), makeSyslogEvent("syslog.local.wedged")); err != nil {
			t.Fatalf("Emit returned error: %v", err)
		}
		if elapsed := time.Since(start); elapsed > perEmitBudget {
			t.Fatalf("local-socket Emit blocked for %s on wedged daemon (want < %s)", elapsed, perEmitBudget)
		}
	}

	// The wedged write parks the writer; the bounded buffer fills and overflow is
	// dropped + counted — never silently discarded, never blocking the caller.
	deadline := time.Now().Add(syslogWriteTimeout + 3*time.Second)
	for s.Drops() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if s.Drops() == 0 {
		t.Errorf("expected non-zero drop counter with a wedged local daemon; got 0")
	}
}

// syslogPadActor is an oversized Actor value (~4 KiB) used purely to grow each
// serialised event so the collector's TCP send buffer fills quickly. Without a
// large payload the kernel send buffer can absorb thousands of small writes
// before a synchronous write blocks; with it, the unbounded (vulnerable) Emit
// blocks within a couple of writes — which is what makes the "does not hang"
// assertion below a true RED-first signal.
var syslogPadActor = func() string {
	b := make([]byte, 4096)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}()

// makeSyslogEvent returns an AuditEvent with a unique ID and a large payload so
// that a stalled collector blocks writes deterministically.
func makeSyslogEvent(action string) types.AuditEvent {
	return types.AuditEvent{
		ID:        uuid.New(),
		Time:      time.Now().UTC(),
		ActorType: types.ActorSystem,
		Actor:     syslogPadActor,
		Action:    action,
		Outcome:   "success",
	}
}
