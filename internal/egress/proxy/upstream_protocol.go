// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// This file handles a peer that speaks HTTP/2 WITHOUT negotiating it: caught
// either by a post-TLS byte sniff at dial time (sniffH2) or by net/http's own
// parse error for a peer that answers only after reading a request
// (isH2Preface). roundTripUpstream then resends over HTTP/2 when replayable
// and remembers the host; otherwise it refuses with
// builtin:upstream-protocol-mismatch and a 400.

// isH2Preface reports whether err is net/http's "malformed HTTP response"
// failure (a bare badStringError with no exported type or sentinel) WRAPPING
// an HTTP/2 frame header — the shape a peer answers with when it speaks
// HTTP/2 only after reading a request on a connection that negotiated no
// ALPN (a peer that speaks first is caught earlier, by sniffH2). With no
// errors.As handle into it, matching relies on the exact message shape
// "malformed HTTP response %q", so an ordinary corrupted response won't match.
//
// sniffH2's own byte read exists for cases the error text can't carry:
// SETTINGS bytes on a different parse arm, or none logged at all — both from
// a peer that writes before it reads, which is what the dial-time sniff sees.
func isH2Preface(err error) bool {
	if err == nil {
		return false
	}
	const marker = `malformed HTTP response "`
	msg := err.Error()
	i := strings.Index(msg, marker)
	if i < 0 {
		return false
	}
	raw, uerr := strconv.Unquote(msg[i+len(marker)-1:])
	return uerr == nil && isH2FrameHeader([]byte(raw))
}

// isH2FrameHeader reports whether b starts with a valid HTTP/2 SETTINGS
// frame header (RFC 9113 §4.1) on stream 0 — what a compliant HTTP/2 server
// always sends first, unprompted, as its half of the connection preface.
func isH2FrameHeader(b []byte) bool {
	const frameTypeSettings = 0x04
	return len(b) >= 9 && b[3] == frameTypeSettings && b[5] == 0 && b[6] == 0 && b[7] == 0 && b[8] == 0
}

// h2ProbeTimeout bounds sniffH2's wait for an unprompted SETTINGS frame.
// Paid once per host (memoized in h2Fallback.hosts) and only when ALPN
// negotiated nothing at all.
const h2ProbeTimeout = 250 * time.Millisecond

// tlsHandshakeTimeout mirrors the egress http.Transport's TLSHandshakeTimeout,
// which a DialTLSContext hook bypasses and so has to apply itself.
const tlsHandshakeTimeout = 15 * time.Second

// errPeerSpeaksH2 is sniffH2's verdict when the peer sends an HTTP/2 SETTINGS
// frame on a no-ALPN connection; the dial fails before any request byte is
// written and roundTripUpstream turns it into the HTTP/2 fallback.
var errPeerSpeaksH2 = errors.New("peer sent an HTTP/2 SETTINGS frame without negotiating h2")

// errNoUnpromptedBytes is sniffH2's verdict for an HTTP/1.1 peer: nothing
// arrived in the probe window. The connection is still usable.
var errNoUnpromptedBytes = errors.New("peer sent nothing before the request")

type dialFunc = func(ctx context.Context, network, addr string) (net.Conn, error)

// h2Fallback is the HTTP/2 path for a peer that speaks HTTP/2 without
// negotiating it. hosts maps a memoKey to what this run learned: true, go
// straight to the fallback transport; false, skip the sniff on later
// connections. No TTL — a peer's protocol doesn't change mid-run. Uses x/net's
// deprecated http2.Transport because net/http itself only speaks HTTP/2 over
// TLS when ALPN selected h2, which these peers never do.
type h2Fallback struct {
	//lint:ignore SA1019 net/http cannot speak HTTP/2 over TLS unless ALPN selected h2 (see h2Fallback)
	transport *http2.Transport
	hosts     sync.Map
}

// offerHTTP2 makes p.transport offer h2,http/1.1, sniffs a no-ALPN peer for
// HTTP/2, and builds the fallback transport. Both TLS paths dial through the
// SAME egressDial so the fallback reaches nothing the HTTP/1.1 lane could
// not. p.transport gets a PRIVATE copy of base since enabling HTTP/2 appends
// to TLSClientConfig.NextProtos on first use.
func (p *Proxy) offerHTTP2(egressDial dialFunc, base *tls.Config) {
	p.transport.ForceAttemptHTTP2 = true
	p.transport.TLSClientConfig = base.Clone()
	p.transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		cfg := base.Clone()
		if cfg == nil {
			cfg = &tls.Config{}
		}
		if cfg.ServerName == "" {
			cfg.ServerName, _, _ = net.SplitHostPort(addr)
		}
		cfg.NextProtos = []string{"h2", "http/1.1"}
		tc, err := handshakeOver(ctx, egressDial, network, addr, cfg)
		if err != nil {
			return nil, err
		}
		key := memoKeyFromAddr(addr)
		if v, seen := p.h2.hosts.Load(key); tc.ConnectionState().NegotiatedProtocol != "" || (seen && !v.(bool)) {
			return tc, nil
		}
		conn, err := sniffH2(tc)
		if errors.Is(err, errNoUnpromptedBytes) {
			// HTTP/1.1 peer: skip the probe on later connections.
			p.h2.hosts.Store(key, false)
			return tc, nil
		}
		return conn, err
	}
	// Skips x/net's "ALPN must say h2" check on purpose: this transport
	// serves peers that never negotiate it.
	//lint:ignore SA1019 see h2Fallback
	p.h2.transport = &http2.Transport{
		TLSClientConfig: base.Clone(),
		IdleConnTimeout: 60 * time.Second,
		DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
			tc, err := handshakeOver(ctx, egressDial, network, addr, cfg)
			if err != nil {
				return nil, err // never a typed-nil *tls.Conn inside net.Conn
			}
			return tc, nil
		},
	}
}

func handshakeOver(ctx context.Context, dial dialFunc, network, addr string, cfg *tls.Config) (*tls.Conn, error) {
	raw, err := dial(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	hctx, cancel := context.WithTimeout(ctx, tlsHandshakeTimeout)
	defer cancel()
	tc := tls.Client(raw, cfg)
	if err := tc.HandshakeContext(hctx); err != nil {
		_ = raw.Close()
		return nil, err
	}
	return tc, nil
}

// sniffH2 reads, for at most h2ProbeTimeout, the first 9 bytes a no-ALPN peer
// sends unprompted. No bytes (an HTTP/1.1 server) yields errNoUnpromptedBytes
// with tc still usable (crypto/tls treats a deadline as temporary). A
// SETTINGS frame header means HTTP/2; anything else is handed back unchanged.
func sniffH2(tc *tls.Conn) (net.Conn, error) {
	var hdr [9]byte
	_ = tc.SetReadDeadline(time.Now().Add(h2ProbeTimeout))
	n, _ := io.ReadFull(tc, hdr[:])
	_ = tc.SetReadDeadline(time.Time{})
	switch {
	case isH2FrameHeader(hdr[:n]):
		// RAW conn, not tc.Close: this peer is dropped mid-protocol-confusion.
		_ = tc.NetConn().Close()
		return nil, errPeerSpeaksH2
	case n == 0:
		return tc, errNoUnpromptedBytes
	}
	return &prefixedConn{Conn: tc, r: io.MultiReader(bytes.NewReader(hdr[:n]), tc)}, nil
}

// memoKey names a peer the way every other host lookup in this package does:
// lower-cased, with a trailing root-label dot dropped.
func memoKey(host, port string) string {
	return strings.TrimSuffix(strings.ToLower(host), ".") + ":" + port
}

func memoKeyFromAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return strings.ToLower(addr)
	}
	return memoKey(host, port)
}

type prefixedConn struct {
	net.Conn
	r io.Reader
}

func (c *prefixedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// h2MismatchError is roundTripUpstream's protocol-mismatch failure: err is
// the HTTP/1.1 attempt's failure; h2Err is the HTTP/2 resend's, nil when
// none ran.
type h2MismatchError struct {
	alpn      string
	hadTLS    bool
	notResent bool
	err       error
	h2Err     error
}

func (e *h2MismatchError) Error() string {
	msg := "upstream protocol mismatch: " + e.err.Error()
	if e.h2Err != nil {
		msg += "; HTTP/2 resend: " + e.h2Err.Error()
	}
	return msg
}

func (e *h2MismatchError) Unwrap() error { return e.err }

// roundTripUpstream is the MITM/LLM and plain lanes' round trip. A host
// already known to speak HTTP/2 unasked goes straight to the fallback
// transport; otherwise the request goes out on p.transport, and if the peer
// speaks HTTP/2 without negotiating it, the host is remembered and the
// request resent ONCE over HTTP/2 when its body can be replayed. Only an
// incomplete request returns as an error, so an allow decision always
// follows a successful round trip.
func (p *Proxy) roundTripUpstream(req *http.Request) (*http.Response, error) {
	key := memoKey(req.URL.Hostname(), cmp.Or(req.URL.Port(), "443"))
	tlsLane := req.URL.Scheme == "https"
	if v, seen := p.h2.hosts.Load(key); seen && v.(bool) && tlsLane {
		//lint:ignore SA1019 see h2Fallback
		return p.h2.transport.RoundTrip(req)
	}
	// Shielded: net/http closes a request body when a dial fails, and a
	// sniffed peer fails the dial before any byte is written, so the
	// untouched body can still go out over HTTP/2.
	var shield *shieldedBody
	if req.Body != nil && req.Body != http.NoBody {
		shield = &shieldedBody{rc: req.Body}
		req.Body = shield
	}
	closeBody := func() {
		if shield != nil {
			_ = shield.rc.Close()
		}
	}
	ctx, alpnState := alpnCapture(req.Context())
	resp, err := p.transport.RoundTrip(req.WithContext(ctx))
	sniffed := errors.Is(err, errPeerSpeaksH2)
	if err == nil || (!sniffed && !isH2Preface(err)) {
		return resp, err
	}
	// hadTLS comes from the request, not the trace: net/http records the
	// handshake only for a *tls.Conn, and sniffH2 may have wrapped one.
	mm := &h2MismatchError{err: err, hadTLS: tlsLane}
	if !sniffed {
		mm.alpn, _ = alpnState()
	}
	if !tlsLane {
		closeBody()
		return nil, mm
	}
	p.h2.hosts.Store(key, true)
	retry, ok := resendable(req, shield, sniffed)
	if !ok {
		mm.notResent = true
		closeBody()
		return nil, mm
	}
	//lint:ignore SA1019 see h2Fallback
	if resp, mm.h2Err = p.h2.transport.RoundTrip(retry); mm.h2Err != nil {
		closeBody()
		return nil, mm
	}
	return resp, nil
}

// shieldedBody hides a request body's Close from an attempt that may have to
// be made again, and records whether the transport read any of it.
type shieldedBody struct {
	rc   io.ReadCloser
	read atomic.Bool
}

func (b *shieldedBody) Read(p []byte) (int, error) {
	b.read.Store(true)
	return b.rc.Read(p)
}

func (b *shieldedBody) Close() error { return nil }

// resendable returns the request to send over HTTP/2, or false when this one
// can't be sent again. A body net/http can rebuild is rebuilt; failing that,
// a sniffed peer's unread body goes out as-is, but a peer detected from its
// answer (isH2Preface) was already written to, so only a rebuildable body
// qualifies.
func resendable(req *http.Request, shield *shieldedBody, sniffed bool) (*http.Request, bool) {
	out := req.Clone(req.Context())
	switch {
	case shield == nil: // no body, or http.NoBody
	case req.GetBody != nil:
		// Tried first: the failed attempt's write goroutine may still be
		// alive, and rebuilding hands the resend a reader it can't touch.
		body, err := req.GetBody()
		if err != nil {
			return nil, false
		}
		out.Body = body
	case sniffed && !shield.read.Load():
		// Only reachable with GetBody nil, so nothing else is reading this body.
		out.Body = shield
	default:
		return nil, false
	}
	return out, true
}

// alpnCapture attaches an httptrace.ClientTrace to ctx that records ONE
// round trip's TLS-handshake outcome, so a protocol-mismatch Cause can name
// the ALPN actually negotiated instead of guessing. The returned getter
// distinguishes "no ALPN" (handshaked, empty protocol) from "no TLS ran here"
// (never handshaked).
func alpnCapture(ctx context.Context) (context.Context, func() (proto string, handshaked bool)) {
	var negotiated string
	var done bool
	trace := &httptrace.ClientTrace{
		TLSHandshakeDone: func(state tls.ConnectionState, _ error) {
			negotiated, done = state.NegotiatedProtocol, true
		},
	}
	return httptrace.WithClientTrace(ctx, trace), func() (string, bool) { return negotiated, done }
}

// alpnOrNone renders the negotiated protocol for the h2-mismatch cause
// sentence: an empty string mid-sentence reads as a bug, "none" doesn't.
func alpnOrNone(proto string) string {
	if proto == "" {
		return "none"
	}
	return proto
}

// h2MismatchSentence is upstreamProtocolMismatchCause's sentence before the
// mask/redact pass. hadTLS is false only for the plain-HTTP test-only branch,
// where no handshake ran, so naming an ALPN outcome would be false.
func h2MismatchSentence(e *h2MismatchError) string {
	if !e.hadTLS {
		return "peer answered HTTP/2 to an HTTP/1.1 request"
	}
	s := "peer answered HTTP/2 to an HTTP/1.1 request (ALPN: " + alpnOrNone(e.alpn) + ")"
	switch {
	case e.h2Err != nil:
		s += "; the HTTP/2 resend also failed: " + e.h2Err.Error()
	case e.notResent:
		s += "; not resent over HTTP/2 because the request had already been written and its body cannot be replayed; later requests to this host use HTTP/2"
	}
	return s
}

// upstreamProtocolMismatchCause runs h2MismatchSentence through the SAME
// mask + topology-redaction pass every other Cause takes: the row is visible
// to the run's own creator.
func (p *Proxy) upstreamProtocolMismatchCause(e *h2MismatchError) string {
	masked := string(maskDecisionBytes([]byte(h2MismatchSentence(e))))
	return p.redactTopology(masked)
}

// refuseH2Mismatch answers roundTripUpstream's *h2MismatchError, reporting
// whether err was one; any other error is left to the caller's dial-failed
// arm. seen carries the request/scan the deny row reports; nil emits no
// decision. denyDialFailed's sibling: same Via computation, but Cause is the
// h2-mismatch sentence, since the peer ANSWERED.
func (p *Proxy) refuseH2Mismatch(w http.ResponseWriter, err error, ruleSource string, seen *egress.DecisionLog, host, msg string) bool {
	var mm *h2MismatchError
	if !errors.As(err, &mm) {
		return false
	}
	cause := p.upstreamProtocolMismatchCause(mm)
	if seen != nil && p.sink != nil {
		dl := decisionLog(seen.Request, egress.Deny, ruleSource)
		dl.Cause = cause
		dl.Via = p.viaHop(host)
		dl.Scan = seen.Scan
		p.sink.emit(dl)
	}
	p.writeUpstreamProtocolMismatch(w, host, msg, cause, err)
	return true
}

// failUpstream answers a failed roundTripUpstream with exactly one deny row:
// the HTTP/2 mismatch refusal (a 400), or builtin:dial-failed and a 502. A
// lane's allow row is only ever emitted after a successful round trip, so a
// failed dial never over-reports an allow. seen may be nil.
func (p *Proxy) failUpstream(w http.ResponseWriter, err error, seen *egress.DecisionLog, host, msg string) {
	if p.refuseH2Mismatch(w, err, ruleSourceUpstreamProtocolMismatch, seen, host, msg) {
		return
	}
	if seen != nil && p.sink != nil {
		p.sink.emit(p.denyDialFailed("builtin:dial-failed", seen.Request, host, err, seen.Scan))
	}
	p.httpError(w, msg, err, http.StatusBadGateway)
}

// writeUpstreamProtocolMismatch answers the refusal to the sandbox with
// cause, never the raw wrapped net/http error — the one refusal that does
// NOT go through httpError. The AWS lane gets writeAWSSDKError's modelled
// body; every other host gets a plain 400, not the usual 502, since 502 is
// exactly the class an SDK retries blindly and a retry only helps once
// roundTripUpstream's memo routes the host to HTTP/2.
//
// SECURITY: err carries the RAW, un-redacted error onto the operator-only
// slog line, masked — never into the decision row or the sandbox body.
func (p *Proxy) writeUpstreamProtocolMismatch(w http.ResponseWriter, host, msg, cause string, err error) {
	slog.Warn("proxy error returned to the sandbox", "msg", msg, "status", http.StatusBadRequest,
		"err", cause, "raw_err", string(maskDecisionBytes([]byte(err.Error()))))
	if isAWSLane(host) {
		writeAWSSDKError(w, http.StatusBadRequest, "UpstreamProtocolMismatchException", cause)
		return
	}
	http.Error(w, msg+": "+cause, http.StatusBadRequest)
}
