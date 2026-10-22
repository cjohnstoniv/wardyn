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
	"sync"
	"sync/atomic"
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

// helloListener answers each connection's hello as the agent does, before HTTP/2 has it.
type helloListener struct{ net.Listener }

func (l helloListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		hello := make([]byte, helloLen)
		if _, err := io.ReadFull(c, hello); err == nil {
			if _, err := c.Write(hello); err == nil {
				return c, nil
			}
		}
		_ = c.Close()
	}
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
	go func() { _ = hs.Serve(helloListener{l}) }()
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

// pipeGuest is a dial whose every connection is served by guest, a fake guest agent; dials counts them.
func pipeGuest(guest func(g net.Conn)) (func(context.Context) (net.Conn, error), *atomic.Int32) {
	dials := new(atomic.Int32)
	return func(context.Context) (net.Conn, error) {
		dials.Add(1)
		host, g := net.Pipe()
		go func() {
			defer g.Close()
			guest(g)
		}()
		return host, nil
	}, dials
}

// readHello reads a host's hello at the guest's end; nil once the host has gone.
func readHello(g net.Conn) []byte {
	hello := make([]byte, helloLen)
	if _, err := io.ReadFull(g, hello); err != nil {
		return nil
	}
	return hello
}

var garbage = bytes.Repeat([]byte{0xff}, 4<<10)

// streamGarbage answers a hello with bytes that never end.
func streamGarbage(g net.Conn) {
	if readHello(g) == nil {
		return
	}
	for {
		if _, err := g.Write(garbage); err != nil {
			return
		}
	}
}

// What precedes a dial's echo belongs to an earlier session: its last writes, and the echo of a hello
// whose host gave up. The host drops it, and the stale echo, whose nonce is not this dial's, does not
// open the session.
func TestTheHostDropsWhatComesBeforeItsEcho(t *testing.T) {
	var mu sync.Mutex
	var hellos [][]byte
	dial, _ := pipeGuest(func(g net.Conn) {
		hello := readHello(g)
		if hello == nil {
			return
		}
		before := []byte("the last session's frames")
		mu.Lock()
		if len(hellos) > 0 {
			before = append(before, hellos[len(hellos)-1]...) // its echo: the agent's version is the host's
		}
		hellos = append(hellos, hello)
		mu.Unlock()
		if _, err := g.Write(append(before, hello...)); err != nil {
			return
		}
		b := make([]byte, 4)
		if _, err := io.ReadFull(g, b); err == nil && string(b) == "ping" {
			_, _ = g.Write([]byte("pong"))
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for i := range 2 {
		conn, err := dialSession(ctx, dial, hostHello)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		_, _ = conn.Write([]byte("ping"))
		b := make([]byte, 4)
		_, err = io.ReadFull(conn, b)
		_ = conn.Close()
		if err != nil || string(b) != "pong" {
			t.Fatalf("dial %d: the session's first bytes = %q, %v; want the guest's answer and nothing from before", i, b, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if n1, n2 := hellos[0][magicLen+1:], hellos[1][magicLen+1:]; bytes.Equal(n1, n2) {
		t.Fatalf("two dials sent the same nonce %x", n1)
	}
}

// The guest is not trusted: a dial waits for its echo within bounds whatever the guest does, and is
// refused by name past them. Only a guest that does not answer is dialled again.
func TestHostileGuestsAreRefused(t *testing.T) {
	short := hostHello
	short.wait = 50 * time.Millisecond
	for _, tc := range []struct {
		name  string
		b     helloBounds
		guest func(g net.Conn)
		want  error
		dials int32
	}{
		{"streams garbage", hostHello, streamGarbage, errHelloBytes, 1},
		{"never answers", short, func(g net.Conn) { _, _ = io.Copy(io.Discard, g) }, errHelloTimeout, int32(short.attempts)},
		{"answers another version", hostHello, func(g net.Conn) {
			if hello := readHello(g); hello != nil {
				hello[magicLen]++
				_, _ = g.Write(hello)
				_, _ = io.Copy(io.Discard, g)
			}
		}, errHelloVersion, 1},
		{"writes past its echo", hostHello, func(g net.Conn) {
			if hello := readHello(g); hello != nil {
				_, _ = g.Write(append(hello, 'x'))
				_, _ = io.Copy(io.Discard, g)
			}
		}, errHelloEcho, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dial, dials := pipeGuest(tc.guest)
			done := make(chan error, 1)
			go func() {
				conn, err := dialSession(context.Background(), dial, tc.b)
				if conn != nil {
					_ = conn.Close()
				}
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, tc.want) || dials.Load() != tc.dials {
					t.Fatalf("dial = %v after %d dials; want %v after %d", err, dials.Load(), tc.want, tc.dials)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the dial outlived its bounds")
			}
		})
	}
}

// A guest that does not answer the hello fails the call by name: a refusal, not a hang.
func TestTheClientSeesARefusalNotAHang(t *testing.T) {
	dial, _ := pipeGuest(streamGarbage)
	c := NewClient(dial)
	defer c.Close()
	done := make(chan error, 1)
	go func() { _, err := c.Ready(context.Background()); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, errHelloBytes) {
			t.Fatalf("ready = %v; want the handshake's refusal", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ready hung on a guest that does not answer the hello")
	}
}
