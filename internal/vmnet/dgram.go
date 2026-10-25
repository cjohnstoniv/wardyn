// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package vmnet

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// maxZeroReads is how many zero-length datagrams in a row mean the guest's
// end is gone rather than one stray empty frame: on some kernels a datagram
// socket whose peer closed reads zero bytes over and over.
const maxZeroReads = 8

// ServeDatagram is Serve over a datagram socket that carries one Ethernet
// frame per datagram, such as the socketpair a Virtualization.framework
// FileHandle attachment takes. One bad frame never ends it: a datagram
// outside frame.go's bounds (a zero-length one included) is dropped and
// reported (dropped-frame, frame_length), and an outbound frame the guest's
// full receive buffer refuses (ENOBUFS) is dropped and reported
// (dropped-frame, guest_full). It ends with nil on Close, and with a logged
// error when the guest's end is gone: maxZeroReads zero-length reads in a
// row, a read error, or a write error other than a full buffer (a closed
// peer refuses writes).
//
// nic stays the caller's to close. The relay works on a nonblocking duplicate
// of it, which Close closes, so Close ends ServeDatagram. The duplicate shares
// nic's open file, so nic is left nonblocking too.
func (rl *Relay) ServeDatagram(nic *os.File) error {
	d, err := rl.newDatagramNIC(nic)
	if err != nil {
		return err
	}
	return d.serve()
}

// datagramNIC is one ServeDatagram: it reads the guest's datagrams as the
// stream framing ServeStream takes, and writes the stream's frames back as
// datagrams.
type datagramNIC struct {
	rl *Relay
	f  *os.File
	// write is f.Write; a seam for tests.
	write func([]byte) (int, error)

	buf     []byte // one datagram, one byte longer than the largest frame
	pending bytes.Buffer
	zeros   int // zero-length reads in a row

	reads   atomic.Uint64 // datagram reads, for tests
	dropped atomic.Uint64 // outbound frames dropped on a full guest buffer

	failOnce sync.Once
	mu       sync.Mutex
	err      error // why the guest's end is gone; nil after Close
}

func (rl *Relay) newDatagramNIC(nic *os.File) (*datagramNIC, error) {
	f, err := dupNonblock(nic)
	if err != nil {
		return nil, err
	}
	return &datagramNIC{rl: rl, f: f, write: f.Write, buf: make([]byte, maxFrame+1)}, nil
}

func (d *datagramNIC) serve() error {
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-d.rl.stop:
		case <-done:
		}
		d.fail(nil)
	}()
	pr, pw := io.Pipe()
	defer pw.Close()
	go d.writeFrames(pr)
	return d.rl.ServeStream(d, pw)
}

// fail ends the serve once: it records why (nil for Close) and closes the
// duplicate, which ends a blocked read.
func (d *datagramNIC) fail(err error) {
	d.failOnce.Do(func() {
		if err != nil {
			d.rl.log.Warn("vmnet: the guest nic ended", "err", err.Error())
		}
		d.mu.Lock()
		d.err = err
		d.mu.Unlock()
		_ = d.f.Close()
	})
}

func (d *datagramNIC) failure() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

// Read is the guest's datagrams in the stream framing, one writeFrame each.
func (d *datagramNIC) Read(p []byte) (int, error) {
	for d.pending.Len() == 0 {
		d.reads.Add(1)
		n, err := d.f.Read(d.buf)
		switch {
		case errors.Is(err, io.EOF): // how os.File reads a zero-length datagram
			n = 0
		case errors.Is(err, os.ErrClosed): // fail closed the duplicate
			if e := d.failure(); e != nil {
				return 0, e
			}
			return 0, io.EOF
		case err != nil: // EAGAIN and EINTR are retried inside os.File
			e := fmt.Errorf("vmnet: guest nic: %w", err)
			d.fail(e)
			return 0, e
		}
		if n == 0 {
			if d.zeros++; d.zeros >= maxZeroReads {
				e := fmt.Errorf("vmnet: guest nic: %d zero-length reads in a row; the guest's end is gone", d.zeros)
				d.fail(e)
				return 0, e
			}
			d.rl.rep.refuse(refusal{Reason: "dropped-frame", Detail: "frame_length"})
			continue
		}
		d.zeros = 0
		// n > maxFrame: the datagram filled the buffer, so it was longer
		// than any frame and read(2) cut it.
		if n < minFrame || n > maxFrame {
			d.rl.rep.refuse(refusal{Reason: "dropped-frame", Detail: "frame_length"})
			continue
		}
		_ = writeFrame(&d.pending, d.buf[:n])
	}
	return d.pending.Read(p)
}

// writeFrames takes the relay's stream framing from pr and writes each whole
// frame as one datagram; a partial frame waits in the pipe for the rest. A
// full guest buffer drops that one frame, as a NIC would.
func (d *datagramNIC) writeFrames(pr *io.PipeReader) {
	buf := make([]byte, maxFrame)
	for {
		frame, err := readFrame(pr, buf)
		if errors.Is(err, io.EOF) { // the serve ended
			return
		}
		if err == nil {
			_, err = d.write(frame)
			if errors.Is(err, unix.ENOBUFS) || errors.Is(err, unix.EAGAIN) {
				d.dropped.Add(1)
				d.rl.rep.refuse(refusal{Reason: "dropped-frame", Detail: "guest_full"})
				continue
			}
			if err != nil {
				err = fmt.Errorf("vmnet: guest nic: %w", err)
			}
		}
		if err != nil {
			d.fail(err)
			_ = pr.CloseWithError(err)
			return
		}
	}
}

func dupNonblock(nic *os.File) (*os.File, error) {
	sc, err := nic.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("vmnet: guest nic: %w", err)
	}
	fd, derr := -1, error(nil)
	if err := sc.Control(func(s uintptr) { fd, derr = unix.Dup(int(s)) }); err != nil {
		return nil, fmt.Errorf("vmnet: guest nic: %w", err)
	}
	if derr != nil {
		return nil, fmt.Errorf("vmnet: guest nic: dup: %w", derr)
	}
	unix.CloseOnExec(fd)
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("vmnet: guest nic: %w", err)
	}
	return os.NewFile(uintptr(fd), nic.Name()), nil
}
