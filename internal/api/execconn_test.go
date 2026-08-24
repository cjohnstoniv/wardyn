// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// pipeExecSession is a fake ExecSession wired to a real net.Conn: whatever is
// written to the conn's peer appears on Stdout, and whatever the caller writes
// to the execConn arrives on the peer — the same shape `socat -
// TCP:127.0.0.1:<port>` has in a sandbox, without a sandbox. closed reports
// whether ExecSession.Close ran.
func pipeExecSession(peer net.Conn, closed *bool) *runner.ExecSession {
	return &runner.ExecSession{
		Stdin:  peer.(io.WriteCloser),
		Stdout: peer,
		Wait:   func() (int, error) { return 0, nil },
		Close: func() error {
			*closed = true
			return peer.Close()
		},
	}
}

// TestExecConn_RoundTripsBytes: an execConn is a plain bidirectional pipe over
// the exec session — bytes written go to Stdin, bytes read come from Stdout.
func TestExecConn_RoundTripsBytes(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	closed := false
	c := newExecConn(pipeExecSession(b, &closed), "sandbox-1:8080")

	go func() { _, _ = c.Write([]byte("ping")) }()
	buf := make([]byte, 4)
	if _, err := io.ReadFull(a, buf); err != nil {
		t.Fatalf("read from peer: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("peer got %q, want %q", buf, "ping")
	}

	go func() { _, _ = a.Write([]byte("pong")) }()
	got := make([]byte, 4)
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("read from execConn: %v", err)
	}
	if string(got) != "pong" {
		t.Fatalf("execConn got %q, want %q", got, "pong")
	}
}

// TestExecConn_CloseIsIdempotent: net/http closes a connection from both its
// read and write loops, so a second Close must not re-run ExecSession.Close
// (which on the docker driver would be a second exec teardown).
func TestExecConn_CloseIsIdempotent(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	calls := 0
	sess := &runner.ExecSession{
		Stdin:  b.(io.WriteCloser),
		Stdout: b,
		Close:  func() error { calls++; return nil },
	}
	c := newExecConn(sess, "sandbox-1:8080")
	if err := c.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if calls != 1 {
		t.Fatalf("ExecSession.Close ran %d times, want 1", calls)
	}
}

// TestExecConn_NilStreamsAreSafe: ExecSession is a partial-fake-friendly
// struct where any stream may be nil (its own doc). A nil Stdout must read
// EOF and a nil Stdin must fail the write — never panic.
func TestExecConn_NilStreamsAreSafe(t *testing.T) {
	c := newExecConn(&runner.ExecSession{}, "sandbox-1:8080")
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("read with nil Stdout: %v, want EOF", err)
	}
	if _, err := c.Write([]byte("x")); err == nil {
		t.Fatal("write with nil Stdin returned nil error")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close with nil Close: %v", err)
	}
}

// TestExecConn_DeadlinesRefused: the adapter has no deadline machinery, so
// every SetDeadline variant REFUSES rather than silently accepting a deadline
// it would never honour. net/http never calls these on the plain-HTTP path;
// a future caller that does gets an error, not a hang.
func TestExecConn_DeadlinesRefused(t *testing.T) {
	c := newExecConn(&runner.ExecSession{}, "sandbox-1:8080")
	for name, err := range map[string]error{
		"SetDeadline":      c.SetDeadline(time.Now()),
		"SetReadDeadline":  c.SetReadDeadline(time.Now()),
		"SetWriteDeadline": c.SetWriteDeadline(time.Now()),
	} {
		if !errors.Is(err, errExecConnDeadline) {
			t.Fatalf("%s returned %v, want errExecConnDeadline", name, err)
		}
	}
}

// TestExecConn_ServesHTTPOverTheExecLane is the load-bearing one: a real
// http.Transport whose DialContext returns an execConn must complete a real
// HTTP round trip against a real server on the other side of the "exec". This
// is exactly how the UI gateway relays to a sandbox-loopback app; if the
// adapter were missing a net.Conn behaviour http.Transport depends on, this
// fails here rather than against a live sandbox.
func TestExecConn_ServesHTTPOverTheExecLane(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "hello from the sandbox")
	}), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		peer, derr := net.Dial("tcp", ln.Addr().String())
		if derr != nil {
			return nil, derr
		}
		closed := false
		return newExecConn(pipeExecSession(peer, &closed), "sandbox-1:8080"), nil
	}}
	defer tr.CloseIdleConnections()

	resp, err := tr.RoundTrip(mustGet(t, "http://sandbox.invalid/"))
	if err != nil {
		t.Fatalf("round trip over exec lane: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello from the sandbox" {
		t.Fatalf("body %q", body)
	}
}

func mustGet(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return req
}
