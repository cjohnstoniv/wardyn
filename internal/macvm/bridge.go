// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// The relay's stream framing (internal/vmnet): a 4-byte big-endian length,
// then one Ethernet frame. The bounds match the relay's: 64 KiB of payload
// plus the 14-byte Ethernet header at most, the header at least. Lengths from
// either side outside them end the bridge.
const (
	ethHeader   = 14
	maxNetFrame = 65536 + ethHeader
	minNetFrame = ethHeader
)

var errNetFrame = errors.New("macvm: network frame length out of bounds")

// NewNetPair makes the guest NIC's datagram socket pair: one Ethernet frame
// per datagram. guest goes into the VM's FileHandle attachment; host is the
// bridge's side.
func NewNetPair() (guest, host *os.File, err error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("macvm: socketpair: %w", err)
	}
	for _, fd := range fds {
		unix.CloseOnExec(fd)
		// Room for bursts; a failure only means the kernel default.
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, 1<<20)
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, 4<<20)
	}
	return os.NewFile(uintptr(fds[0]), "guest-nic"), os.NewFile(uintptr(fds[1]), "guest-nic-host"), nil
}

// Bridge joins the guest NIC's host-side datagram socket to the attach stream
// of the run's vmnet container: guest datagrams go out as length-prefixed
// frames on the container's stdin, and the container's stdout, demultiplexed
// from Docker's 8-byte stream headers, comes back as datagrams.
type Bridge struct {
	Guest  io.ReadWriter // one frame per Read/Write (the datagram socket)
	Attach io.ReadWriter // the hijacked attach connection
	// Stderr gets the container's stderr bytes (the relay's log), if set.
	Stderr io.Writer
}

// Run moves frames both ways until either side ends, and returns why. The
// caller closes both sides afterwards, which ends the other direction.
func (b *Bridge) Run() error {
	errc := make(chan error, 2)
	go func() { errc <- b.guestToRelay() }()
	go func() { errc <- b.relayToGuest() }()
	return <-errc
}

func (b *Bridge) guestToRelay() error {
	buf := make([]byte, maxNetFrame+1)
	out := make([]byte, 4+maxNetFrame)
	for {
		n, err := b.Guest.Read(buf)
		if err != nil {
			return fmt.Errorf("macvm: guest nic: %w", err)
		}
		if n < minNetFrame || n > maxNetFrame {
			return fmt.Errorf("%w: %d from the guest", errNetFrame, n)
		}
		binary.BigEndian.PutUint32(out, uint32(n))
		copy(out[4:], buf[:n])
		if _, err := b.Attach.Write(out[:4+n]); err != nil {
			return fmt.Errorf("macvm: relay attach: %w", err)
		}
	}
}

func (b *Bridge) relayToGuest() error {
	r := &dockerDemux{r: b.Attach, stderr: b.Stderr}
	buf := make([]byte, maxNetFrame)
	var hdr [4]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return fmt.Errorf("macvm: relay attach: %w", err)
		}
		n := binary.BigEndian.Uint32(hdr[:])
		if n < minNetFrame || n > maxNetFrame {
			return fmt.Errorf("%w: %d from the relay", errNetFrame, n)
		}
		if _, err := io.ReadFull(r, buf[:n]); err != nil {
			return fmt.Errorf("macvm: relay attach: %w", err)
		}
		if _, err := b.Guest.Write(buf[:n]); err != nil {
			return fmt.Errorf("macvm: guest nic: %w", err)
		}
	}
}

// Docker's multiplexed attach stream (no TTY): each chunk is an 8-byte header
// (stream type, three zero bytes, a 4-byte big-endian size) then the bytes.
const (
	streamStdout = 1
	streamStderr = 2
	// maxStreamChunk bounds one chunk's size, so a bad header cannot make the
	// helper allocate or skip without limit.
	maxStreamChunk = 1 << 20
)

// dockerDemux reads the stdout bytes of a multiplexed stream; stderr chunks
// go to stderr (or are dropped), and any other stream type ends the stream.
type dockerDemux struct {
	r      io.Reader
	stderr io.Writer
	left   int // stdout bytes left in the current chunk
}

func (d *dockerDemux) Read(p []byte) (int, error) {
	for d.left == 0 {
		var h [8]byte
		if _, err := io.ReadFull(d.r, h[:]); err != nil {
			return 0, err
		}
		size := binary.BigEndian.Uint32(h[4:])
		if size > maxStreamChunk {
			return 0, fmt.Errorf("macvm: attach stream chunk of %d bytes", size)
		}
		switch h[0] {
		case streamStdout:
			d.left = int(size)
		case streamStderr:
			w := d.stderr
			if w == nil {
				w = io.Discard
			}
			if _, err := io.CopyN(w, d.r, int64(size)); err != nil {
				return 0, err
			}
		default:
			return 0, fmt.Errorf("macvm: attach stream type %d", h[0])
		}
	}
	if len(p) > d.left {
		p = p[:d.left]
	}
	n, err := d.r.Read(p)
	d.left -= n
	return n, err
}
