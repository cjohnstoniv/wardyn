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

	"golang.org/x/sys/unix"
)

// ServeDatagram is Serve over a datagram socket that carries one Ethernet
// frame per datagram, such as the socketpair a Virtualization.framework
// FileHandle attachment takes. A datagram outside frame.go's bounds is
// dropped and reported (dropped-frame, frame_length), and serving goes on.
// A zero-length datagram reads as end of file and ends it.
//
// nic stays the caller's to close. The relay works on a nonblocking duplicate
// of it, which Close closes, so Close ends ServeDatagram. The duplicate shares
// nic's open file, so nic is left nonblocking too.
func (rl *Relay) ServeDatagram(nic *os.File) error {
	f, err := dupNonblock(nic)
	if err != nil {
		return err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-rl.stop:
		case <-done:
		}
		_ = f.Close()
	}()
	w := newDatagramWriter(f)
	defer w.Close()
	return rl.ServeStream(&datagramReader{rl: rl, f: f}, w)
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

// datagramReader turns the guest's datagrams into the stream framing
// ServeStream reads, one writeFrame per datagram.
type datagramReader struct {
	rl      *Relay
	f       *os.File
	buf     []byte // one datagram, one byte longer than the largest frame
	pending bytes.Buffer
}

func (r *datagramReader) Read(p []byte) (int, error) {
	if r.buf == nil {
		r.buf = make([]byte, maxFrame+1)
	}
	for r.pending.Len() == 0 {
		n, err := r.f.Read(r.buf)
		if err != nil {
			// Close closed the duplicate: the guest's side has ended.
			if errors.Is(err, os.ErrClosed) {
				return 0, io.EOF
			}
			return 0, err
		}
		// n > maxFrame: the datagram filled the buffer, so it was longer
		// than any frame and read(2) cut it.
		if n < minFrame || n > maxFrame {
			r.rl.rep.refuse(refusal{Reason: "dropped-frame", Detail: "frame_length"})
			continue
		}
		_ = writeFrame(&r.pending, r.buf[:n])
	}
	return r.pending.Read(p)
}

// newDatagramWriter takes the relay's stream framing and writes each whole
// frame as one datagram. A partial frame waits in the pipe for the rest; a
// length frame.go refuses ends the writer.
func newDatagramWriter(f *os.File) *io.PipeWriter {
	pr, pw := io.Pipe()
	go func() {
		buf := make([]byte, maxFrame)
		for {
			frame, err := readFrame(pr, buf)
			if err == nil {
				_, err = f.Write(frame)
			}
			if err != nil {
				_ = pr.CloseWithError(err)
				return
			}
		}
	}()
	return pw
}
