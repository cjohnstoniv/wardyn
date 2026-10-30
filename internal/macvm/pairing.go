// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// DesktopOpenByte is the first byte the host writes on a desktop vsock
// connection; the guest agent then dials the guest's Screen Sharing and
// splices. Any other first byte makes the guest close the connection.
const DesktopOpenByte = 0x01

// DesktopShim serves one desktop session: client is runnerd's connection on
// desktop.sock, guest the opened desktop vsock connection to Apple's Screen
// Sharing. The shim (MACVM-DESKTOP) re-presents it as plain RFB; it owns and
// closes both. With no shim, desktop.sock is not served.
type DesktopShim interface {
	ServeDesktop(client, guest net.Conn)
}

// errNoGuest is returned when no guest connection arrives in time.
var errNoGuest = errors.New("macvm: the guest agent is not connected")

// pairDeadline bounds a guest connection's proof. pairPendingMax caps the connections a port holds while
// their proofs are due; a dialer over the cap is closed at once.
var pairDeadline = 5 * time.Second

const pairPendingMax = 4

// pairer holds the guest's one idle connection on a vsock port. A connection is the guest agent's only
// once it has proved the base's pairing key (guestagent.PairProof over this port and a fresh challenge);
// until then it is neither idle nor closes the idle one, and one that fails is closed. The guest keeps
// exactly one idle connection per port and redials after a session, so a newer proven connection replaces
// (and closes) an older idle one, which is stale.
type pairer struct {
	mu      sync.Mutex
	idle    net.Conn
	gen     chan struct{} // closed and replaced whenever idle changes
	pending int           // connections whose proof is due
	closed  bool
	port    uint32
	key     []byte
	open    []byte // written to the guest connection when it is paired
	// connected is called when a guest connection has proved the key (for events).
	connected func()
	log       *slog.Logger
}

func newPairer(port uint32, key, open []byte) *pairer {
	return &pairer{gen: make(chan struct{}), port: port, key: key, open: open}
}

// acceptGuest takes guest connections from l until it is closed. Each one proves the key in its own
// goroutine, so a slow dialer never holds up the next.
func (p *pairer) acceptGuest(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		full := p.pending >= pairPendingMax
		if !full {
			p.pending++
		}
		p.mu.Unlock()
		if full {
			_ = c.Close()
			continue
		}
		go p.pair(c)
	}
}

// pair makes c the idle connection once it proves the key, and closes it otherwise.
func (p *pairer) pair(c net.Conn) {
	ok := p.proved(c)
	p.mu.Lock()
	p.pending--
	if !ok || p.closed {
		p.mu.Unlock()
		_ = c.Close()
		if !ok && p.log != nil {
			p.log.Warn("macvm: refused a guest connection without the pairing proof", "port", p.port)
		}
		return
	}
	if p.idle != nil {
		_ = p.idle.Close()
	}
	p.idle = c
	close(p.gen)
	p.gen = make(chan struct{})
	cb := p.connected
	p.mu.Unlock()
	if cb != nil {
		cb()
	}
}

// proved sends c a challenge and checks its answer, all within pairDeadline, which it then clears.
func (p *pairer) proved(c net.Conn) bool {
	frame := make([]byte, len(guestagent.PairMagic)+guestagent.PairChallengeLen)
	copy(frame, guestagent.PairMagic)
	challenge := frame[len(guestagent.PairMagic):]
	_, _ = rand.Read(challenge) // crypto/rand.Read does not return an error: it crashes the program instead
	if c.SetDeadline(time.Now().Add(pairDeadline)) != nil {
		return false
	}
	if _, err := c.Write(frame); err != nil {
		return false
	}
	got := make([]byte, sha256.Size)
	if _, err := io.ReadFull(c, got); err != nil {
		return false
	}
	return hmac.Equal(got, guestagent.PairProof(p.key, p.port, challenge)) && c.SetDeadline(time.Time{}) == nil
}

// take returns the idle guest connection, waiting for one until ctx ends.
func (p *pairer) take(ctx context.Context) (net.Conn, error) {
	for {
		p.mu.Lock()
		c, gen := p.idle, p.gen
		p.idle = nil
		p.mu.Unlock()
		if c != nil {
			if len(p.open) > 0 {
				if _, err := c.Write(p.open); err != nil {
					_ = c.Close()
					continue
				}
			}
			return c, nil
		}
		select {
		case <-gen:
		case <-ctx.Done():
			return nil, errNoGuest
		}
	}
}

func (p *pairer) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if p.idle != nil {
		_ = p.idle.Close()
		p.idle = nil
	}
}

// pairWait is how long a host connection waits for the guest's connection.
const pairWait = 30 * time.Second

// servePaired accepts host connections on l and gives each one the guest's
// idle connection, via serve. A host connection with no guest is closed.
func servePaired(l net.Listener, p *pairer, serve func(host, guest net.Conn)) {
	for {
		h, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), pairWait)
			g, err := p.take(ctx)
			cancel()
			if err != nil {
				_ = h.Close()
				return
			}
			serve(h, g)
		}()
	}
}

// splice copies both ways and closes both when either side ends.
func splice(a, b net.Conn) {
	var once sync.Once
	done := func() {
		once.Do(func() {
			_ = a.Close()
			_ = b.Close()
		})
	}
	go func() { _, _ = io.Copy(a, b); done() }()
	_, _ = io.Copy(b, a)
	done()
}
