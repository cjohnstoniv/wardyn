// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package vmnet

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
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

// prepRelay is a Prep relay logging to log.
func prepRelay(t *testing.T) (*Relay, *lockedLog) {
	t.Helper()
	var log lockedLog
	rl, err := New(Config{Prep: true, Log: slog.New(slog.NewJSONHandler(&log, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return rl, &log
}

// serveNIC runs one datagram serve of host; its result arrives on the channel.
func serveNIC(t *testing.T, rl *Relay, host *os.File, write func(d *datagramNIC) func([]byte) (int, error)) (*datagramNIC, chan error) {
	t.Helper()
	d, err := rl.newDatagramNIC(host)
	if err != nil {
		t.Fatal(err)
	}
	if write != nil {
		d.write = write(d)
	}
	served := make(chan error, 1)
	go func() { served <- d.serve() }()
	return d, served
}

// dhcpRoundTrip sends a DISCOVER with xid and reads datagrams until its reply.
func dhcpRoundTrip(t *testing.T, guest int, xid uint32) error {
	t.Helper()
	if _, err := unix.Write(guest, dhcpRequestFrame(dhcpDiscover, xid)); err != nil {
		return err
	}
	buf := make([]byte, maxFrame+1)
	want := binary.BigEndian.AppendUint32(nil, xid)
	for {
		n, err := readGuest(guest, buf)
		if err != nil {
			return fmt.Errorf("no DHCP reply for xid %#x: %w", xid, err)
		}
		if r := parseReply(t, buf[:n]); r.msgType == dhcpOffer && bytes.Equal(r.xid, want) {
			return nil
		}
	}
}

func ended(t *testing.T, served chan error) error {
	t.Helper()
	select {
	case err := <-served:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the serve did not end")
		return nil
	}
}

// TestServeDatagramDropsBadDatagramsAndAnswersDHCP: a zero-length datagram,
// a runt and one longer than the largest frame are each dropped and
// reported, the relay keeps serving, and the next DHCP request gets its
// reply. Close ends the serve with nil and leaves the caller's nic open.
func TestServeDatagramDropsBadDatagramsAndAnswersDHCP(t *testing.T) {
	rl, log := prepRelay(t)
	guest, host := datagramPair(t)
	served := make(chan error, 1)
	go func() { served <- rl.ServeDatagram(host) }()

	for _, n := range []int{0, minFrame - 1, maxFrame + 1} {
		if _, err := unix.Write(guest, make([]byte, n)); err != nil {
			t.Fatalf("%d-byte datagram: %v", n, err)
		}
	}
	if err := dhcpRoundTrip(t, guest, 0x1234); err != nil {
		t.Fatalf("%v (log %s)", err, log.String())
	}
	if got := rl.DHCPRequests(); got != 1 {
		t.Fatalf("DHCPRequests = %d; want 1", got)
	}
	if !strings.Contains(log.String(), `"detail":"frame_length"`) {
		t.Fatalf("the bad datagrams were not reported: %s", log.String())
	}

	rl.Close()
	if err := ended(t, served); err != nil {
		t.Fatalf("ServeDatagram after Close: %v", err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(host.Fd()), &st); err != nil {
		t.Fatalf("the caller's nic was closed: %v", err)
	}
}

// TestServeDatagramEndsAfterZeroReadsInARow: fewer than maxZeroReads empty
// datagrams are stray frames; maxZeroReads in a row mean the guest's end is
// gone, and the serve ends with an error after exactly that many reads.
func TestServeDatagramEndsAfterZeroReadsInARow(t *testing.T) {
	rl, log := prepRelay(t)
	defer rl.Close()
	guest, host := datagramPair(t)
	d, served := serveNIC(t, rl, host, nil)
	for range maxZeroReads - 1 {
		_, _ = unix.Write(guest, nil)
	}
	if err := dhcpRoundTrip(t, guest, 1); err != nil {
		t.Fatal(err)
	}
	for range maxZeroReads {
		_, _ = unix.Write(guest, nil)
	}
	err := ended(t, served)
	if err == nil || !strings.Contains(err.Error(), "zero-length reads in a row") {
		t.Fatalf("serve ended with %v", err)
	}
	// maxZeroReads-1 strays, the DHCP request, then maxZeroReads in a row.
	if got := d.reads.Load(); got != 2*maxZeroReads {
		t.Fatalf("%d reads; want %d", got, 2*maxZeroReads)
	}
	if !strings.Contains(log.String(), "the guest nic ended") {
		t.Fatalf("the end was not logged: %s", log.String())
	}
}

// TestServeDatagramEndsWhenGuestCloses: once the guest's end is closed, the
// next frame out (here the stack's dial of the guest) is refused, and the
// serve ends with that error without spinning on reads.
func TestServeDatagramEndsWhenGuestCloses(t *testing.T) {
	rl, _ := prepRelay(t)
	defer rl.Close()
	guest, host := datagramPair(t)
	d, served := serveNIC(t, rl, host, nil)
	if err := dhcpRoundTrip(t, guest, 2); err != nil {
		t.Fatal(err)
	}
	_ = unix.Close(guest)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _, _ = rl.DialGuest(ctx, 22) }()
	err := ended(t, served)
	if err == nil {
		t.Fatal("the serve ended with nil after the guest's end closed")
	}
	t.Logf("the serve ended with: %v", err)
	if got := d.reads.Load(); got > maxZeroReads+2 {
		t.Fatalf("%d reads: the serve spun on the closed peer", got)
	}
}

// TestServeDatagramDropsOnFullGuestBuffer: an outbound frame the guest's
// full receive buffer refuses (ENOBUFS, as macOS returns) is dropped and
// reported, and the writer goes on: the next reply arrives.
func TestServeDatagramDropsOnFullGuestBuffer(t *testing.T) {
	rl, log := prepRelay(t)
	defer rl.Close()
	guest, host := datagramPair(t)
	var calls int
	d, served := serveNIC(t, rl, host, func(d *datagramNIC) func([]byte) (int, error) {
		return func(b []byte) (int, error) {
			if calls++; calls == 1 {
				return 0, &os.PathError{Op: "write", Path: "guest-nic-host", Err: unix.ENOBUFS}
			}
			return d.f.Write(b)
		}
	})
	if _, err := unix.Write(guest, dhcpRequestFrame(dhcpDiscover, 3)); err != nil {
		t.Fatal(err)
	}
	if err := dhcpRoundTrip(t, guest, 4); err != nil {
		t.Fatalf("%v (log %s)", err, log.String())
	}
	if d.dropped.Load() != 1 || !strings.Contains(log.String(), `"detail":"guest_full"`) {
		t.Fatalf("dropped %d, log %s", d.dropped.Load(), log.String())
	}
	select {
	case err := <-served:
		t.Fatalf("the serve ended: %v", err)
	default:
	}
}

// TestDialGuestFailureIsNilConn: a dial the guest never answers returns an
// error and a nil net.Conn, never one wrapping a nil *gonet.TCPConn whose
// Close panics.
func TestDialGuestFailureIsNilConn(t *testing.T) {
	rl, _ := prepRelay(t)
	defer rl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	c, err := rl.DialGuest(ctx, 22)
	if err == nil || c != nil {
		t.Fatalf("DialGuest = %#v, %v; want nil and an error", c, err)
	}
}

// readGuest is a blocking unix.Read on the guest end, retried on EINTR: the Go runtime's preemption
// signal interrupts a raw read, which the relay's os.File would retry.
func readGuest(fd int, buf []byte) (int, error) {
	for {
		n, err := unix.Read(fd, buf)
		if !errors.Is(err, unix.EINTR) {
			return n, err
		}
	}
}

// TestDatagramWriterBuffersPartialFrame: a frame split across stream writes
// goes out as one datagram once it is whole, and not before.
func TestDatagramWriterBuffersPartialFrame(t *testing.T) {
	rl, _ := prepRelay(t)
	defer rl.Close()
	guest, host := datagramPair(t)
	d, err := rl.newDatagramNIC(host)
	if err != nil {
		t.Fatal(err)
	}
	pr, w := io.Pipe()
	go d.writeFrames(pr)
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
	n, err := readGuest(guest, buf)
	if err != nil || !bytes.Equal(buf[:n], frame) {
		t.Fatalf("datagram %d bytes, %v; want the %d-byte frame", n, err, len(frame))
	}
	if n, _, err := unix.Recvfrom(guest, buf, unix.MSG_DONTWAIT); !errors.Is(err, unix.EAGAIN) {
		t.Fatalf("a second datagram went out: %d bytes, %v", n, err)
	}
}
