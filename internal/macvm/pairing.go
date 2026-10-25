// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"
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

// pairer holds the guest's one idle connection on a vsock port. The guest
// keeps exactly one idle connection per port and redials after a session, so
// a newer connection replaces (and closes) an older idle one, which is stale.
type pairer struct {
	mu   sync.Mutex
	idle net.Conn
	gen  chan struct{} // closed and replaced whenever idle changes
	open []byte        // written to the guest connection when it is paired
	// connected is called when a guest connection arrives (for events).
	connected func()
}

func newPairer(open []byte) *pairer { return &pairer{gen: make(chan struct{}), open: open} }

// acceptGuest takes guest connections from l until it is closed.
func (p *pairer) acceptGuest(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
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
