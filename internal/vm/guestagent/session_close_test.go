// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"runtime"
	"testing"
	"time"
)

// A Stdin.Write blocked on a guest that reads nothing is woken by Session.Close, which returns within
// 1 s and resets only that exec's stream: the guest sees its request end (it kills the process then),
// the client keeps its one connection, a second Session on it still passes bytes, and no goroutine is
// left. A graceful Stdin.Close started during the stall returns once Close has run.
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
	// The stall: the other session's input no longer passes, the stuck exec's unread input holding the shared
	// connection's window. Until then the write may still be moving (a slow runner).
	var during chan error
	for until := time.Now().Add(20 * time.Second); ; {
		during = echoStart("during")
		err := wait(during, time.Second)
		if errors.Is(err, context.DeadlineExceeded) {
			break
		}
		if err != nil {
			t.Fatalf("the other session's input before the stall failed: %v", err)
		}
		if time.Now().After(until) {
			t.Fatal("the stuck exec's write never stalled the connection within 20s")
		}
	}
	// Whether it waits behind the write for the frame lock is TestStdinClose_WaitsBehindAHeldFrame's.
	graceful := make(chan error, 1)
	go func() { graceful <- s.Stdin.Close() }()

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

	// The stalled exec's reset hands the connection's window back.
	if err := wait(during, time.Second); err != nil {
		t.Fatalf("the other session's input from the stall did not pass within 1s of Close: %v", err)
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

// A graceful Stdin.Close waits behind a Stdin.Write parked inside a frame: the write holds the frame lock
// from the frame's header to the end of its payload, so the close frame cannot land inside it.
func TestStdinClose_WaitsBehindAHeldFrame(t *testing.T) {
	upR, upW := io.Pipe()
	t.Cleanup(func() { _ = upR.Close() })
	in := stdinWriter{&Session{up: upW}}
	wrote := make(chan error, 1)
	go func() { _, err := in.Write(make([]byte, 2*MaxFrame)); wrote <- err }()
	// The first frame's header is read and its payload withheld: the write is parked inside that frame.
	var hdr [5]byte
	if _, err := io.ReadFull(upR, hdr[:]); err != nil {
		t.Fatal(err)
	}
	if hdr[0] != FrameStdin {
		t.Fatalf("the first frame is 0x%02x, want stdin", hdr[0])
	}
	closed := make(chan error, 1)
	go func() { closed <- in.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("the graceful Stdin.Close returned %v while the write held the frame lock", err)
	case <-time.After(200 * time.Millisecond):
	}
	// Reading the rest frees both, and every frame arrives whole: the held payload, then the second stdin
	// frame and the close, in either order.
	got := make(chan []byte, 1)
	go func() {
		var kinds []byte
		if _, err := io.ReadFull(upR, make([]byte, MaxFrame)); err == nil {
			for len(kinds) < 2 {
				typ, _, err := ReadFrame(upR)
				if err != nil {
					break
				}
				kinds = append(kinds, typ)
			}
		}
		got <- kinds
		_, _ = io.Copy(io.Discard, upR) // a frame's empty payload is a write too, and the transport reads on
	}()
	for _, c := range []chan error{wrote, closed} {
		select {
		case err := <-c:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("the write or the graceful close stayed blocked once the frames were read")
		}
	}
	_ = upW.Close()
	select {
	case kinds := <-got:
		stdin, closes := 0, 0
		for _, k := range kinds {
			switch k {
			case FrameStdin:
				stdin++
			case FrameStdinClose:
				closes++
			}
		}
		if stdin != 1 || closes != 1 {
			t.Fatalf("after the held frame the guest read frames %x, want one more stdin and the close", kinds)
		}
	case <-time.After(time.Second):
		t.Fatal("the frames after the held one were not read")
	}
}
