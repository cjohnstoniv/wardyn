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
	"time"

	"golang.org/x/net/http2"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// This file is ONE subject: the upstream PROTOCOL a round trip actually got
// back. The egress transport offers h2,http/1.1 over ALPN (issue #360); a peer
// that speaks HTTP/2 WITHOUT negotiating it is caught two ways — a post-TLS
// byte sniff at dial time (sniffH2) and, for a peer that only answers once it
// has read a request, net/http's own parse error (isH2Preface) — then the
// request is resent over HTTP/2 when it can be replayed and the host is
// remembered for the rest of the run (roundTripUpstream). When that is not
// possible, or the HTTP/2 resend fails too, it is issue #359's refusal:
// builtin:upstream-protocol-mismatch and a 400.

// isH2Preface reports whether err is Go's HTTP/1.x transport's own "malformed
// HTTP response" failure (net/http/transport.go's persistConn.readLoop wraps
// net/http/response.go's ReadResponse error — a bare badStringError, no
// exported type or sentinel — as `fmt.Errorf("net/http: HTTP/1.x transport
// connection broken: %w", err)`) WRAPPING an HTTP/2 frame header: the shape a
// peer answers with when it speaks HTTP/2 only after reading an HTTP/1.1
// request it was sent on a connection that negotiated no ALPN (a peer that
// speaks first is caught earlier, by sniffH2). With no errors.As handle into
// it, the only way in is the exact message shape response.go's badStringError
// produces: "malformed HTTP response %q" with the line ReadResponse could not
// parse; the unquoted payload must then pass isH2FrameHeader, so an ordinary
// corrupted response does not match.
//
// What the error text cannot carry, sniffH2's byte read covers instead: a
// SETTINGS payload with a 0x20 byte before any 0x0a trips response.go's
// "malformed HTTP status code" arm with only a line fragment, and SETTINGS
// that arrive before net/http counts the request as outstanding are logged as
// "Unsolicited response received on idle HTTP channel" and dropped with no
// bytes at all. Both come from a peer that writes before it reads, which is
// exactly the case the dial-time sniff sees.
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

// isH2FrameHeader reports whether b starts with a syntactically valid HTTP/2
// frame header (RFC 9113 §4.1) naming a SETTINGS frame (type 0x04) on stream
// 0 — the frame a compliant HTTP/2 server always sends first, unprompted, as
// its half of the connection preface.
func isH2FrameHeader(b []byte) bool {
	const frameTypeSettings = 0x04
	return len(b) >= 9 && b[3] == frameTypeSettings && b[5] == 0 && b[6] == 0 && b[7] == 0 && b[8] == 0
}

// h2ProbeTimeout bounds sniffH2's wait for an HTTP/2 server's unprompted
// SETTINGS frame. It is paid only on a NEW connection whose TLS handshake
// negotiated no ALPN protocol at all — a peer that picks h2 or http/1.1 is
// never probed — and it has to cover one round trip through the corp proxy:
// the peer writes its SETTINGS only after reading the client's Finished.
const h2ProbeTimeout = 250 * time.Millisecond

// tlsHandshakeTimeout mirrors the egress http.Transport's TLSHandshakeTimeout,
// which a DialTLSContext hook bypasses and so has to apply itself.
const tlsHandshakeTimeout = 15 * time.Second

// errPeerSpeaksH2 is sniffH2's verdict: the peer sent an HTTP/2 SETTINGS frame
// on a connection that negotiated no ALPN. The dial fails before any request
// byte is written, and roundTripUpstream turns it into the HTTP/2 fallback.
var errPeerSpeaksH2 = errors.New("peer sent an HTTP/2 SETTINGS frame without negotiating h2")

type dialFunc = func(ctx context.Context, network, addr string) (net.Conn, error)

// h2Fallback is the egress lane's HTTP/2 path for a peer that speaks HTTP/2
// without negotiating it. hosts holds "host:port" keys and lives as long as
// the Proxy, which is per run; no TTL, since a peer's protocol does not
// change mid-run. It uses x/net's http2.Transport although x/net deprecates
// it for net/http: net/http speaks HTTP/2 over TLS only when ALPN selected
// h2, which is exactly what these peers never do.
type h2Fallback struct {
	//lint:ignore SA1019 net/http cannot speak HTTP/2 over TLS unless ALPN selected h2 (see h2Fallback)
	transport *http2.Transport
	hosts     sync.Map
}

// offerHTTP2 makes p.transport offer h2,http/1.1 and speak HTTP/2 when the
// peer negotiates it, sniffs a no-ALPN peer for HTTP/2 (sniffH2), and builds
// the fallback transport. Both TLS paths dial through the SAME egressDial —
// vetted target, upstream CONNECT, no_proxy bypass — so the fallback reaches
// nothing the HTTP/1.1 lane could not. p.transport gets a PRIVATE copy of
// base: its DialTLSContext does TLS itself, but enabling HTTP/2 appends to the
// transport's own TLSClientConfig.NextProtos on first use, and base is the
// very config controlTransport (HTTP/1.1 only) shares.
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
		if tc.ConnectionState().NegotiatedProtocol != "" {
			return tc, nil
		}
		return sniffH2(tc)
	}
	// x/net clones TLSClientConfig per dial, adds NextProtos ["h2"] and sets
	// ServerName; the custom dialer skips its "ALPN must say h2" check, which
	// is the point: this transport serves peers that never negotiate it.
	//lint:ignore SA1019 see h2Fallback
	p.h2.transport = &http2.Transport{
		TLSClientConfig: base,
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
// sends unprompted. An HTTP/1.1 server sends nothing before a request, so the
// read times out with no bytes and tc is returned untouched (crypto/tls
// treats a deadline as temporary; the conn stays usable). A SETTINGS frame
// header means HTTP/2. Anything else is handed back in front of tc so the
// HTTP/1.1 reader sees the peer's bytes exactly as it did before.
func sniffH2(tc *tls.Conn) (net.Conn, error) {
	var hdr [9]byte
	_ = tc.SetReadDeadline(time.Now().Add(h2ProbeTimeout))
	n, _ := io.ReadFull(tc, hdr[:])
	_ = tc.SetReadDeadline(time.Time{})
	switch {
	case isH2FrameHeader(hdr[:n]):
		_ = tc.Close()
		return nil, errPeerSpeaksH2
	case n == 0:
		return tc, nil
	}
	return &prefixedConn{Conn: tc, r: io.MultiReader(bytes.NewReader(hdr[:n]), tc)}, nil
}

type prefixedConn struct {
	net.Conn
	r io.Reader
}

func (c *prefixedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// h2MismatchError is roundTripUpstream's protocol-mismatch failure: the peer
// spoke HTTP/2 without negotiating it and the request could not be completed
// over HTTP/2 either. err is the HTTP/1.1 attempt's failure; h2Err is the
// HTTP/2 resend's, nil when none ran (notResent: the body could not be
// replayed; neither: the plain-HTTP lane, where there is no TLS to redo).
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
// transport. Otherwise the request goes out on p.transport (h2 when
// negotiated); if the peer turns out to speak HTTP/2 without negotiating it,
// the host is remembered and the request is resent ONCE over HTTP/2 when its
// body can be replayed. Only a request that did not complete comes back as an
// error, so a caller's allow decision still follows a successful round trip
// (E3), and it stays one decision per request.
func (p *Proxy) roundTripUpstream(req *http.Request) (*http.Response, error) {
	key := req.URL.Hostname() + ":" + cmp.Or(req.URL.Port(), "443")
	tlsLane := req.URL.Scheme == "https"
	if _, known := p.h2.hosts.Load(key); known && tlsLane {
		//lint:ignore SA1019 see h2Fallback
		return p.h2.transport.RoundTrip(req)
	}
	ctx, alpnState := alpnCapture(req.Context())
	resp, err := p.transport.RoundTrip(req.WithContext(ctx))
	sniffed := errors.Is(err, errPeerSpeaksH2)
	if err == nil || (!sniffed && !isH2Preface(err)) {
		return resp, err
	}
	mm := &h2MismatchError{err: err}
	mm.alpn, mm.hadTLS = alpnState()
	if sniffed {
		// The dial failed inside DialTLSContext, before net/http's trace hook.
		mm.alpn, mm.hadTLS = "", true
	}
	if !tlsLane {
		return nil, mm
	}
	p.h2.hosts.Store(key, struct{}{})
	retry, ok := replayable(req)
	if !ok {
		mm.notResent = true
		return nil, mm
	}
	//lint:ignore SA1019 see h2Fallback
	if resp, mm.h2Err = p.h2.transport.RoundTrip(retry); mm.h2Err != nil {
		return nil, mm
	}
	return resp, nil
}

// replayable returns a copy of req that can be sent again, or false when its
// body was a one-shot stream the first attempt may already have consumed.
func replayable(req *http.Request) (*http.Request, bool) {
	out := req.Clone(req.Context())
	switch {
	case req.Body == nil || req.Body == http.NoBody:
	case req.GetBody != nil:
		body, err := req.GetBody()
		if err != nil {
			return nil, false
		}
		out.Body = body
	default:
		return nil, false
	}
	return out, true
}

// alpnCapture attaches an httptrace.ClientTrace to ctx that records ONE
// outbound round trip's TLS-handshake outcome, so a protocol-mismatch Cause
// can name the ALPN this transport actually negotiated instead of guessing.
// The returned getter reports (negotiated-protocol, true) once
// TLSHandshakeDone has fired (net/http fires it after DialTLSContext returns
// a *tls.Conn) — proto is "" when the handshake completed with no protocol
// negotiated — or ("", false) when the round trip never reached a TLS
// handshake at all (the plain-HTTP forward lane's test-only branch,
// upstreamSchemeFor), so a caller can tell "no ALPN" from "no TLS ran here"
// rather than rendering a fabricated "none" for the second.
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

// alpnOrNone renders alpnCapture's negotiated protocol for the h2-mismatch
// cause sentence: "none" is a WORD an operator reads, where an empty string
// sitting in the middle of an otherwise readable sentence looks like a bug.
func alpnOrNone(proto string) string {
	if proto == "" {
		return "none"
	}
	return proto
}

// h2MismatchSentence is upstreamProtocolMismatchCause's sentence before the
// mask/redact pass. hadTLS is false only for the plain-HTTP forward lane's
// test-only branch (upstreamSchemeFor's http/80 arm, mitm_hosts.go): no TLS
// handshake ran there, so naming an ALPN outcome would claim a negotiation
// that never happened. The tail names the HTTP/2 attempt: why it did not run,
// or how it failed.
func h2MismatchSentence(e *h2MismatchError) string {
	if !e.hadTLS {
		return "peer answered HTTP/2 to an HTTP/1.1 request"
	}
	s := "peer answered HTTP/2 to an HTTP/1.1 request (ALPN: " + alpnOrNone(e.alpn) + ")"
	switch {
	case e.h2Err != nil:
		s += "; the HTTP/2 resend also failed: " + e.h2Err.Error()
	case e.notResent:
		s += "; not resent over HTTP/2 because the request body cannot be replayed; later requests to this host use HTTP/2"
	}
	return s
}

// upstreamProtocolMismatchCause is h2MismatchSentence run through the SAME
// mask + topology-redaction pass every other Cause takes (see
// dialFailureCause, sandbox_error.go): the HTTP/2 resend's error text is an
// upstream error like any dial failure's, and the row is visible to the run's
// OWN CREATOR (auditScope).
func (p *Proxy) upstreamProtocolMismatchCause(e *h2MismatchError) string {
	masked := string(maskDecisionBytes([]byte(h2MismatchSentence(e))))
	return p.redactTopology(masked)
}

// refuseH2Mismatch answers roundTripUpstream's *h2MismatchError, reporting
// whether err was one; any other error is left to the caller's dial-failed
// arm. seen carries the request and scan summary the deny row reports; nil
// means the caller emits no decision for this request. It is
// denyDialFailed's sibling (sandbox_error.go): same Via computation, but Cause
// is the h2-mismatch sentence rather than a dial-stage-prefixed text, because
// the peer ANSWERED. ruleSource is an argument so each call site keeps
// ruleSourceUpstreamProtocolMismatch written out on its own line, which is
// what docs/AUDIT-ACTIONS.md's rule_source table cites.
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

// writeUpstreamProtocolMismatch answers the refusal to the sandbox with cause
// — never the raw wrapped net/http error, which is the HTTP/2 frame bytes
// themselves (mostly control characters), not a readable diagnosis, the one
// respect this refusal does NOT go through httpError. The AWS lane gets
// writeAWSSDKError's modelled, non-retryable body (an AWS SDK hands plain text
// to a JSON parser and crashes on it — httpErrorAWSAware's own rationale);
// every other host gets a plain 400 carrying the same sentence — isAWSLane is
// evaluated here regardless of which lane called this, exactly as
// httpErrorAWSAware evaluates it for every other refusal. A 400, not the 502
// every other dial-shaped refusal answers with: 502 is exactly the class of
// error both AWS SDKs (and most others) retry blindly, and a retry here only
// helps once roundTripUpstream's memo routes the host to HTTP/2.
//
// err carries the RAW, un-redacted error (the HTTP/2 frame bytes) — never
// into the decision row or the sandbox body, only onto the operator-only slog
// line, masked, the same promise egress.DecisionLog.Cause's doc comment makes
// for every other dial-shaped refusal ("the full unredacted text stays on the
// sidecar's own slog.Warn line").
func (p *Proxy) writeUpstreamProtocolMismatch(w http.ResponseWriter, host, msg, cause string, err error) {
	slog.Warn("proxy error returned to the sandbox", "msg", msg, "status", http.StatusBadRequest,
		"err", cause, "raw_err", string(maskDecisionBytes([]byte(err.Error()))))
	if isAWSLane(host) {
		writeAWSSDKError(w, http.StatusBadRequest, "UpstreamProtocolMismatchException", cause)
		return
	}
	http.Error(w, msg+": "+cause, http.StatusBadRequest)
}
