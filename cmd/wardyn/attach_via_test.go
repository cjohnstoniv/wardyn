// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"testing"
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
