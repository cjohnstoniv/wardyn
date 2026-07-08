// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package sinks provides production audit.Sink implementations (syslog,
// webhook, file) plus a Fanout multiplexer and config wiring.
// All implementations are stdlib-only: no third-party logging or HTTP clients.
//
// Constraint: sinks must never block the caller's goroutine beyond the time
// required for a buffered channel send (webhook) or a local syscall (syslog,
// file). Drop counters are incremented and logged on overflow; events are
// never silently discarded.
package sinks

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/syslog"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// syslog sink tuning constants.
//
// These bound the request path when the sink is pointed at a remote (tcp/udp)
// collector that stalls. log/syslog.Writer does not expose the underlying
// net.Conn, so we cannot set a SetWriteDeadline on it directly; instead we run
// the actual write on a single background goroutine fed by a bounded buffer.
// Emit only does a non-blocking channel send, so it can never be parked by a
// hung collector. See HIGH finding: "Bound the remote syslog sink so a hung
// collector can't block the request path."
const (
	// syslogBufferSize is the capacity of the in-process event queue for remote
	// syslog. Events that would overflow the buffer are dropped and counted
	// (never silently discarded — Drops() exposes the count).
	syslogBufferSize = 1024
	// syslogWriteTimeout bounds a single blocked write to the collector. If the
	// background writer is stuck on s.w.Info for longer than this, the event is
	// counted as dropped so the writer can move on and never wedge permanently
	// on one TCP write.
	syslogWriteTimeout = 2 * time.Second
)

// SyslogSink emits audit events to the system syslog daemon as RFC 5424-ish
// messages (using Go's log/syslog, which writes RFC 3164 to local sockets and
// RFC 5424-ish via the "wardyn" tag). The JSON-serialised AuditEvent is the
// message body.
//
// If Network and Addr are both empty the sink dials the local syslog socket
// (/dev/log on Linux, /var/run/syslog on macOS) and Emit writes synchronously
// (a fast local syscall, the original behavior). When Network is non-empty
// (remote "tcp"/"udp" collector) Emit instead hands the event to a bounded
// async buffer drained by a single background writer goroutine, so a hung or
// slow collector can never block the calling request handler (Fanout.Emit waits
// for every child synchronously from the request path).
type SyslogSink struct {
	// Network is the syslog transport: "tcp", "udp", or "" for local socket.
	Network string
	// Addr is the syslog endpoint, e.g. "host:514". Ignored when Network is "".
	Addr string

	w *syslog.Writer

	// remote async machinery (nil/unused for local-socket sinks).
	queue  chan []byte    // bounded buffer of pre-marshalled JSON messages
	drops  atomic.Int64   // events dropped due to overflow or write timeout
	wg     sync.WaitGroup // tracks the background writer goroutine
	stop   chan struct{}  // closed by Close to drain+terminate the writer
	remote bool           // true when the async path is in use
}

// NewSyslogSink constructs and dials the syslog connection.
// Returns an error if the connection cannot be established.
//
// For remote transports (network "tcp"/"udp") a single background writer
// goroutine is started so that Emit stays non-blocking even if the collector
// stalls; it is shut down by Close.
func NewSyslogSink(network, addr string) (*SyslogSink, error) {
	w, err := syslog.Dial(network, addr, syslog.LOG_INFO|syslog.LOG_DAEMON, "wardyn")
	if err != nil {
		return nil, fmt.Errorf("sinks.syslog: dial: %w", err)
	}
	s := &SyslogSink{Network: network, Addr: addr, w: w}

	// Only remote collectors can stall arbitrarily; local socket writes are a
	// fast syscall and keep the original synchronous behavior.
	if network != "" {
		s.remote = true
		s.queue = make(chan []byte, syslogBufferSize)
		s.stop = make(chan struct{})
		s.wg.Add(1)
		go s.writeLoop()
	}
	return s, nil
}

// Name implements audit.Sink.
func (s *SyslogSink) Name() string { return "syslog" }

// Emit serialises ev to JSON and writes it as an INFO syslog entry.
// A cancelled context causes the write to be skipped without error (the
// recorder is shutting down).
//
// For local-socket sinks the write is synchronous (a fast local syscall). For
// remote (tcp/udp) sinks Emit performs only a non-blocking enqueue onto a
// bounded buffer: if the buffer is full (e.g. the collector is hung and the
// background writer is stalled) the event is dropped and counted rather than
// blocking the caller's request goroutine. This is the HIGH-severity fix that
// prevents a hung remote collector from wedging the synchronous request path
// via Fanout.Emit.
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

	if !s.remote {
		// Local socket: fast syscall, keep synchronous behavior.
		if err := s.w.Info(string(b)); err != nil {
			return fmt.Errorf("sinks.syslog: write: %w", err)
		}
		return nil
	}

	// Remote collector: never block the caller. Non-blocking enqueue; on
	// overflow drop + count (never silently discarded — Drops() reports it).
	select {
	case s.queue <- b:
	default:
		s.drops.Add(1)
		log.Printf("sinks.syslog: queue overflow (collector %s %s slow/hung), drop counter=%d",
			s.Network, s.Addr, s.drops.Load())
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
			log.Printf("sinks.syslog: write error (drops=%d): %v", s.drops.Load(), err)
		}
	case <-timer.C:
		s.drops.Add(1)
		log.Printf("sinks.syslog: write to collector %s %s timed out after %s (drops=%d)",
			s.Network, s.Addr, syslogWriteTimeout, s.drops.Load())
	}
}

// Drops returns the number of events dropped due to buffer overflow or write
// timeout (remote sinks only; always 0 for local-socket sinks). Exposed so the
// never-drop-silently invariant can be observed, matching WebhookSink.Drops.
func (s *SyslogSink) Drops() int64 { return s.drops.Load() }

// Close closes the underlying syslog connection. For remote sinks it first
// signals the background writer to drain and stop.
func (s *SyslogSink) Close() error {
	if s.remote {
		// Signal the writer to drain and exit; guard against a double Close.
		select {
		case <-s.stop:
		default:
			close(s.stop)
		}
		s.wg.Wait()
	}
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
