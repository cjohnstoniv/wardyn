// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestPlanVia: --via on an OS with the local transport and on Windows, which has none.
func TestPlanVia(t *testing.T) {
	for _, tc := range []struct {
		via       string
		localOS   bool
		try, must bool
		err       string
	}{
		{"", true, true, false, ""},
		{viaAuto, true, true, false, ""},
		{viaLocal, true, true, true, ""},
		{viaRelay, true, false, false, ""},
		{viaAuto, false, false, false, ""},
		{viaRelay, false, false, false, ""},
		{viaLocal, false, false, false, "local attach is not available on Windows: the runner lives in WSL2; use --via relay"},
		{"ssh", true, false, false, `--via must be auto, local or relay, not "ssh"`},
	} {
		try, must, err := planVia(tc.via, tc.localOS)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if try != tc.try || must != tc.must || got != tc.err {
			t.Errorf("planVia(%q, %v) = %v, %v, %q; want %v, %v, %q", tc.via, tc.localOS, try, must, got, tc.try, tc.must, tc.err)
		}
	}
}

// TestAttachTransportLines pins the one stderr line naming the path a session took.
func TestAttachTransportLines(t *testing.T) {
	if got, want := localAttachLine("mbp-dev"), "Attached on this computer (runner mbp-dev). Ctrl-C goes to the sandbox; close the window to detach."; got != want {
		t.Errorf("local line = %q, want %q", got, want)
	}
	if got, want := relayAttachLine("https://wardyn.example.com/base/"), "Attached through the organisation (wardyn.example.com). Ctrl-C goes to the sandbox; close the window to detach."; got != want {
		t.Errorf("relay line = %q, want %q", got, want)
	}
}

// TestLocalFrameBytes pins the socket's frame form runnerd reads: type byte, big-endian uint32 length,
// payload.
func TestLocalFrameBytes(t *testing.T) {
	var b bytes.Buffer
	if err := writeLocalFrame(&b, localHello, []byte(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	if want := []byte("h\x00\x00\x00\x07{\"v\":1}"); !bytes.Equal(b.Bytes(), want) {
		t.Errorf("frame = %q, want %q", b.Bytes(), want)
	}
	typ, p, err := readLocalFrame(&b)
	if err != nil || typ != localHello || string(p) != `{"v":1}` {
		t.Errorf("read back %q %q %v", typ, p, err)
	}
	if err := writeLocalFrame(&b, localData, make([]byte, maxLocalFrame+1)); err == nil {
		t.Error("a frame over maxLocalFrame was written")
	}
}

// TestSettleAttachEndPrefersTheClose: the input half fails first (its write hit the closed socket), then the
// output half returns the server's close; the close is what is shown.
func TestSettleAttachEndPrefersTheClose(t *testing.T) {
	writeErr := errors.New("failed to write msg: use of closed network connection")
	for _, code := range []websocket.StatusCode{4401, 4409} {
		out := make(chan error) // unbuffered: the close arrives after settleAttachEnd starts waiting
		go func() { out <- websocket.CloseError{Code: code, Reason: "session ended"} }()
		got := settleAttachEnd(writeErr, out, time.Minute)
		if websocket.CloseStatus(got) != code {
			t.Errorf("settled on %v, want the %d close", got, code)
		}
	}
	// A first end that already is a close, or a clean one, is kept without waiting.
	closeErr := websocket.CloseError{Code: 4401, Reason: "session ended"}
	if got := settleAttachEnd(closeErr, nil, time.Minute); !errors.Is(got, closeErr) {
		t.Errorf("settled on %v, want the first close", got)
	}
	if got := settleAttachEnd(nil, nil, time.Minute); got != nil {
		t.Errorf("settled on %v, want nil", got)
	}
	// An output end without a close leaves the first error.
	out := make(chan error, 1)
	out <- errors.New("read failed")
	if got := settleAttachEnd(writeErr, out, time.Minute); got != writeErr {
		t.Errorf("settled on %v, want the write error", got)
	}
}

// TestReadLocalFrameRefusesOverSize: a length over maxLocalFrame is refused before its payload is read.
func TestReadLocalFrameRefusesOverSize(t *testing.T) {
	hdr := []byte{localData, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(hdr[1:], maxLocalFrame+1)
	_, _, err := readLocalFrame(io.MultiReader(bytes.NewReader(hdr), zeroReader{}))
	if err == nil || !strings.Contains(err.Error(), "is over") {
		t.Fatalf("readLocalFrame = %v, want the over-size refusal", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
