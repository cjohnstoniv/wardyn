// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bufio"
	"errors"
	"fmt"
	"net"
)

// Every byte of a host connection to the agent travels in records, both ways: a type byte and a
// body, encoded with COBS (consistent overhead byte stuffing) so that no 0x00 is in one, each ended
// by a 0x00. A session's own bytes are only ever the body of a data record, so whatever a workload
// puts in them, they cannot form the records that mark where sessions start (a host's hello and the
// agent's echo of it). A reader that has lost its place in the stream finds it at the next 0x00.
const (
	recData  byte = 1 // the body is session bytes
	recHello byte = 2 // the body is a hello (hello.go)

	maxBody   = 32 << 10                          // a write is cut into records of at most this much body
	maxRecord = 1 + maxBody + (1+maxBody)/254 + 2 // the longest record, its 0x00 included
)

var errRecord = errors.New("guestagent: the guest agent sent what is not a session's record")

// appendRecord appends to dst the record of typ (never 0) and body.
func appendRecord(dst []byte, typ byte, body []byte) []byte {
	code := len(dst) // the group's code byte: one more than the bytes in the group
	dst = append(dst, 0, typ)
	for _, b := range body {
		if b != 0 {
			dst = append(dst, b)
			if len(dst)-code < 0xff {
				continue
			}
		}
		dst[code] = byte(len(dst) - code) // a 0x00, or a full group of 254 bytes, ends the group
		code = len(dst)
		dst = append(dst, 0)
	}
	dst[code] = byte(len(dst) - code)
	return append(dst, 0)
}

// appendData appends p to dst as data records.
func appendData(dst, p []byte) []byte {
	for len(p) > 0 {
		k := min(len(p), maxBody)
		dst, p = appendRecord(dst, recData, p[:k]), p[k:]
	}
	return dst
}

// decodeRecord decodes rec, a record without its 0x00; ok is false if rec is not one.
func decodeRecord(rec []byte) (typ byte, body []byte, ok bool) {
	out := make([]byte, 0, len(rec))
	for i := 0; i < len(rec); {
		n := int(rec[i])
		if n == 0 || i+n > len(rec) {
			return 0, nil, false
		}
		out = append(out, rec[i+1:i+n]...)
		if i += n; n < 0xff && i < len(rec) {
			out = append(out, 0)
		}
	}
	if len(out) == 0 {
		return 0, nil, false
	}
	return out[0], out[1:], true
}

// recordConn is runnerd's end of a session. What it writes goes in data records, and once its hello
// has been answered (hello.go) it reads nothing but the bodies of data records, which it never looks
// inside.
type recordConn struct {
	net.Conn
	r    *bufio.Reader
	body []byte // what is left of the last data record read
}

func newRecordConn(c net.Conn) *recordConn {
	return &recordConn{Conn: c, r: bufio.NewReaderSize(c, maxRecord)}
}

func (c *recordConn) Write(p []byte) (int, error) {
	if _, err := c.Conn.Write(appendData(nil, p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *recordConn) Read(p []byte) (int, error) {
	for len(c.body) == 0 {
		rec, err := c.r.ReadSlice(0)
		if errors.Is(err, bufio.ErrBufferFull) {
			return 0, fmt.Errorf("%w: over %d bytes", errRecord, maxRecord)
		}
		if err != nil {
			return 0, err
		}
		typ, body, ok := decodeRecord(rec[:len(rec)-1])
		if !ok || typ != recData {
			return 0, errRecord
		}
		c.body = body
	}
	n := copy(p, c.body)
	c.body = c.body[n:]
	return n, nil
}
