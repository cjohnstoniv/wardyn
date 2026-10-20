// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// guestServer is an h2c guest whose /v1/exec handler is h; it reads the spec frame first, as the real agent does.
func guestServer(t *testing.T, h func(w http.ResponseWriter, flush func())) *Client {
	t.Helper()
	return rawGuest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, err := ReadFrame(r.Body); err != nil {
			return
		}
		w.WriteHeader(http.StatusOK)
		flush := w.(http.Flusher).Flush
		flush()
		h(w, flush)
	}))
}

// rawGuest is an h2c guest served by handler.
func rawGuest(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	hs := &http.Server{Protocols: &protocols, Handler: handler}
	go func() { _ = hs.Serve(l) }()
	t.Cleanup(func() { _ = hs.Close() })
	c := NewClient(func(context.Context) (net.Conn, error) { return net.Dial("tcp", l.Addr().String()) })
	t.Cleanup(c.Close)
	return c
}

func TestCloseDoesNotWaitOnAReaderThatStopped(t *testing.T) {
	c := guestServer(t, func(w http.ResponseWriter, flush func()) {
		for { // stdout without end, never an exit frame
			if WriteFrame(w, FrameStdout, []byte("hello\n")) != nil {
				return
			}
			flush()
			time.Sleep(time.Millisecond)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := c.Exec(ctx, ExecRequest{Argv: []string{"x"}, TTY: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stdout.Read(make([]byte, 6)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // demux is now parked writing the next frame into the unread pipe
	closed := make(chan struct{})
	go func() { _ = s.Close(); _ = s.Close(); close(closed) }() // Close is idempotent
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked after the Stdout reader stopped")
	}
	select {
	case <-s.done:
	default:
		t.Fatal("demux still running after Close")
	}
	if _, err := s.Stdout.Read(make([]byte, 6)); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("read after Close = %v, want io.ErrClosedPipe", err)
	}
}

func TestGuestErrorTextIsCappedAndQuoted(t *testing.T) {
	huge := bytes.Repeat([]byte("\x1b[31m\n"), MaxFrame/6) // ~64 KB of escape bytes
	c := guestServer(t, func(w http.ResponseWriter, flush func()) {
		_ = WriteFrame(w, FrameError, huge)
		flush()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	check := func(what string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: no error", what)
		}
		msg := err.Error()
		// %q turns each escape byte into at most 4 characters, so the cap bounds the message at 4 KB plus its prefix.
		if len(msg) > 4*maxGuestText+100 {
			t.Fatalf("%s: error is %d bytes, uncapped", what, len(msg))
		}
		if strings.ContainsAny(msg, "\x1b\n") || !strings.Contains(msg, `\x1b[31m\n`) {
			t.Fatalf("%s: error text is not quoted: %.80q", what, msg)
		}
	}
	_, err := c.Start(ctx, ExecRequest{Argv: []string{"x"}})
	check("Start", err)
	s, err := c.Exec(ctx, ExecRequest{Argv: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Wait()
	check("Exec", err)

	// The non-200 path: statusErr reads at most 1 KB of the body.
	for _, code := range []int{http.StatusInternalServerError, http.StatusNotFound} {
		bad := rawGuest(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write(huge)
		}))
		_, err = bad.Agent(ctx)
		check("status "+http.StatusText(code), err)
	}
}

func TestStartRefusesAnExecIDOutsideTheAllowedSet(t *testing.T) {
	for _, tc := range []struct {
		id string
		ok bool
	}{
		{"e1", true},
		{"_x", true},
		{"a.b_c-9", true},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), false},
		{".", false},
		{"..", false},
		{".hidden", false},
		{"a/b", false},
		{"a b", false},
		{"a\nb", false},
		{"a%2fb", false},
	} {
		c := guestServer(t, func(w http.ResponseWriter, flush func()) {
			_ = WriteFrame(w, FrameStarted, []byte(tc.id))
			flush()
		})
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		id, err := c.Start(ctx, ExecRequest{Argv: []string{"x"}})
		cancel()
		if tc.ok && (err != nil || id != tc.id) {
			t.Errorf("id %q = %q, %v; want accepted", tc.id, id, err)
		}
		if !tc.ok && (err == nil || !strings.Contains(err.Error(), "exec id")) {
			t.Errorf("id %q accepted (%q, %v)", tc.id, id, err)
		}
	}
}
