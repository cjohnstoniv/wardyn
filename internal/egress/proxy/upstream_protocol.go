// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// This file is ONE subject: the upstream PROTOCOL a round trip actually got
// back — detecting when a peer answers the wrong one (HTTP/2 to an HTTP/1.1
// request, issue #359) and building/emitting/writing that refusal. Split out
// of sandbox_error.go, which is about sandbox-facing error BODIES in general;
// this file is about ONE specific mismatch and how this proxy notices it.
// Issue #360 adds this file's other two detection paths — an h2-fallback
// negotiation case and a post-TLS byte sniff for the cases isH2Preface cannot
// see (a SETTINGS frame whose payload happens to contain a space byte before
// any newline reads as a DIFFERENT net/http parse error — see isH2Preface's
// own doc comment and TestIsH2Preface's "known miss" case).

// isH2Preface reports whether err is Go's HTTP/1.x transport's own "malformed
// HTTP response" failure (net/http/transport.go's persistConn.readLoop wraps
// net/http/response.go's ReadResponse error — a bare badStringError, no
// exported type or sentinel — as `fmt.Errorf("net/http: HTTP/1.x transport
// connection broken: %w", err)`) WRAPPING an HTTP/2 frame header: the shape a
// TLS-terminating peer answers with when it speaks HTTP/2 unconditionally to a
// request this proxy's HTTP/1.1-only transport sent (mkTransport pins
// ForceAttemptHTTP2 false — issue #360 — so no ALPN ever offers h2). With no
// errors.As handle into it, the only way in is the exact message shape
// response.go's badStringError produces: "malformed HTTP response %q" with the
// line ReadResponse could not parse.
//
// Structural, not textual: a message that merely CONTAINS "malformed HTTP
// response" would also match an ordinary corrupted response, so this unquotes
// the %q-quoted payload and requires the first 9 bytes to be a syntactically
// valid HTTP/2 frame header (RFC 7540 §4.1) naming a SETTINGS frame (type
// 0x04) on stream 0 — the frame a compliant HTTP/2 server always sends first,
// unprompted, as its half of the connection preface.
//
// A KNOWN MISS, not a bug: net/http's own line-parser (response.go's
// ReadResponse) cuts the first line on the first SPACE byte, before it ever
// gets to "malformed HTTP response" — a SETTINGS payload that happens to
// contain a 0x20 byte before any 0x0a instead trips response.go's "malformed
// HTTP status code" arm, carrying only a fragment of the line, not the frame
// header. This function cannot see that shape from the error text alone (see
// TestIsH2Preface's own case for it); such an answer is misfiled as
// builtin:dial-failed today. So is a peer whose frames arrive before net/http
// has counted the request as outstanding: the transport drops them as an
// unsolicited response on an idle connection and the error carries no bytes.
// A post-TLS byte sniff that does not depend on net/http's parse error at all
// is issue #360's fix for both gaps.
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
	if uerr != nil || len(raw) < 9 {
		return false
	}
	const frameTypeSettings = 0x04
	b := []byte(raw)
	return b[3] == frameTypeSettings && b[5] == 0 && b[6] == 0 && b[7] == 0 && b[8] == 0
}

// alpnCapture attaches an httptrace.ClientTrace to ctx that records ONE
// outbound round trip's TLS-handshake outcome, so a protocol-mismatch Cause
// can name the ALPN this transport actually negotiated instead of guessing.
// The returned getter reports (negotiated-protocol, true) once
// TLSHandshakeDone has fired — proto is "" when the handshake completed with
// no protocol negotiated, which is what a nil/unset TLSClientConfig.NextProtos
// produces — or ("", false) when the round trip never reached a TLS handshake
// at all (the plain-HTTP forward lane's test-only branch, upstreamSchemeFor),
// so a caller can tell "no ALPN" from "no TLS ran here" rather than rendering
// a fabricated "none" for the second.
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
// that never happened.
func h2MismatchSentence(alpn string, hadTLS bool) string {
	if !hadTLS {
		return "peer answered HTTP/2 to an HTTP/1.1 request"
	}
	return "peer answered HTTP/2 to an HTTP/1.1 request (ALPN: " + alpnOrNone(alpn) + ")"
}

// upstreamProtocolMismatchCause is h2MismatchSentence run through the SAME
// mask + topology-redaction pass every other Cause takes (see
// dialFailureCause, sandbox_error.go): this sentence carries no upstream error
// text of its own, but the row is visible to the run's OWN CREATOR
// (auditScope), and running it through the shared pipeline anyway is one less
// place a future caller could forget the pass.
func (p *Proxy) upstreamProtocolMismatchCause(alpn string, hadTLS bool) string {
	masked := string(maskDecisionBytes([]byte(h2MismatchSentence(alpn, hadTLS))))
	return p.redactTopology(masked)
}

// denyUpstreamProtocolMismatch is denyDialFailed's sibling (sandbox_error.go)
// for isH2Preface's refusal: same Via computation, but Cause is the ALPN-aware
// h2-mismatch sentence rather than causeSentence's dial-stage-prefixed text,
// because the round trip COMPLETED and answered the wrong protocol — it never
// lost a dial. ruleSource is taken as an argument for the same reason
// denyDialFailed's is (see that function's doc comment): each emitting site
// keeps ruleSourceUpstreamProtocolMismatch written out on its own line, which
// is what docs/AUDIT-ACTIONS.md's rule_source table cites.
func (p *Proxy) denyUpstreamProtocolMismatch(ruleSource string, req egress.Request, host, alpn string, hadTLS bool, scan *egress.ScanSummary) egress.DecisionLog {
	dl := decisionLog(req, egress.Deny, ruleSource)
	dl.Cause = p.upstreamProtocolMismatchCause(alpn, hadTLS)
	dl.Via = p.viaHop(host)
	dl.Scan = scan
	return dl
}

// emitH2Mismatch is denyUpstreamProtocolMismatch's emit-if-present call,
// shared by both isH2Preface sites (forwardInspectedLLM, handlePlain) so
// neither repeats the nil-check: sink-nil-safe the same way every other emit
// site in the package is.
func (p *Proxy) emitH2Mismatch(ruleSource string, req egress.Request, host, alpn string, hadTLS bool, scan *egress.ScanSummary) {
	if p.sink != nil {
		p.sink.emit(p.denyUpstreamProtocolMismatch(ruleSource, req, host, alpn, hadTLS, scan))
	}
}

// writeUpstreamProtocolMismatch answers isH2Preface's refusal to the sandbox
// with cause — never the raw wrapped net/http error, which is the HTTP/2 frame
// bytes themselves (mostly control characters), not a readable diagnosis, the
// one respect this refusal does NOT go through httpError. The AWS lane gets
// writeAWSSDKError's modelled, non-retryable body (an AWS SDK hands plain text
// to a JSON parser and crashes on it — httpErrorAWSAware's own rationale);
// every other host gets a plain 400 carrying the same sentence — isAWSLane is
// evaluated here regardless of which lane called this, exactly as
// httpErrorAWSAware evaluates it for every other refusal, so the brokered/MITM
// LLM forward and the plain forward lane get identical AWS-lane treatment. A
// 400, not the 502 every other dial-shaped refusal answers with: a protocol
// mismatch never succeeds on retry, and 502 is exactly the class of error both
// AWS SDKs (and most others) retry.
//
// err carries the RAW, un-redacted net/http error (the HTTP/2 frame bytes) —
// never into the decision row or the sandbox body, only onto the operator-only
// slog line, masked, the same promise egress.DecisionLog.Cause's doc comment
// makes for every other dial-shaped refusal ("the full unredacted text stays
// on the sidecar's own slog.Warn line").
func (p *Proxy) writeUpstreamProtocolMismatch(w http.ResponseWriter, host, msg, cause string, err error) {
	slog.Warn("proxy error returned to the sandbox", "msg", msg, "status", http.StatusBadRequest,
		"err", cause, "raw_err", string(maskDecisionBytes([]byte(err.Error()))))
	if isAWSLane(host) {
		writeAWSSDKError(w, http.StatusBadRequest, "UpstreamProtocolMismatchException", cause)
		return
	}
	http.Error(w, msg+": "+cause, http.StatusBadRequest)
}
