// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package guestagenttest lets a test serve the guest agent's API to a guestagent.Client from an
// http.Server and listener of its own. A Client carries each connection in records and opens it with
// a hello; Listener answers the hello and unwraps the records, so the server sees the plain HTTP/2
// stream, as the agent's own port listener gives it.
//
// The guestagent package exports none of its framing, so this package has its own copy of the
// encoding. The guestagent package's tests serve their Client through Listener, so a change to one
// copy that the other lacks fails them.
package guestagenttest

import (
	"bufio"
	"errors"
	"net"
)

// As the guestagent package has them (record.go).
const (
	recData   byte = 1
	recHello  byte = 2
	maxBody        = 32 << 10
	maxRecord      = 1 + maxBody + (1+maxBody)/254 + 2
)

var errRecord = errors.New("guestagenttest: a record no session carries")

// Listener wraps ln so that each connection it accepts answers its host's hello and carries the
// session in records.
func Listener(ln net.Listener) net.Listener { return listener{ln} }

type listener struct{ net.Listener }

func (l listener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &conn{Conn: c, r: bufio.NewReaderSize(c, maxRecord)}, nil
}

// conn is one session. Its first Read waits for the host's hello and echoes it, with the host's
// version for its own: an HTTP/2 server reads the client's preface before it writes.
type conn struct {
	net.Conn
	r      *bufio.Reader
	opened bool
	body   []byte // what is left of the last data record read
}

func (c *conn) Read(p []byte) (int, error) {
	for len(c.body) == 0 {
		rec, err := c.r.ReadSlice(0)
		if err != nil {
			return 0, err
		}
		typ, body, ok := decode(rec[:len(rec)-1])
		switch {
		case ok && typ == recHello && !c.opened:
			c.opened = true
			if _, err := c.Conn.Write(encode([]byte{0}, recHello, body)); err != nil {
				return 0, err
			}
		case ok && typ == recData && c.opened:
			c.body = body
		case c.opened:
			return 0, errRecord
		} // before its hello, a connection's records are dropped, as the agent drops them
	}
	n := copy(p, c.body)
	c.body = c.body[n:]
	return n, nil
}

func (c *conn) Write(p []byte) (int, error) {
	var b []byte
	for rest := p; len(rest) > 0; {
		k := min(len(rest), maxBody)
		b, rest = encode(b, recData, rest[:k]), rest[k:]
	}
	if _, err := c.Conn.Write(b); err != nil {
		return 0, err
	}
	return len(p), nil
}

// encode appends to dst the record of typ (never 0) and body: COBS, then the 0x00 that ends it.
func encode(dst []byte, typ byte, body []byte) []byte {
	code := len(dst)
	dst = append(dst, 0, typ)
	for _, b := range body {
		if b != 0 {
			dst = append(dst, b)
			if len(dst)-code < 0xff {
				continue
			}
		}
		dst[code] = byte(len(dst) - code)
		code = len(dst)
		dst = append(dst, 0)
	}
	dst[code] = byte(len(dst) - code)
	return append(dst, 0)
}

// decode decodes rec, a record without its 0x00.
func decode(rec []byte) (typ byte, body []byte, ok bool) {
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
