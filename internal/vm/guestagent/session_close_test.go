// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"runtime"
	"testing"
	"time"
)

// A Stdin.Write blocked on a guest that reads nothing is woken by Session.Close, which returns within
// 1 s and resets only that exec's stream: the guest sees its request end (it kills the process then),
// the client keeps its one connection, a second Session on it still passes bytes, and no goroutine is
// left. The graceful Stdin.Close is not a stop: it waits behind that write for the frame lock.
func TestSessionClose_WakesBlockedStdinWrite(t *testing.T) {
	stuckEnded := make(chan struct{})
	c := rawGuest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, spec, err := ReadFrame(r.Body)
		if err != nil {
			return
		}
		var er ExecRequest
		_ = json.Unmarshal(spec, &er)
		w.WriteHeader(http.StatusOK)
		flush := w.(http.Flusher).Flush
		flush()
		if er.Argv[0] == "stuck" {
			<-r.Context().Done() // reads no stdin
			close(stuckEnded)
			return
		}
		for { // echo stdin to stdout
			typ, p, err := ReadFrame(r.Body)
			if err != nil {
				return
			}
			if typ == FrameStdin {
				_ = WriteFrame(w, FrameStdout, p)
				flush()
			}
		}
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	other, err := c.Exec(ctx, ExecRequest{Argv: []string{"echo"}, TTY: true})
	if err != nil {
		t.Fatal(err)
	}
	// other is closed only on the passing path: on a failure, rawGuest's cleanup closes the connection,
	// which ends every stream, so a hang in Close fails the test instead of holding it.
	echoStart := func(msg string) chan error {
		got := make(chan error, 1)
		go func() {
			if _, err := other.Stdin.Write([]byte(msg)); err != nil {
				got <- err
				return
			}
			b := make([]byte, len(msg))
			_, err := io.ReadFull(other.Stdout, b)
			if err == nil && !bytes.Equal(b, []byte(msg)) {
				err = io.ErrUnexpectedEOF
			}
			got <- err
		}()
		return got
	}
	wait := func(got chan error, within time.Duration) error {
		select {
		case err := <-got:
			return err
		case <-time.After(within):
			return context.DeadlineExceeded
		}
	}
	echo := func(msg string, within time.Duration) error { return wait(echoStart(msg), within) }
	if err := echo("before", time.Second); err != nil {
		t.Fatalf("the other session before the stall: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	base := runtime.NumGoroutine()

	s, err := c.Exec(ctx, ExecRequest{Argv: []string{"stuck"}, TTY: true})
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		at  time.Time
		err error
	}
	wrote := make(chan result, 1)
	// Far more than the HTTP/2 stream window, so the write cannot complete while the guest reads nothing.
	go func() { _, err := s.Stdin.Write(make([]byte, 16<<20)); wrote <- result{time.Now(), err} }()
	select {
	case w := <-wrote:
		t.Fatalf("Stdin.Write returned %v before Close with nothing read", w.err)
	case <-time.After(300 * time.Millisecond):
	}
	// The write has stalled once the other session's input cannot pass either: the stuck exec's unread input
	// then holds the shared connection's window, so the write is parked inside a frame, holding the frame
	// lock. Until then it may still be moving (a slow runner), and a graceful Stdin.Close can pass between
	// its frames. A timeout (the stall) is recorded; any other error fails below.
	var during chan error
	var duringErr error
	for until := time.Now().Add(10 * time.Second); ; {
		during = echoStart("during")
		if duringErr = wait(during, time.Second); duringErr != nil || time.Now().After(until) {
			break
		}
	}
	graceful := make(chan error, 1)
	go func() { graceful <- s.Stdin.Close() }()
	select {
	case err := <-graceful:
		t.Fatalf("the graceful Stdin.Close returned %v while the write held the frame lock", err)
	case <-time.After(200 * time.Millisecond):
	}

	start := time.Now()
	closed := make(chan struct{})
	go func() { _ = s.Close(); _ = s.Close(); close(closed) }() // Close is idempotent
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Session.Close did not return within 1s of a blocked Stdin.Write")
	}
	select {
	case w := <-wrote:
		if w.err == nil {
			t.Fatal("the blocked Stdin.Write reported success after Close")
		}
		if d := w.at.Sub(start); d > time.Second {
			t.Fatalf("the blocked Stdin.Write returned %s after Close, want within 1s", d)
		}
	case <-time.After(time.Second):
		t.Fatal("Session.Close did not wake the blocked Stdin.Write")
	}
	select {
	case <-graceful:
	case <-time.After(time.Second):
		t.Fatal("the graceful Stdin.Close stayed blocked after Close")
	}
	select {
	case <-stuckEnded:
	case <-time.After(time.Second):
		t.Fatal("the guest's request did not end within 1s of Close: the exec's stream was not reset")
	}
	select {
	case <-s.done:
	default:
		t.Fatal("demux still running after Close")
	}

	t.Logf("the other session during the stall: %v", duringErr)
	switch duringErr {
	case nil:
	case context.DeadlineExceeded:
		// The stalled exec's unread input can hold the shared connection's window; its reset hands it back.
		if err := wait(during, time.Second); err != nil {
			t.Fatalf("the other session's input from the stall did not pass within 1s of Close: %v", err)
		}
	default:
		t.Fatalf("the other session's input during the stall failed: %v", duringErr)
	}
	if err := echo("after", 2*time.Second); err != nil {
		t.Fatalf("the other session after Close: %v", err)
	}
	c.mu.Lock()
	conns := len(c.conns)
	c.mu.Unlock()
	if conns != 1 {
		t.Fatalf("the client holds %d connections after Close, want the same 1", conns)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base {
		t.Fatalf("%d goroutines after Close, %d before the stuck exec", n, base)
	}
	_ = other.Close()
}
