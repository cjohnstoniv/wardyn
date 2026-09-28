// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package sinks provides production audit.Sink implementations (syslog, webhook, file) plus a Fanout
// multiplexer and config wiring. Stdlib-only: no third-party logging or HTTP clients.
//
// Constraint: sinks must never block the caller's goroutine beyond a buffered channel send or a local
// syscall. Overflow increments and logs a drop counter; events are never silently discarded.
package sinks

import (
	"context"
	"fmt"
	"log/slog"
	"log/syslog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// syslogBufferSize: log/syslog.Writer exposes no SetWriteDeadline, so writes run on a background goroutine
// fed by this bounded buffer and Emit only ever does a non-blocking send. Overflow drops+counts, never
// silently discards (Drops() exposes the count). Applies to every transport, remote and local socket alike.
const syslogBufferSize = 1024

// syslogWriteTimeoutNS bounds a single blocked write; past it the event counts as dropped so the writer
// moves on. An atomic.Int64 (not a const) only so a test can shrink it without racing writeLoop's reads.
var syslogWriteTimeoutNS = func() *atomic.Int64 {
	var v atomic.Int64
	v.Store(int64(2 * time.Second))
	return &v
}()

func syslogWriteTimeout() time.Duration {
	return time.Duration(syslogWriteTimeoutNS.Load())
}

func setSyslogWriteTimeout(d time.Duration) (prev time.Duration) {
	return time.Duration(syslogWriteTimeoutNS.Swap(int64(d)))
}

// syslogWriter is the minimal write surface a SyslogSink needs; *syslog.Writer satisfies it. Exists so
// tests can inject a wedged writer to prove Emit never blocks (a real syslog.Writer is otherwise un-fakeable).
type syslogWriter interface {
	Info(string) error
	Close() error
}

// SyslogSink emits audit events to the system syslog daemon as RFC 5424-ish messages via Go's log/syslog,
// with the JSON-serialised AuditEvent as the message body.
//
// Every transport (local socket or remote tcp/udp) routes through a bounded async buffer drained by a
// single background writer, so a wedged daemon/collector can never block the calling request handler.
type SyslogSink struct {
	Network string // syslog transport: "tcp", "udp", or "" for local socket
	Addr    string // syslog endpoint, e.g. "host:514"; ignored when Network is ""

	w syslogWriter

	queue chan []byte    // bounded buffer of pre-marshalled JSON messages
	drops atomic.Int64   // events dropped due to overflow or write timeout
	wg    sync.WaitGroup // tracks the background writer goroutine
	stop  chan struct{}  // closed by Close to drain+terminate the writer
}

// NewSyslogSink dials the syslog connection and starts the background writer goroutine that keeps Emit
// non-blocking even if the daemon/collector stalls; Close shuts it down.
func NewSyslogSink(network, addr string) (*SyslogSink, error) {
	w, err := syslog.Dial(network, addr, syslog.LOG_INFO|syslog.LOG_DAEMON, "wardyn")
	if err != nil {
		return nil, fmt.Errorf("sinks.syslog: dial: %w", err)
	}
	return newSyslogSinkWith(w, network, addr), nil
}

// newSyslogSinkWith wraps an already-open writer; test seam for a wedged fake writer, no real syslog dial.
func newSyslogSinkWith(w syslogWriter, network, addr string) *SyslogSink {
	s := &SyslogSink{
		Network: network,
		Addr:    addr,
		w:       w,
		queue:   make(chan []byte, syslogBufferSize),
		stop:    make(chan struct{}),
	}
	s.wg.Add(1)
	go s.writeLoop()
	return s
}

// Name implements audit.Sink.
func (s *SyslogSink) Name() string { return "syslog" }

// Emit serialises ev to JSON and hands it to the background writer as an INFO syslog entry. A cancelled
// context skips the write without error. Emit NEVER blocks: a full buffer drops+counts instead.
func (s *SyslogSink) Emit(ctx context.Context, ev types.AuditEvent) error {
	select {
	case <-ctx.Done():
		return nil
	default:
	}
	b, err := marshalEvent(ev)
	if err != nil {
		return fmt.Errorf("sinks.syslog: marshal: %w", err)
	}

	// Non-blocking enqueue; overflow drops + counts.
	select {
	case s.queue <- b:
	default:
		s.drops.Add(1)
		slog.WarnContext(ctx, "sinks.syslog: queue overflow (daemon/collector slow/hung)",
			slog.String("network", s.Network),
			slog.String("addr", s.Addr),
			slog.Int64("drops", s.drops.Load()))
	}
	return nil
}

// writeLoop drains the buffer, bounding each write with syslogWriteTimeout so a stalled collector wedges
// at most one in-flight write (dropped) rather than filling the buffer forever.
func (s *SyslogSink) writeLoop() {
	defer s.wg.Done()
	for {
		select {
		case b := <-s.queue:
			s.timedWrite(b)
		case <-s.stop:
			// Best-effort drain of anything already queued, still bounded.
			for {
				select {
				case b := <-s.queue:
					s.timedWrite(b)
				default:
					return
				}
			}
		}
	}
}

// timedWrite bounds one syslog write with syslogWriteTimeout by racing it in its own goroutine against a
// timer, since log/syslog.Writer has no write deadline; on timeout the event counts as dropped and the
// abandoned goroutine exits later once the collector recovers or the connection closes.
func (s *SyslogSink) timedWrite(b []byte) {
	done := make(chan error, 1)
	go func() { done <- s.w.Info(string(b)) }()

	timeout := syslogWriteTimeout()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case err := <-done:
		if err != nil {
			s.drops.Add(1)
			slog.Error("sinks.syslog: write error",
				slog.Int64("drops", s.drops.Load()),
				slog.Any("err", err))
		}
	case <-timer.C:
		s.drops.Add(1)
		slog.Error("sinks.syslog: write to collector timed out",
			slog.String("network", s.Network),
			slog.String("addr", s.Addr),
			slog.Duration("timeout", timeout),
			slog.Int64("drops", s.drops.Load()))
	}
}

// Drops returns events dropped by overflow or write timeout, matching WebhookSink.Drops.
func (s *SyslogSink) Drops() int64 { return s.drops.Load() }

// Close signals the background writer to drain and stop, then closes the syslog connection.
func (s *SyslogSink) Close() error {
	// Signal the writer to drain and exit; guard against a double Close.
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	s.wg.Wait()
	return s.w.Close()
}
