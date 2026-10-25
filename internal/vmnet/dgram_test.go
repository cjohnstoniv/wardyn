// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package vmnet

import (
	"bytes"
	"encoding/binary"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// datagramPair is a connected datagram socketpair, as a Virtualization.framework
// FileHandle attachment takes: guest is the test's side (raw fd, with a receive
// timeout), host the relay's.
func datagramPair(t *testing.T) (guest int, host *os.File) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range fds {
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, 1<<20)
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, 4<<20)
	}
	tv := unix.NsecToTimeval((5 * time.Second).Nanoseconds())
	if err := unix.SetsockoptTimeval(fds[0], unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		t.Fatal(err)
	}
	host = os.NewFile(uintptr(fds[1]), "guest-nic-host")
	t.Cleanup(func() { _ = unix.Close(fds[0]); _ = host.Close() })
	return fds[0], host
}

type lockedLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestServeDatagramDropsOversizedAndAnswersDHCP: a datagram longer than the
// largest frame is dropped and reported, the relay keeps serving, and the
// next DHCP request gets its reply as one datagram. Close ends the serve and
// leaves the caller's nic open.
func TestServeDatagramDropsOversizedAndAnswersDHCP(t *testing.T) {
	var log lockedLog
	rl, err := New(Config{Prep: true, Log: slog.New(slog.NewJSONHandler(&log, nil))})
	if err != nil {
		t.Fatal(err)
	}
	guest, host := datagramPair(t)
	served := make(chan error, 1)
	go func() { served <- rl.ServeDatagram(host) }()

	if _, err := unix.Write(guest, make([]byte, maxFrame+1)); err != nil {
		t.Fatalf("oversized datagram: %v", err)
	}
	if _, err := unix.Write(guest, dhcpRequestFrame(dhcpDiscover, 0x1234)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, maxFrame+1)
	n, err := unix.Read(guest, buf)
	if err != nil {
		t.Fatalf("no DHCP reply after the oversized datagram: %v (log %s)", err, log.String())
	}
	if r := parseReply(t, buf[:n]); r.msgType != dhcpOffer {
		t.Fatalf("reply type %d; want an offer", r.msgType)
	}
	if got := rl.DHCPRequests(); got != 1 {
		t.Fatalf("DHCPRequests = %d; want 1", got)
	}
	if !strings.Contains(log.String(), `"detail":"frame_length"`) {
		t.Fatalf("the oversized datagram was not reported: %s", log.String())
	}

	rl.Close()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("ServeDatagram after Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not end ServeDatagram")
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(host.Fd()), &st); err != nil {
		t.Fatalf("the caller's nic was closed: %v", err)
	}
}

// TestDatagramWriterBuffersPartialFrame: a frame split across stream writes
// goes out as one datagram once it is whole, and not before.
func TestDatagramWriterBuffersPartialFrame(t *testing.T) {
	guest, host := datagramPair(t)
	w := newDatagramWriter(host)
	defer w.Close()
	frame := bytes.Repeat([]byte{0xab}, 60)
	stream := binary.BigEndian.AppendUint32(nil, uint32(len(frame)))
	stream = append(stream, frame...)
	if _, err := w.Write(stream[:34]); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, maxFrame)
	time.Sleep(100 * time.Millisecond)
	if n, _, err := unix.Recvfrom(guest, buf, unix.MSG_DONTWAIT); !errors.Is(err, unix.EAGAIN) {
		t.Fatalf("a partial frame went out: %d bytes, %v", n, err)
	}
	if _, err := w.Write(stream[34:]); err != nil {
		t.Fatal(err)
	}
	n, err := unix.Read(guest, buf)
	if err != nil || !bytes.Equal(buf[:n], frame) {
		t.Fatalf("datagram %d bytes, %v; want the %d-byte frame", n, err, len(frame))
	}
	if n, _, err := unix.Recvfrom(guest, buf, unix.MSG_DONTWAIT); !errors.Is(err, unix.EAGAIN) {
		t.Fatalf("a second datagram went out: %d bytes, %v", n, err)
	}
}
