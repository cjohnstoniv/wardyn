// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"net/http/httptest"
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
	ts := httptest.NewServer(srv.Handler())
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
	ts := httptest.NewServer(srv.Handler())
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
