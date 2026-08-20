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
