// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"sync/atomic"
	"syscall"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// This file is the opaque CONNECT tunnel after its 200: the relay itself, the
// guard on what the dialled hop may put into it, and the one audit row a
// tunnel that died leaves behind. Split out of proxy.go for the file-size gate.

const (
	// ruleSourceTunnelFailed marks a CONNECT tunnel that was acknowledged and
	// then died before it carried an answer. The dial succeeded, so the allow
	// row stands and this deny follows it; "builtin:dial-failed" would say the
	// tunnel never opened. The wire value is egress.RuleSourceTunnelFailed, the
	// one every consumer classifies against (egress.IsNetworkFault).
	ruleSourceTunnelFailed = egress.RuleSourceTunnelFailed
)

// resolveBypassedCause is the Cause on a builtin:resolve-failed row when an
// upstream proxy is configured: the only names resolved here to be dialled are
// the ones the upstream bypass list covers.
const resolveBypassedCause = "this name is on the upstream proxy's bypass list, so the proxy resolves and dials it itself, " +
	"and it did not resolve at the proxy"

// tunnel pipes bytes in both directions until EITHER side finishes, then closes
// both connections — the standard CONNECT-proxy shape.
//
// Why the first finisher closes and not both: waiting (wg.Wait()) for
// BOTH io.Copy calls before closing anything would let either direction pin
// the tunnel forever. When the sandbox side goes away the client->upstream
// copy returns and half-closes the upstream write side, but the
// upstream->client copy stays blocked in Read until the upstream sends or
// closes. An upstream that never does — an attacker-controlled allowed host, a
// hung TLS endpoint, a dropped FIN — would pin that goroutine, its 32 KiB copy
// buffer, the hijacked client socket and the upstream socket FOREVER: the
// listener's IdleTimeout (server.go) does not apply to a hijacked connection,
// and nothing else deadlines or caps an opaque tunnel (the inner MITM server
// has ReadHeaderTimeout/ReadTimeout/IdleTimeout, mitm.go — this lane has
// none of its own). A prompt-injected process in the sandbox could open and
// abandon tunnels in a loop, measured at 2 goroutines + both sockets retained
// per tunnel, inside a sidecar sized at 256 MiB.
//
// Closing on the first finisher bounds that to the lifetime of whichever
// direction ends first, and costs nothing a CONNECT tunnel relies on: the
// half-close below still fires first, so a peer that is merely done SENDING
// sees EOF exactly as before, and a TLS session (every real user of this lane)
// is over for both directions once either endpoint is gone. The second copy
// goroutine returns as soon as Close unblocks its Read; done is buffered so it
// can never block on a receiver that has already left.
//
// It returns the SOURCE of the direction that finished first, and the error
// that ended it (nil for a clean end of stream).
func tunnel(a, b net.Conn) (first net.Conn, err error) {
	type end struct {
		src net.Conn
		err error
	}
	done := make(chan end, 2)
	cp := func(dst, src net.Conn) {
		_, err := io.Copy(dst, src)
		// Half-close the write side if supported so the peer sees EOF.
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- end{src, err}
	}
	go cp(a, b)
	go cp(b, a)
	e := <-done
	_ = a.Close()
	_ = b.Close()
	return e.src, e.err
}

// What the client has put into the tunnel so far (tunnelWatch.client).
const (
	clientSilent int32 = iota // nothing yet
	clientPlain               // its first byte was not a TLS handshake record
	clientTLS                 // its first byte was 0x16: the guard is armed
)

// tunnelWatch.alert when the upstream's first record was not an alert, and
// when it was one whose description had not arrived with its header.
const (
	alertNone   int32 = -1
	alertUnread int32 = -2
)

// tlsRecordHeaderLen is a TLS record header: content type, version, length.
// tlsAlertRecordLen is a whole plaintext alert: the header, the level, the
// description.
const (
	tlsRecordHeaderLen = 5
	tlsAlertRecordLen  = tlsRecordHeaderLen + 2
)

// errTunnelRefused ends the upstream-to-client copy when the guard refuses
// the upstream's first bytes. Nothing was relayed.
var errTunnelRefused = errors.New("proxy: the upstream's first bytes were refused")

// tunnelRefusal is what the guard refused. Only status (three digits) comes
// from the upstream's bytes.
type tunnelRefusal struct {
	armed  bool   // refused as the answer to a TLS hello, not as an unprompted HTTP response
	status string // "" when the bytes were not an HTTP/1.x status line
}

// tunnelWatch wraps the dialled side of a CONNECT tunnel. It notes what went
// each way for the row tunnelEnded may write, and guards the upstream's FIRST
// bytes: a hop that acknowledged the CONNECT and then answers a TLS hello in
// plaintext (an error page, a captive portal) would otherwise have that
// relayed to a client that can only report it as a TLS decode error.
//
// The guard is armed by the client's first byte alone (0x16, the same test
// clientSpeaksTLS uses). A tunnel the client opens with anything else (ssh
// over 443, plain HTTP, a websocket upgrade) is relayed untouched, as is
// everything after the first bytes of an armed one. One case needs no hello:
// an upstream that writes "HTTP/" before any client byte reached it is
// answering the CONNECT a second time, and is refused.
//
// Read runs on one goroutine (tunnel's copy) and Write on another, and
// tunnelEnded reads the result while the slower of the two may still be
// running, so everything they share is atomic.
type tunnelWatch struct {
	net.Conn
	br     *bufio.Reader
	vetted bool // Read's goroutine only

	client  atomic.Int32 // clientSilent, clientPlain or clientTLS
	relayed atomic.Int64 // upstream bytes handed on to the client
	alert   atomic.Int32 // alertNone, alertUnread, or the first record's alert description
	refused atomic.Pointer[tunnelRefusal]
}

func newTunnelWatch(upstream net.Conn) *tunnelWatch {
	w := &tunnelWatch{Conn: upstream, br: bufio.NewReader(upstream)}
	w.alert.Store(alertNone)
	return w
}

// Write records what the client opened with BEFORE the bytes leave, so by the
// time the upstream can have answered them Read already sees the guard armed.
// Set after the write, the first client flight would race its own answer, and
// tunnelEnded could run on that answer before the write was counted.
func (w *tunnelWatch) Write(b []byte) (int, error) {
	if len(b) > 0 && w.client.Load() == clientSilent {
		opened := clientPlain
		if b[0] == 0x16 {
			opened = clientTLS
		}
		w.client.Store(opened)
	}
	return w.Conn.Write(b)
}

// CloseWrite keeps tunnel's half-close reaching the wrapped connection.
func (w *tunnelWatch) CloseWrite() error {
	if cw, ok := w.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (w *tunnelWatch) Read(b []byte) (int, error) {
	if !w.vetted {
		if err := w.vet(); err != nil {
			return 0, err
		}
		w.vetted = true
	}
	n, err := w.br.Read(b)
	w.relayed.Add(int64(n))
	return n, err
}

// vet holds the upstream's first bytes until they can be judged. It waits as
// long as the upstream takes to speak: there is no first-read timeout, and a
// tunnel whose client leaves first is closed by tunnel, which ends the wait.
func (w *tunnelWatch) vet() error {
	if _, err := w.br.Peek(1); err != nil {
		return err
	}
	switch w.client.Load() {
	case clientTLS:
		// The first five bytes, or the end of the stream. Fewer than five is
		// not a record: return the error and relay none of them.
		head, err := w.br.Peek(tlsRecordHeaderLen)
		if err != nil {
			return err
		}
		if !isTLSRecordHeader(head) {
			return w.refuse(true)
		}
		// A server's alert is relayed: it is how TLS says no, and the client
		// reports it better than an empty read.
		if head[0] == 0x15 {
			w.alert.Store(alertUnread)
			if flight, _ := w.br.Peek(w.br.Buffered()); len(flight) >= tlsAlertRecordLen {
				w.alert.Store(int32(flight[tlsAlertRecordLen-1]))
			}
		}
	case clientSilent:
		// Whatever arrived in this first flight, without waiting for more: a
		// peer that speaks first (an ssh banner) must not be held back.
		if flight, _ := w.br.Peek(w.br.Buffered()); bytes.HasPrefix(flight, []byte("HTTP/")) {
			return w.refuse(false)
		}
	}
	return nil
}

// isTLSRecordHeader reports whether head opens a TLS record: a content type
// of change_cipher_spec, alert, handshake or application_data, then the major
// version byte every TLS version writes.
func isTLSRecordHeader(head []byte) bool {
	return len(head) >= 2 && head[0] >= 0x14 && head[0] <= 0x17 && head[1] == 0x03
}

// statusLineRe is the STRICT parse of an HTTP/1.x status line: the only
// upstream text, beside an alert's description byte, that may reach a Cause.
// Only the status code is captured. The reason phrase must still be printable
// ASCII and capped for the line to parse, and is never kept.
var statusLineRe = regexp.MustCompile(`^HTTP/1\.[0-9] ([0-9]{3})(?: [ -~]{0,64})?\r?\n`)

// refuse records what the upstream sent instead of TLS, from the flight
// already buffered, and returns the error that ends the relay.
func (w *tunnelWatch) refuse(armed bool) error {
	r := &tunnelRefusal{armed: armed}
	flight, _ := w.br.Peek(w.br.Buffered())
	if m := statusLineRe.FindSubmatch(flight); m != nil {
		r.status = string(m[1])
	}
	w.refused.Store(r)
	return errTunnelRefused
}

// failure is the sentence for the row a dead tunnel leaves, or "" for a
// tunnel that needs none: one the client never wrote into, and any tunnel
// that carried an answer. upstreamFirst says which direction ended first and
// err what ended it. No client byte reaches it, and from the upstream only refuse's strict status
// parse and the alert's description byte do. No reason phrase is ever kept,
// on any hop: a sandbox can reach a host of its choosing through the
// operator's proxy without sending a byte, so the phrase is never known to be
// the operator's wording.
func (w *tunnelWatch) failure(upstreamFirst bool, err error) string {
	if r := w.refused.Load(); r != nil {
		got := "bytes that are not a TLS record"
		switch {
		case r.status != "":
			got = "HTTP " + r.status
		case !r.armed:
			got = "an HTTP response"
		}
		if r.armed {
			return "tunnel first bytes: the upstream answered a TLS hello with " + got + ", not TLS; nothing was relayed"
		}
		return "tunnel first bytes: the upstream sent " + got + " before the client sent anything; nothing was relayed"
	}
	relayed := w.relayed.Load()
	if alert := w.alert.Load(); alert != alertNone && relayed <= tlsAlertRecordLen {
		if alert == alertUnread {
			return "tunnel tls handshake: the upstream answered the TLS hello with an alert and nothing else"
		}
		return fmt.Sprintf("tunnel tls handshake: the upstream answered the TLS hello with alert %d and nothing else", alert)
	}
	if w.client.Load() == clientSilent || relayed > 0 {
		return ""
	}
	switch {
	case upstreamFirst && errors.Is(err, syscall.ECONNRESET):
		return "tunnel first bytes: the upstream reset the connection without answering"
	case upstreamFirst:
		return "tunnel first bytes: the upstream closed without answering"
	}
	return "tunnel first bytes: the client closed before any reply"
}

// tunnelEnded writes the one egress.deny a tunnel that died after its 200
// leaves: the guard refused the upstream's first bytes, the upstream's whole
// answer was a TLS alert, or the client sent bytes and none came back. The
// allow emitted when the dial succeeded stays: the tunnel did open. seen is
// that allow (nil emits nothing); first and err are what tunnel returned.
func (p *Proxy) tunnelEnded(seen *egress.DecisionLog, host string, w *tunnelWatch, first net.Conn, err error) {
	if seen == nil || p.sink == nil {
		return
	}
	cause := w.failure(first == net.Conn(w), err)
	if cause == "" {
		return
	}
	dl := decisionLog(seen.Request, egress.Deny, ruleSourceTunnelFailed)
	dl.Cause = p.redactTopology(string(maskDecisionBytes([]byte(cause))))
	dl.Via = p.viaHop(host)
	p.sink.emit(dl)
}

// emitDialledAllow emits an allow that followed a forward dial, stamped with
// the hop class that carried it, so an allow row says whether the upstream
// proxy was in the path. An allow that dialled nothing (the MITM CONNECT
// allow) does not come through here.
func (p *Proxy) emitDialledAllow(dl egress.DecisionLog, host string) {
	if p.sink == nil {
		return
	}
	dl.Via = p.viaHop(host)
	p.sink.emit(dl)
}
