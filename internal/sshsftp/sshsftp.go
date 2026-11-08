// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package sshsftp reads the sftp packet framing (a 4-byte length, then the packet) on a byte stream the SSH
// gateway relays, without decoding the protocol: a counter of the client's requests and a guard on the
// sandbox's replies.
package sshsftp

import (
	"encoding/binary"
	"errors"
	"io"
)

// LengthSize is the size of a packet's length field.
const LengthSize = 4

// TypeData is SSH_FXP_DATA, the reply that carries file content.
const TypeData = 103

// Framer splits a byte stream into sftp packets. The zero value is ready.
type Framer struct {
	head []byte // the current packet's first bytes, its length included, up to the peek
	left uint32 // bytes of the current packet not yet seen after its length
}

// Feed walks p. For each run of bytes that belongs to one packet it calls piece with those bytes, the
// packet's first bytes so far (its length included, at most peek of them) and whether the packet ended with
// this run. A packet of length 0 has no body and ends with its length.
func (f *Framer) Feed(p []byte, peek int, piece func(bytes, head []byte, done bool)) {
	for len(p) > 0 {
		if len(f.head) < LengthSize {
			take := min(LengthSize-len(f.head), len(p))
			f.head = append(f.head, p[:take]...)
			empty := false
			if len(f.head) == LengthSize {
				f.left = binary.BigEndian.Uint32(f.head)
				empty = f.left == 0
			}
			piece(p[:take], f.head, empty)
			if empty {
				f.head = f.head[:0]
			}
			p = p[take:]
			continue
		}
		take := int(min(uint64(f.left), uint64(len(p))))
		if room := peek - len(f.head); room > 0 {
			f.head = append(f.head, p[:min(room, take)]...)
		}
		f.left -= uint32(take)
		done := f.left == 0
		piece(p[:take], f.head, done)
		if done {
			f.head = f.head[:0]
		}
		p = p[take:]
	}
}

// ErrData is the error a ReplyGuard returns in place of a file-content reply.
var ErrData = errors.New("sftp file content blocked by the run's data_out rule")

// ReplyGuard relays the sftp replies of R and stops at the first file-content reply (SSH_FXP_DATA): it
// returns ErrData from Read, calls OnData once, and passes none of that packet's bytes on. A packet's bytes
// are held until its type is known (its first 5 bytes), so a DATA packet split across reads leaks nothing.
// Every other reply passes unchanged. After ErrData every Read returns it.
type ReplyGuard struct {
	R      io.Reader
	OnData func()

	f       Framer
	buf     []byte
	held    []byte // the current packet's bytes, held until its type is known
	decided bool   // the current packet's type is known and it is not DATA
	out     []byte // released bytes not yet read
	err     error
}

// Read implements io.Reader.
func (g *ReplyGuard) Read(p []byte) (int, error) {
	if g.buf == nil {
		g.buf = make([]byte, 32<<10)
	}
	for len(g.out) == 0 && g.err == nil {
		n, err := g.R.Read(g.buf)
		g.f.Feed(g.buf[:n], LengthSize+1, func(bytes, head []byte, done bool) {
			if g.err != nil {
				return
			}
			if g.decided {
				g.out = append(g.out, bytes...)
			} else if g.held = append(g.held, bytes...); len(head) > LengthSize || done {
				if len(head) > LengthSize && head[LengthSize] == TypeData {
					g.err, g.held = ErrData, nil
					if g.OnData != nil {
						g.OnData()
					}
					return
				}
				g.out, g.held, g.decided = append(g.out, g.held...), g.held[:0], true
			}
			if done {
				g.decided = false
			}
		})
		if g.err == nil && err != nil {
			g.out = append(g.out, g.held...) // a partial packet at the end of the stream
			g.held, g.err = nil, err
		}
	}
	if len(g.out) > 0 {
		n := copy(p, g.out)
		g.out = g.out[n:]
		return n, nil
	}
	return 0, g.err
}
