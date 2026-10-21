// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
)

// flock is a unix call, so this test lives apart from spool_sink_test.go: that file has no build
// constraint and a Windows vet of the package must compile it (#3424).
// TestSpoolSinkAcceptIsBounded: a spool whose file lock never frees refuses that hello within the
// bound and closes it, instead of holding the sink's lock; once the lock frees, a new hello is accepted.
func TestSpoolSinkAcceptIsBounded(t *testing.T) {
	old := sinkAcceptBound
	sinkAcceptBound = 200 * time.Millisecond
	t.Cleanup(func() { sinkAcceptBound = old })
	h := newSinkHarness(t)
	lock, err := os.OpenFile(filepath.Join(h.dir, "spools", h.id.String(), "lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	stuck := h.dial(h.key, inst)
	var ne net.Error
	if f, err := read(t, stuck); err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		t.Fatalf("a hello on a stuck spool was not refused within the bound: %+v, %v", f, err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	mustRead(t, h.dial(h.key, inst), proxy.FrameAccept)
}
