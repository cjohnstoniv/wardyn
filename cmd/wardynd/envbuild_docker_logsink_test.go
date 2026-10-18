// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"strings"
	"testing"
)

// Build output is untrusted: one endless line with no newline used to grow the
// sink's pending buffer without bound inside wardynd. The pending partial line is
// now capped, and a line within the cap is still held until its newline arrives.
func TestSlogLineWriter_PendingLineIsBounded(t *testing.T) {
	w := &slogLineWriter{}
	chunk := []byte(strings.Repeat("x", 1<<10))
	for i := 0; i < 64; i++ { // 64 KiB with no newline
		if n, err := w.Write(chunk); err != nil || n != len(chunk) {
			t.Fatalf("Write = %d, %v", n, err)
		}
		if len(w.buf) > maxSlogLineBytes+len(chunk) {
			t.Fatalf("pending buffer is %d bytes after %d KiB of one line; want it capped near %d", len(w.buf), i+1, maxSlogLineBytes)
		}
	}

	buf := captureSlog(t)
	w = &slogLineWriter{}
	if _, err := w.Write([]byte("short part, ")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("then the rest\n")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "short part, then the rest") {
		t.Errorf("a line within the cap was not joined across writes: %s", buf.String())
	}
}
