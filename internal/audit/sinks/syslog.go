// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package sinks provides production audit.Sink implementations (syslog,
// webhook, file) plus a Fanout multiplexer and config wiring. Stdlib-only: no
// third-party logging or HTTP clients.
//
// Constraint: sinks must never block the caller's goroutine beyond a buffered
// channel send or a local syscall. Drop counters are incremented and logged on
// overflow; events are never silently discarded.
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

// syslogBufferSize is the capacity of the in-process event queue, for every
// transport (remote tcp/udp AND the local /dev/log socket): log/syslog.Writer
// exposes no SetWriteDeadline, so the actual write runs on a background
// goroutine fed by this bounded buffer, and Emit only ever does a non-blocking
// send. Events that overflow the buffer are dropped and counted, never
// silently discarded (Drops() exposes the count).
const syslogBufferSize = 1024

// syslogWriteTimeoutNS bounds a single blocked write: if the background writer
// is stuck on s.w.Info for longer than this, the event is counted as dropped
// so the writer can move on.
//
// An atomic.Int64 of nanoseconds (not a const) purely so a test can shrink it
// without racing the background writeLoop goroutine that reads it
// concurrently — see TestSyslogWriteTimeout_ProductionValueUnchanged for the
// guard that the production default itself is untouched.
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

// syslogWriter is the minimal write surface a SyslogSink needs; *syslog.Writer
// satisfies it. It exists so tests can inject a wedged writer to prove Emit
// never blocks (log/syslog.Writer dials a real socket and is otherwise
// un-fakeable).
type syslogWriter interface {
	Info(string) error
	Close() error
}

// SyslogSink emits audit events to the system syslog daemon as RFC 5424-ish
// messages via Go's log/syslog, with the JSON-serialised AuditEvent as the
// message body.
//
// EVERY transport — the local socket (Network=="") and a remote "tcp"/"udp"
// collector — routes through a bounded async buffer drained by a single
// background writer goroutine, so a wedged daemon or hung collector can never
// block the calling request handler (Fanout.Emit waits for every child
// synchronously).
type SyslogSink struct {
	// Network is the syslog transport: "tcp", "udp", or "" for local socket.
	Network string
	// Addr is the syslog endpoint, e.g. "host:514". Ignored when Network is "".
	Addr string

	w syslogWriter

	queue chan []byte    // bounded buffer of pre-marshalled JSON messages
	drops atomic.Int64   // events dropped due to overflow or write timeout
	wg    sync.WaitGroup // tracks the background writer goroutine
	stop  chan struct{}  // closed by Close to drain+terminate the writer
}

// NewSyslogSink constructs and dials the syslog connection, returning an error
// if it cannot be established. A single background writer goroutine is
// started so Emit stays non-blocking even if the daemon/collector stalls; it
// is shut down by Close.
func NewSyslogSink(network, addr string) (*SyslogSink, error) {
	w, err := syslog.Dial(network, addr, syslog.LOG_INFO|syslog.LOG_DAEMON, "wardyn")
	if err != nil {
		return nil, fmt.Errorf("sinks.syslog: dial: %w", err)
	}
	return newSyslogSinkWith(w, network, addr), nil
}

// newSyslogSinkWith wraps an already-open writer and starts the background
// writer. Test seam: a wedged fake writer proves Emit never blocks, without
// dialing a real syslog.
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

// Emit serialises ev to JSON and hands it to the background writer as an INFO
// syslog entry. A cancelled context skips the write without error (the
// recorder is shutting down).
//
// Emit NEVER blocks: a full buffer (daemon/collector hung, writer stalled)
// drops and counts the event rather than blocking the caller's request
// goroutine.
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

	// Non-blocking enqueue; on overflow drop + count (Drops() reports it).
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

// writeLoop drains the bounded buffer and writes each event, bounding any
// single write with syslogWriteTimeout so a stalled collector can wedge at
// most one in-flight write (counted as dropped) rather than the buffer
// filling forever behind it.
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

// timedWrite performs one syslog write bounded by syslogWriteTimeout. Because
// log/syslog.Writer exposes no write deadline, the write runs in its own
// goroutine raced against a timer; on timeout the event is counted as dropped
// and the writer moves on. The abandoned goroutine unblocks and exits once the
// collector recovers or the connection closes.
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

// Drops returns the number of events dropped due to buffer overflow or write
// timeout, matching WebhookSink.Drops.
func (s *SyslogSink) Drops() int64 { return s.drops.Load() }

// Close signals the background writer to drain and stop, then closes the
// underlying syslog connection.
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
