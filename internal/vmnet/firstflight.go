// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"time"
)

// The guest's first flight on a tunnelled flow, read before the proxy is told
// to dial. A flow that opens with a TLS record must carry, in its ClientHello,
// a server name equal to the name the flow was resolved as — on every port,
// not only 443 — or it is closed before a byte leaves the relay: otherwise a
// guest could send a TLS connection for one host to another host's address
// (fronting). The ClientHello is reassembled by crypto/tls itself, so one split
// across TCP segments or TLS records, or sent slowly, is read whole up to the
// bound; past the bound or the deadline it is treated as missing.
const maxClientHello = 16 << 10

// Variables only so tests can shorten them.
var (
	helloBudget = 10 * time.Second // the whole ClientHello, from its first byte
	firstWait   = 10 * time.Second // the first byte, from the handshake
)

type flightKind int

const (
	flightNone  flightKind = iota // nothing within firstWait
	flightTLS                     // a ClientHello with a server name
	flightOther                   // bytes that are not a TLS record (an SSH banner, a database's opening)
)

var (
	errSNIMissing = errors.New("no usable TLS server name")
	errStopHello  = errors.New("client hello read")
)

// flight is what readFlight read: bytes holds every byte taken from the
// guest, which the relay forwards first once the flow is tunnelled.
type flight struct {
	kind  flightKind
	sni   string
	bytes []byte
}

// readFlight reads the guest's opening bytes from c. A TLS record is read to
// the end of its ClientHello; err is errSNIMissing when that yields no server
// name (none sent, unparseable, larger than maxClientHello, or slower than
// helloBudget).
func readFlight(c net.Conn) (flight, error) {
	first := make([]byte, 1)
	_ = c.SetReadDeadline(time.Now().Add(firstWait))
	// Every return clears it: a deadline left behind (the silent guest's, which
	// has already passed) fails the splice's first read of the guest, and the
	// relay half-closes a tunnel the guest never ended.
	defer func() { _ = c.SetReadDeadline(time.Time{}) }()
	n, err := c.Read(first)
	if n == 0 {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return flight{kind: flightNone}, nil
		}
		return flight{}, err
	}
	if first[0] != 0x16 { // not a TLS handshake record
		return flight{kind: flightOther, bytes: first}, nil
	}
	_ = c.SetReadDeadline(time.Now().Add(helloBudget))
	rec := &recordingConn{Conn: c, pending: first}
	var sni string
	srv := tls.Server(rec, &tls.Config{GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		sni = h.ServerName
		return nil, errStopHello
	}})
	_ = srv.Handshake()
	sni = strings.TrimSuffix(strings.ToLower(sni), ".")
	if sni == "" {
		return flight{bytes: rec.seen}, errSNIMissing
	}
	return flight{kind: flightTLS, sni: sni, bytes: rec.seen}, nil
}

// recordingConn feeds crypto/tls the guest's bytes and keeps every one it
// read; it never writes to the guest (the alert crypto/tls sends when the
// capture stops is discarded) and stops reading at maxClientHello.
type recordingConn struct {
	net.Conn
	pending []byte
	seen    []byte
}

func (r *recordingConn) Read(p []byte) (int, error) {
	if len(r.pending) > 0 {
		n := copy(p, r.pending)
		r.seen = append(r.seen, r.pending[:n]...)
		r.pending = r.pending[n:]
		return n, nil
	}
	room := maxClientHello - len(r.seen)
	if room <= 0 {
		return 0, errSNIMissing
	}
	if len(p) > room {
		p = p[:room]
	}
	n, err := r.Conn.Read(p)
	r.seen = append(r.seen, p[:n]...)
	return n, err
}

func (r *recordingConn) Write(p []byte) (int, error) { return len(p), nil }
