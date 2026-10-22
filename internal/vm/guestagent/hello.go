// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

// Every host connection opens with a hello: sessionMagic, helloVersion and a fresh nonce. The port
// is one byte stream for every host that connects to it in turn, and the agent may never read the
// end-of-file between two of them (a host that redials at once), so the hello is what tells the
// agent where a session starts. The agent ends the writes of the session before it, then answers
// with the same bytes carrying its own version, and the host drops what it reads before that echo.
//
// The nonce marks a session boundary and nothing more. A guest can echo whatever it reads, so
// nothing on the host treats the echo as authentication or as a reason to trust the guest.
const (
	magicLen     = 16
	nonceLen     = 16
	helloLen     = magicLen + 1 + nonceLen
	helloVersion = byte(1) // a later protocol negotiates on it; until then a mismatch is refused
)

// sessionMagic is derived when the package loads, so that no binary holds it: a host may well pipe
// a Wardyn binary through a session's stdin, and the magic appearing there would end that session.
var sessionMagic = func() []byte {
	h := sha256.Sum256([]byte("wardyn guest agent session hello"))
	return h[:magicLen]
}()

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
	errHelloEcho    = errors.New("guestagent: the guest agent's session handshake echo is malformed")
)

// dialSession dials the agent and opens a session on the connection. A guest that does not answer in
// time is dialled again, up to b.attempts; any other failure is final.
func dialSession(ctx context.Context, dial func(context.Context) (net.Conn, error), b helloBounds) (net.Conn, error) {
	var err error
	for range b.attempts {
		var conn net.Conn
		if conn, err = dial(ctx); err != nil {
			return nil, err
		}
		if err = openSession(ctx, conn, b); err == nil {
			return conn, nil
		}
		_ = conn.Close()
		if !errors.Is(err, errHelloTimeout) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("%w, on %d dials", err, b.attempts)
}

// openSession writes a hello with a fresh nonce on conn and reads until the agent's echo of it.
// What comes first is an earlier session's (its last writes, or the echo of a hello whose host gave
// up) and is dropped. The echo only says where this session's bytes start; the bounds hold
// whatever the guest sends.
func openSession(ctx context.Context, conn net.Conn, b helloBounds) error {
	hello := make([]byte, helloLen)
	copy(hello, sessionMagic)
	hello[magicLen] = helloVersion
	_, _ = rand.Read(hello[magicLen+1:]) // crypto/rand.Read does not return an error: it crashes the program instead
	_ = conn.SetDeadline(time.Now().Add(b.wait))
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	err := exchange(conn, hello, b.bytes)
	if !stop() {
		return fmt.Errorf("guestagent: session handshake: %w", ctx.Err())
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return fmt.Errorf("%w within %s", errHelloTimeout, b.wait)
	}
	if err == nil {
		err = conn.SetDeadline(time.Time{})
	}
	return err
}

// exchange writes hello and reads up to the end of its echo, at most limit bytes before it.
func exchange(conn net.Conn, hello []byte, limit int) error {
	if _, err := conn.Write(hello); err != nil {
		return err
	}
	nonce := hello[magicLen+1:]
	buf := make([]byte, 32<<10)
	var seen []byte // the last helloLen-1 bytes read: an echo may have begun in them
	for read := 0; read < limit; {
		n, err := conn.Read(buf)
		read += n
		w := append(seen, buf[:n]...)
		if i := bytes.Index(w, nonce); i >= 0 {
			return checkEcho(w, i)
		}
		seen = append([]byte(nil), w[max(0, len(w)-(helloLen-1)):]...)
		if err != nil {
			return err
		}
	}
	return fmt.Errorf("%w (over %d bytes)", errHelloBytes, limit)
}

// checkEcho checks the echo whose nonce is at w[i:]. The agent writes nothing after its echo until
// the host speaks, so bytes after it are refused rather than dropped.
func checkEcho(w []byte, i int) error {
	start := i - 1 - magicLen
	switch {
	case start < 0 || !bytes.Equal(w[start:start+magicLen], sessionMagic):
		return errHelloEcho
	case w[i-1] != helloVersion:
		return fmt.Errorf("%w (%d, not %d)", errHelloVersion, w[i-1], helloVersion)
	case len(w) > i+nonceLen:
		return fmt.Errorf("%w: %d bytes follow it", errHelloEcho, len(w)-i-nonceLen)
	}
	return nil
}
