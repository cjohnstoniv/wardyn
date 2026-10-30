// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

// Every host connection opens with a hello: a record (record.go) whose body is helloVersion and a
// fresh nonce. The port is one byte stream for every host that connects to it in turn, and the agent
// may never read the end-of-file between two of them (a host that redials at once), so the hello is
// what tells the agent where a session starts. The agent ends the writes of the session before it,
// then answers with the same record carrying its own version, and the host drops every record that
// comes before that echo. A session's bytes travel only inside data records, so nothing a session
// carries can be taken for a hello or an echo.
//
// The nonce marks a session boundary and nothing more: it is not what authenticates the guest. The
// helper's pairing does that (pair.go): before a connection can carry a hello, the guest has proved it
// holds the base's pairing key, which only the guest agent can read.
const (
	nonceLen     = 16
	helloLen     = 1 + nonceLen // helloVersion, then the nonce
	helloVersion = byte(1)      // a later protocol negotiates on it; until then a mismatch is refused
)

// helloBounds bound a host's wait for the agent's echo: the guest is not trusted, so neither a guest
// that streams bytes nor one that never answers holds a dial open.
type helloBounds struct {
	wait     time.Duration // for the echo, per dial
	bytes    int           // read before the echo, per dial
	attempts int           // dials before a guest that does not answer is refused
}

// hostHello is runnerd's. A running agent answers at once, or after its port reader's 200 ms idle
// wait, and what precedes the echo is an earlier session's last writes, far under the byte bound. A
// hello can be lost (the agent drops one whose host left in the middle of it), so a silent guest is
// dialled again: the HTTP/2 transport does not retry a failed dial.
var hostHello = helloBounds{wait: 5 * time.Second, bytes: 1 << 20, attempts: 3}

var (
	errHelloTimeout = errors.New("guestagent: the guest agent did not answer the session handshake")
	errHelloBytes   = errors.New("guestagent: the guest agent kept sending without answering the session handshake")
	errHelloVersion = errors.New("guestagent: the guest agent answered the session handshake with another protocol version")
)

// dialSession dials the agent and opens a session on the connection, which then carries the session
// in records. A guest that does not answer in time is dialled again, up to b.attempts; any other
// failure is final.
func dialSession(ctx context.Context, dial func(context.Context) (net.Conn, error), b helloBounds) (net.Conn, error) {
	var err error
	for range b.attempts {
		var conn net.Conn
		if conn, err = dial(ctx); err != nil {
			return nil, err
		}
		rc := newRecordConn(conn)
		if err = rc.open(ctx, b); err == nil {
			return rc, nil
		}
		_ = conn.Close()
		if !errors.Is(err, errHelloTimeout) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("%w, on %d dials", err, b.attempts)
}

// open sends a hello with a fresh nonce and reads up to the agent's echo of it. The records before
// the echo are an earlier session's (its last writes, or the echo of a hello whose host gave up) and
// are dropped. The echo only says where this session's bytes start; the bounds hold whatever the
// guest sends.
func (c *recordConn) open(ctx context.Context, b helloBounds) error {
	hello := make([]byte, helloLen)
	hello[0] = helloVersion
	_, _ = rand.Read(hello[1:]) // crypto/rand.Read does not return an error: it crashes the program instead
	_ = c.SetDeadline(time.Now().Add(b.wait))
	stop := context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Unix(1, 0)) })
	err := c.handshake(hello, b.bytes)
	if !stop() {
		return fmt.Errorf("guestagent: session handshake: %w", ctx.Err())
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return fmt.Errorf("%w within %s", errHelloTimeout, b.wait)
	}
	if err == nil {
		err = c.SetDeadline(time.Time{})
	}
	return err
}

// handshake writes hello and reads records up to its echo, at most limit bytes of them. Only a hello
// record carrying this hello's nonce is the echo; every other record is dropped.
func (c *recordConn) handshake(hello []byte, limit int) error {
	// The 0x00 ahead of the hello ends a record an earlier host left unfinished on the port.
	if _, err := c.Conn.Write(appendRecord([]byte{0}, recHello, hello)); err != nil {
		return err
	}
	for read := 0; read < limit; {
		rec, err := c.r.ReadSlice(0)
		read += len(rec)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue // longer than any record, so not the echo
		}
		if err != nil {
			return err
		}
		typ, echo, ok := decodeRecord(rec[:len(rec)-1])
		if !ok || typ != recHello || len(echo) != helloLen || !bytes.Equal(echo[1:], hello[1:]) {
			continue
		}
		if echo[0] != helloVersion {
			return fmt.Errorf("%w (%d, not %d)", errHelloVersion, echo[0], helloVersion)
		}
		return nil
	}
	return fmt.Errorf("%w (over %d bytes)", errHelloBytes, limit)
}
