// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package sinks provides production audit.Sink implementations (syslog,
// webhook, file) plus a Fanout multiplexer and config wiring.
// All implementations are stdlib-only: no third-party logging or HTTP clients.
//
// Constraint: sinks must never block the caller's goroutine beyond the time
// required for a buffered channel send (webhook, syslog) or a local syscall
// (file). Drop counters are incremented and logged on overflow; events are
// never silently discarded.
package sinks

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"log/syslog"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// syslog sink tuning constants.
//
// These bound the request path for EVERY transport (remote tcp/udp collector AND
// the local /dev/log socket). log/syslog.Writer does not expose the underlying
// net.Conn, so we cannot set a SetWriteDeadline on it directly; instead we run
// the actual write on a single background goroutine fed by a bounded buffer.
// Emit only does a non-blocking channel send, so it can never be parked by a
// hung collector — nor by a wedged local syslog daemon or a full /dev/log
// datagram peer buffer (a unixgram send blocks when the receiver's buffer is
// full). See HIGH finding "Bound the remote syslog sink…" and U095 (the same
// bug class on the local socket, originally left synchronous on the false
// assumption that a /dev/log write is always a fast syscall).
const (
	// syslogBufferSize is the capacity of the in-process event queue. Events that
	// would overflow the buffer are dropped and counted (never silently discarded
	// — Drops() exposes the count).
	syslogBufferSize = 1024
	// syslogWriteTimeout bounds a single blocked write. If the background writer
	// is stuck on s.w.Info for longer than this, the event is counted as dropped
	// so the writer can move on and never wedge permanently on one write.
	syslogWriteTimeout = 2 * time.Second
)

// syslogWriter is the minimal write surface a SyslogSink needs; *syslog.Writer
// satisfies it. It exists so tests can inject a wedged writer and prove Emit
// never blocks even when the underlying transport hangs (log/syslog.Writer is a
// concrete type dialing a real socket, otherwise un-fakeable).
type syslogWriter interface {
	Info(string) error
	Close() error
}

// SyslogSink emits audit events to the system syslog daemon as RFC 5424-ish
// messages (using Go's log/syslog, which writes RFC 3164 to local sockets and
// RFC 5424-ish via the "wardyn" tag). The JSON-serialised AuditEvent is the
// message body.
//
// EVERY transport — the local socket (/dev/log on Linux, /var/run/syslog on
// macOS; Network=="") and a remote "tcp"/"udp" collector — routes through a
// bounded async buffer drained by a single background writer goroutine, so a
// wedged local syslog daemon, a full /dev/log datagram peer buffer, or a hung
// remote collector can never block the calling request handler (Fanout.Emit
// waits for every child synchronously from the request path).
type SyslogSink struct {
	// Network is the syslog transport: "tcp", "udp", or "" for local socket.
	Network string
	// Addr is the syslog endpoint, e.g. "host:514". Ignored when Network is "".
	Addr string

	w syslogWriter

	// async machinery (always in use; no transport writes synchronously).
	queue chan []byte    // bounded buffer of pre-marshalled JSON messages
	drops atomic.Int64   // events dropped due to overflow or write timeout
	wg    sync.WaitGroup // tracks the background writer goroutine
	stop  chan struct{}  // closed by Close to drain+terminate the writer
}

// NewSyslogSink constructs and dials the syslog connection.
// Returns an error if the connection cannot be established.
//
// A single background writer goroutine is started for every transport so that
// Emit stays non-blocking even if the daemon/collector stalls; it is shut down
// by Close.
func NewSyslogSink(network, addr string) (*SyslogSink, error) {
	w, err := syslog.Dial(network, addr, syslog.LOG_INFO|syslog.LOG_DAEMON, "wardyn")
	if err != nil {
		return nil, fmt.Errorf("sinks.syslog: dial: %w", err)
	}
	return newSyslogSinkWith(w, network, addr), nil
}

// newSyslogSinkWith wraps an already-open writer and starts the background
// writer. Test seam: a wedged fake writer proves Emit never blocks on a stalled
// transport (incl. the local socket) without dialing a real syslog.
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
// syslog entry. A cancelled context causes the write to be skipped without error
// (the recorder is shutting down).
//
// Emit NEVER blocks: it performs only a non-blocking enqueue onto a bounded
// buffer for every transport. If the buffer is full (the daemon/collector is
// hung and the background writer is stalled) the event is dropped and counted
// rather than blocking the caller's request goroutine. This prevents a hung
// remote collector OR a wedged local syslog daemon from stalling the synchronous
// request path via Fanout.Emit.
func (s *SyslogSink) Emit(ctx context.Context, ev types.AuditEvent) error {
	select {
	case <-ctx.Done():
		return nil
	default:
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("sinks.syslog: marshal: %w", err)
	}

	// Never block the caller. Non-blocking enqueue; on overflow drop + count
	// (never silently discarded — Drops() reports it).
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

// writeLoop drains the bounded buffer and writes each event to the remote
// collector, bounding any single write with syslogWriteTimeout so a stalled
// TCP collector can wedge at most one in-flight write (then that event is
// counted as dropped) rather than the buffer filling forever behind a
// permanently blocked write.
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
// goroutine and is raced against a timer; on timeout the event is counted as
// dropped and the writer moves on. The abandoned write goroutine will unblock
// and exit if/when the collector recovers or the connection is closed, so it
// does not accumulate under a single stalled write (Emit's bounded buffer caps
// total in-flight work).
func (s *SyslogSink) timedWrite(b []byte) {
	done := make(chan error, 1)
	go func() { done <- s.w.Info(string(b)) }()

	timer := time.NewTimer(syslogWriteTimeout)
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
			slog.Duration("timeout", syslogWriteTimeout),
			slog.Int64("drops", s.drops.Load()))
	}
}

// Drops returns the number of events dropped due to buffer overflow or write
// timeout. Exposed so the never-drop-silently invariant can be observed,
// matching WebhookSink.Drops.
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

// syslogAvailable returns true when a local syslog socket exists.
// Used only in tests to skip gracefully on platforms without local syslog.
func syslogAvailable() bool {
	switch runtime.GOOS {
	case "linux":
		// /dev/log is the conventional Linux local socket.
		w, err := syslog.Dial("", "", syslog.LOG_INFO|syslog.LOG_DAEMON, "wardyn-test")
		if err != nil {
			return false
		}
		_ = w.Close()
		return true
	case "darwin":
		w, err := syslog.Dial("", "", syslog.LOG_INFO|syslog.LOG_DAEMON, "wardyn-test")
		if err != nil {
			return false
		}
		_ = w.Close()
		return true
	default:
		return false
	}
}
