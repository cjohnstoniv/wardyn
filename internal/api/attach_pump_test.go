// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"github.com/google/uuid"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestAttachWS_LargePasteSurvives pins attachReadLimit. coder/websocket's
// DEFAULT message read limit is 32 KiB and exceeding it does not truncate the
// frame — it closes the socket with StatusMessageTooBig — so before the explicit
// SetReadLimit an operator pasting a >32 KiB patch or log lost the whole
// terminal session. The assertion is that the bytes ARRIVE at the sandbox: a
// server without SetReadLimit kills the connection instead, the session never
// sees a write, and waitFor times out.
func TestAttachWS_LargePasteSurvives(t *testing.T) {
	srv, _, fr, _, run := holderTestServer(t)
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	defer ts.Close()

	c := dialAttach(t, ts, srv, run.ID, holderOwner, "")
	if mode := readAttachMode(t, c); mode.ReadOnly {
		t.Fatal("the FIRST client was told it is read-only")
	}
	waitFor(t, "the holder's session to open", func() bool { return fr.session(0) != nil })
	sess := fr.session(0)

	// One message, comfortably past the library default and past the PTY read
	// buffer, but inside attachReadLimit.
	paste := bytes.Repeat([]byte("x"), 64<<10)
	wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wcancel()
	if err := c.Write(wctx, websocket.MessageBinary, paste); err != nil {
		t.Fatalf("write %d-byte paste: %v", len(paste), err)
	}

	waitFor(t, "the paste to reach the sandbox", func() bool { return sess.writtenBytes() >= len(paste) })
}

// TestAttachWS_KeystrokesDoNotUpdateTheRunRow pins that the client->server pump
// no longer issues one agent_runs UPDATE per inbound PTY frame, synchronously
// ahead of the write to the sandbox. The 30s attachKeepalive ticker already
// bounds updated_at staleness for the whole attach, so the per-frame touch was
// pure write amplification (and DB latency in front of every keystroke).
//
// The barrier is the sandbox itself: the holder's keystrokes ARE written
// through, so waiting for all 20 to land proves the pump consumed every frame —
// which is exactly when a per-frame touch would have fired.
func TestAttachWS_KeystrokesDoNotUpdateTheRunRow(t *testing.T) {
	srv, st, fr, _, run := holderTestServer(t)
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	defer ts.Close()

	c := dialAttach(t, ts, srv, run.ID, holderOwner, "")
	readAttachMode(t, c)
	waitFor(t, "the holder's session to open", func() bool { return fr.session(0) != nil })
	sess := fr.session(0)

	// The handler touches ONCE on open (so an attach racing a reap tick still
	// resets the clock); everything after that is the ticker's job.
	waitFor(t, "the on-open touch", func() bool { return st.touches() >= 1 })
	before := st.touches()

	wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wcancel()
	for range 20 {
		if err := c.Write(wctx, websocket.MessageBinary, []byte("ls\r")); err != nil {
			t.Fatalf("write keystrokes: %v", err)
		}
	}
	waitFor(t, "the keystrokes to reach the sandbox", func() bool { return sess.written() >= 20 })

	// The keepalive ticker is 30s, so nothing legitimate can touch during this
	// test beyond the on-open call already counted.
	if got := st.touches(); got != before {
		t.Errorf("20 keystroke frames caused %d agent_runs UPDATEs; want 0 (the 30s keepalive owns the idle clock)", got-before)
	}
}

// TestAttachWS_ShellExitSendsNormalClosure pins #1112: when the PTY session
// ends (sess.Read returns io.EOF — a shell exit), the client must receive a
// REAL close frame carrying StatusNormalClosure, not a bare connection drop.
// Before the fix, attachPump cancelled the shared pump ctx first; that races
// coder/websocket's own context-triggered teardown of the OTHER goroutine's
// blocked c.Read(ctx) (which forcibly closes the raw connection with no close
// frame the instant ctx is Done), so the client saw "failed to read frame
// header: EOF" instead of a clean close.
func TestAttachWS_ShellExitSendsNormalClosure(t *testing.T) {
	srv, _, fr, _, run := holderTestServer(t)
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	defer ts.Close()

	c := dialAttach(t, ts, srv, run.ID, holderOwner, "")
	readAttachMode(t, c)
	waitFor(t, "the holder's session to open", func() bool { return fr.session(0) != nil })
	sess := fr.session(0)

	// Simulate the remote shell exiting normally: closing the pipe's write
	// side makes sess.Read return io.EOF, exactly like a real PTY session
	// ending.
	_ = sess.w.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("client Read returned no error after the shell exited; want a close frame")
	} else if got := websocket.CloseStatus(err); got != websocket.StatusNormalClosure {
		t.Fatalf("close status = %v (%v), want StatusNormalClosure — the client must see a real close frame, not a bare drop", got, err)
	}
}

// flowPumpClient dials an attach on the gated runner and returns the client, the
// holder's session and the run, with Session.Write already parked on a paste so
// every control frame sent after it proves it is not queued behind that write.
func flowPumpClient(t *testing.T, srv *Server, gr *gatedRunner, ts *httptest.Server, runID uuid.UUID) (*websocket.Conn, *gatedSession) {
	t.Helper()
	c := dialAttach(t, ts, srv, runID, holderOwner, "")
	if mode := readAttachMode(t, c); mode.ReadOnly {
		t.Fatal("the first client was told it is read-only")
	}
	waitFor(t, "the holder's session to open", func() bool { return gr.session(0) != nil })
	return c, gr.session(0)
}

// clientFrames reads the client socket on its own goroutine (Ping needs a
// concurrent reader) and delivers each binary frame; the channel closes with the
// socket, and the read error is left in *errp before that.
func clientFrames(c *websocket.Conn, errp *error) <-chan []byte {
	out := make(chan []byte, 16)
	go func() {
		defer close(out)
		for {
			typ, data, err := c.Read(context.Background())
			if err != nil {
				*errp = err
				return
			}
			if typ == websocket.MessageBinary {
				out <- data
			}
		}
	}()
	return out
}

func recvWithin(ch <-chan []byte, d time.Duration) ([]byte, bool) {
	select {
	case b, ok := <-ch:
		return b, ok
	case <-time.After(d):
		return nil, false
	}
}

// TestAttachPump_ControlWhileWriteBlocked: with Session.Write parked on a paste,
// pause, resume, pong and close are still processed. Stopping output reads alone
// would pass the big-output case and still strand these behind the write.
func TestAttachPump_ControlWhileWriteBlocked(t *testing.T) {
	srv, gr, _, run := f5Server(t)
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	defer ts.Close()
	c, sess := flowPumpClient(t, srv, gr, ts, run.ID)
	defer close(sess.release)

	wsWrite(t, c, websocket.MessageBinary, []byte("paste"))
	waitEntered(t, sess, "the paste")

	var rerr error
	frames := clientFrames(c, &rerr)
	wsWrite(t, c, websocket.MessageText, []byte(`{"type":"pause"}`))
	wsPing(t, c) // pong: the reader is alive behind the parked write
	go func() { _, _ = sess.w.Write([]byte("held")) }()
	if _, ok := recvWithin(frames, 300*time.Millisecond); ok {
		t.Fatal("output reached a paused client")
	}
	wsWrite(t, c, websocket.MessageText, []byte(`{"type":"resume"}`))
	if got, ok := recvWithin(frames, 3*time.Second); !ok || string(got) != "held" {
		t.Fatalf("after resume got %q ok=%v, want the held output", got, ok)
	}

	// close: the handshake completes while the write is still parked.
	if err := c.Close(websocket.StatusNormalClosure, "bye"); err != nil {
		t.Fatalf("close handshake behind a blocked write: %v", err)
	}
}

// TestAttachPump_RevokeWhilePaused: a take-over decided while output is paused
// closes the displaced socket with the take-over reason.
func TestAttachPump_RevokeWhilePaused(t *testing.T) {
	srv, gr, _, run := f5Server(t)
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	defer ts.Close()
	c, sess := flowPumpClient(t, srv, gr, ts, run.ID)
	defer close(sess.release)

	var rerr error
	frames := clientFrames(c, &rerr)
	wsWrite(t, c, websocket.MessageText, []byte(`{"type":"pause"}`))
	wsPing(t, c)
	prev := srv.evictAttachHolder(run.ID)
	if prev == nil {
		t.Fatal("no holder to evict")
	}
	prev.displace(attachTakeoverReason("someone"))

	select {
	case _, ok := <-frames:
		if ok {
			t.Fatal("output reached a revoked, paused client")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the displaced socket stayed open while paused")
	}
	var ce websocket.CloseError
	if !errors.As(rerr, &ce) || ce.Code != websocket.StatusPolicyViolation {
		t.Fatalf("read after a revocation while paused = %v, want a 1008 close", rerr)
	}
}

// TestAttachPump_PauseStall: a pause never followed by a resume ends the pump
// with the reason "client stalled".
func TestAttachPump_PauseStall(t *testing.T) {
	srv, gr, rec, run := f5Server(t)
	srv.pauseLimit = 100 * time.Millisecond
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	defer ts.Close()
	c, sess := flowPumpClient(t, srv, gr, ts, run.ID)
	defer close(sess.release)
	go drainClient(c)

	wsWrite(t, c, websocket.MessageText, []byte(`{"type":"pause"}`))
	ev := waitForAudit(t, rec, run.ID, "session.detach", "success")
	if ev == nil {
		t.Fatal("no session.detach after an unresumed pause")
	}
	if !strings.Contains(string(ev.Data), `"reason":"client stalled"`) {
		t.Fatalf("detach data = %s, want reason client stalled", ev.Data)
	}
}
