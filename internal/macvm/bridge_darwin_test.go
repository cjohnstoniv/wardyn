// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"bytes"
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestBridgeSurvivesFullGuestBuffer: on macOS a datagram write to a peer
// whose receive buffer is full may fail with ENOBUFS rather than block. 2000
// relay frames into a 64 KiB guest buffer that nobody reads, then a drain:
// the bridge is still up and a later frame reaches the guest, whichever way
// the kernel behaved.
func TestBridgeSurvivesFullGuestBuffer(t *testing.T) {
	guestVM, hostNIC, err := NewNetPair()
	if err != nil {
		t.Fatal(err)
	}
	defer guestVM.Close()
	defer hostNIC.Close()
	sc, err := guestVM.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var guest int
	_ = sc.Control(func(fd uintptr) { guest = int(fd) })
	if err := unix.SetsockoptInt(guest, unix.SOL_SOCKET, unix.SO_RCVBUF, 64<<10); err != nil {
		t.Fatal(err)
	}
	relaySide, attach := net.Pipe()
	defer relaySide.Close()
	b := &Bridge{Guest: hostNIC, Attach: attach}
	errc := make(chan error, 1)
	go func() { errc <- b.Run() }()

	sent := make(chan struct{})
	go func() {
		defer close(sent)
		for range 2000 {
			if _, err := relaySide.Write(dockerChunk(streamStdout, lenFrame(frame(1500, 0xCD)))); err != nil {
				return
			}
		}
	}()
	buf := make([]byte, maxNetFrame)
	drain := func() {
		for {
			if _, _, err := unix.Recvfrom(guest, buf, unix.MSG_DONTWAIT); err != nil {
				return
			}
		}
	}
	// Either the writes were dropped (sent closes) or they block until drained.
	for done := false; !done; {
		select {
		case <-sent:
			done = true
		case <-time.After(200 * time.Millisecond):
		}
		drain()
	}
	drain()
	marker := frame(60, 0xEE)
	go func() { _, _ = relaySide.Write(dockerChunk(streamStdout, lenFrame(marker))) }()
	tv := unix.NsecToTimeval((5 * time.Second).Nanoseconds())
	_ = unix.SetsockoptTimeval(guest, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
	for {
		n, err := unix.Read(guest, buf)
		if err != nil {
			t.Fatalf("the marker frame never arrived: %v", err)
		}
		if bytes.Equal(buf[:n], marker) {
			break
		}
	}
	select {
	case err := <-errc:
		t.Fatalf("the bridge ended: %v", err)
	default:
	}
	t.Logf("frames dropped on a full guest buffer: %d", b.dropped.Load())
}
