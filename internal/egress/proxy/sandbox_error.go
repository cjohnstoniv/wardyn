// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// This file is ONE subject: what a proxy error may say to the SANDBOX.
//
// Every sandbox-facing error in the package goes through Proxy.httpError, so
// the two things that must be true of such a body — the process-global
// credential mask, and the topology redaction F152 added — are decided here and
// nowhere else. Split out of proxy.go for the 1000-line file-size gate; the
// seam is the trust boundary, not an arbitrary cut.

// httpError writes "<msg>: <err>" to the SANDBOX with the process-global secret
// mask applied to the error text — the same mask the decision-log path uses. A
// proxy error routinely wraps upstream/control-plane text (see resolveInjection's
// status echo), and the sandbox is the one party that must never observe an
// injected credential, so error strings are masked here rather than at each site
// — every sandbox-facing error goes through this helper, so no handler in the
// package holds a raw err.Error().
// F152 — the mask is not enough on its own: maskDecisionBytes only replaces
// values registered in procRegistry (i.e. CREDENTIALS), and a Go transport error
// always embeds the ENDPOINT that failed. Three sandbox-reachable callers were
// therefore handing the untrusted (possibly prompt-injected) process inside the
// box an internal address it has no other way to learn — relayControlPlane the
// control-plane base URL, its resolved ip:port and the internal API path;
// handleConnect's dial-failure branch the operator's corporate upstream proxy
// host:port; handlePlain the vetted destination IP, which under the site-config
// internal_hosts lift is a private address — and serveMITMRequest's credential
// refresh the same control-plane URL. buildBaseSandboxEnv exports the sandbox
// only WARDYN_PROXY_URL/HTTP_PROXY, never these, so it was new information
// across the product's primary trust boundary, not an echo.
//
// So the sandbox body keeps the message and the DIAGNOSIS ("connect: connection
// refused", a real upstream status) and loses the topology: anything derived
// from p.controlPlaneURL or p.upstream.addr, and every IP literal (which is what
// a vetted or pinned address renders as). The two halves are deliberately not
// the same shape, and the difference is what the sandbox keeps:
//   - the endpoint patterns are ENDPOINT-SCOPED (topologyPatterns compiles only
//     the operator's configured control-plane URL and corp-proxy address, plus
//     whatever path follows them), so a HOSTNAME the sandbox itself named — the
//     destination a developer asked for — survives, and with it the half of the
//     message they need;
//   - the IP arm is a BLANKET redaction of every address literal, a
//     sandbox-named one included. That is not an oversight: once an address is
//     inside a transport error string the proxy cannot tell a vetted
//     destination from a pinned internal address, and the literals a sandbox
//     can name at all are the site-config internal_hosts lift's private
//     addresses — exactly the class this hides. Fail closed on the ambiguity
//     and keep the diagnosis instead.
//
// The operator keeps the whole (masked) text on the sidecar's own log.
func (p *Proxy) httpError(w http.ResponseWriter, msg string, err error, code int) {
	masked := string(maskDecisionBytes([]byte(err.Error())))
	slog.Warn("proxy error returned to the sandbox", "msg", msg, "status", code, "err", masked)
	http.Error(w, msg+": "+p.redactTopology(masked), code)
}

// redactedEndpoint replaces an internal endpoint in a sandbox-facing error.
const redactedEndpoint = "<redacted>"

// ipLiteralRe matches an IPv4 or bracketed-IPv6 literal with an optional port.
// A literal address in an error string is either the vetted destination IP, the
// pinned control-plane address or the pinned corp-proxy address — all three are
// topology the sandbox is not given anywhere else.
var ipLiteralRe = regexp.MustCompile(`(\[[0-9a-fA-F:]+\]|\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})(:\d{1,5})?`)

// redactTopology removes internal endpoints from an already secret-masked error
// string. The p.topologyRe pass is scoped to the operator's configured endpoints,
// so a HOSTNAME the sandbox supplied itself survives it; the ipLiteralRe pass
// that follows is deliberately blanket — every address literal goes, including
// one the request itself named (see httpError for why the ambiguity fails
// closed).
func (p *Proxy) redactTopology(s string) string {
	for _, re := range p.topologyRe {
		s = re.ReplaceAllLiteralString(s, redactedEndpoint)
	}
	return ipLiteralRe.ReplaceAllLiteralString(s, redactedEndpoint)
}

// topologyPatterns compiles the operator-configured endpoints whose appearance
// in a sandbox-facing error is a disclosure: the control-plane base URL (with
// whatever internal path follows it) and the corp upstream proxy address. Built
// once at construction — this runs on an error path, not per request.
func topologyPatterns(controlPlaneURL string, up *upstreamProxy) []*regexp.Regexp {
	var vals []string
	add := func(v string) {
		if v != "" && !slices.Contains(vals, v) {
			vals = append(vals, v)
		}
	}
	add(controlPlaneURL)
	if u, err := url.Parse(controlPlaneURL); err == nil {
		add(u.Host)
	}
	if up != nil {
		add(up.addr)
		if h, _, err := net.SplitHostPort(up.addr); err == nil {
			add(h)
		}
	}
	// Longest first: the base URL must win over its own host substring.
	slices.SortFunc(vals, func(a, b string) int { return len(b) - len(a) })
	out := make([]*regexp.Regexp, 0, len(vals))
	for _, v := range vals {
		// Consume the path/query that follows the endpoint too — the internal API
		// route is part of what the sandbox learns from a wrapped transport error.
		out = append(out, regexp.MustCompile(regexp.QuoteMeta(v)+`[^"\s]*`))
	}
	return out
}

// The dial-failed shape: a request policy ALLOWED, where the network then
// lost it. Every emitting site needs the SAME two things httpError already
// gives the sandbox body — the secret mask and the topology redaction — in
// the DECISION LOG too (egress.DecisionLog.Cause), because auditScope hands a
// run's own CREATOR that whole row, not just the operator. One helper for
// both fields (Cause, Via) so a fifth call site cannot forget either pass.

// viaDirect and viaUpstreamProxy are egress.DecisionLog.Via's two class
// tokens — never an address (see that field's doc comment).
const (
	viaDirect        = "direct"
	viaUpstreamProxy = "upstream-proxy"
)

// dialFailureCause returns the MASKED, TOPOLOGY-REDACTED sentence naming why a
// dial-shaped refusal happened — the same two passes httpError applies to the
// sandbox-facing body, run here too because this text also lands in
// egress.DecisionLog.Cause.
func (p *Proxy) dialFailureCause(err error) string {
	masked := string(maskDecisionBytes([]byte(err.Error())))
	return p.redactTopology(masked)
}

// viaHop classifies which hop CLASS a forward-egress dial to host attempted —
// see egress.DecisionLog.Via's doc comment for why this is a class token and
// never the operator's corp-proxy address.
func (p *Proxy) viaHop(host string) string {
	if p.upstream != nil && !p.bypassUpstream(host) {
		return viaUpstreamProxy
	}
	return viaDirect
}

// dialStage names WHICH LEG of a dial-shaped failure produced err, so Cause
// says what actually failed instead of a blanket "dial failed" — an x509
// handshake failure read as a network dial failure hides which layer
// actually failed.
//
// A heuristic over the error TEXT, not a type switch: dialThroughUpstream's
// own stages (resolve/dial/CONNECT-handshake, upstream.go) and crypto/tls's
// handshake failures ("tls: ...", "x509: ...", a bare protocol alert) do not
// share one error type to switch on, and http.Transport re-wraps both behind
// a *url.Error before RoundTrip ever returns them to a caller here.
//
// ponytail: string-matching over the wrapped error text, not an exhaustive
// errors.As classification of every net/crypto-tls error shape — upgrade to
// typed matching if a stage this misclassifies turns up.
func dialStage(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "upstream proxy"), strings.Contains(msg, "upstream CONNECT"), strings.Contains(msg, "upstream buffer"):
		return "upstream proxy connect"
	case strings.Contains(msg, "tls:"), strings.Contains(msg, "x509:"), strings.Contains(msg, "remote error:"):
		return "tls handshake"
	default:
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Op == "dial" {
			return "tcp dial"
		}
		return "dial"
	}
}

// causeSentence builds the STAGE-PREFIXED, masked, topology-redacted sentence
// naming why a genuinely DIAL-shaped refusal happened — what
// egress.DecisionLog.Cause carries, and the same text an AWS-lane
// sandbox error body's "message" carries on the ONE site that shares both
// (forwardInspectedLLM's RoundTrip failure — see httpErrorAWSAware), so the
// audited cause and what the agent's own SDK is told are the same words.
//
// Callers for a non-dial refusal (a credential resolve failure, a gateway's
// own guard refusal) want dialFailureCause alone: dialStage's TCP-dial/
// TLS-handshake/upstream-proxy vocabulary does not apply to those and would
// mislabel a refusal as a "dial" it never attempted.
func (p *Proxy) causeSentence(err error) string {
	return dialStage(err) + ": " + p.dialFailureCause(err)
}

// denyDialFailed builds a dial-shaped deny decision: req is the request the
// earlier ALLOW was computed for (superseded here rather than over-reported,
// E3); scan carries forward any scan summary the superseded allow already
// attached (nil when there is none). ruleSource is taken as an argument, not
// hardcoded, so each real emitting site keeps "builtin:dial-failed" written
// out in full on ITS OWN line — docs/AUDIT-ACTIONS.md's rule_source table
// cites every one of them by exact line, which a shared symbol would hide
// this behind. This is still the sole construction point for the Cause/Via
// pair — see this file's header comment.
func (p *Proxy) denyDialFailed(ruleSource string, req egress.Request, host string, err error, scan *egress.ScanSummary) egress.DecisionLog {
	dl := decisionLog(req, egress.Deny, ruleSource)
	dl.Cause = p.causeSentence(err)
	dl.Via = p.viaHop(host)
	dl.Scan = scan
	return dl
}

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
// dialFailureCause): this sentence carries no upstream error text of its own,
// but the row is visible to the run's OWN CREATOR (auditScope), and running it
// through the shared pipeline anyway is one less place a future caller could
// forget the pass.
func (p *Proxy) upstreamProtocolMismatchCause(alpn string, hadTLS bool) string {
	masked := string(maskDecisionBytes([]byte(h2MismatchSentence(alpn, hadTLS))))
	return p.redactTopology(masked)
}

// denyUpstreamProtocolMismatch is denyDialFailed's sibling for isH2Preface's
// refusal: same Via computation, but Cause is the ALPN-aware h2-mismatch
// sentence rather than causeSentence's dial-stage-prefixed text, because the
// round trip COMPLETED and answered the wrong protocol — it never lost a
// dial. ruleSource is taken as an argument for the same reason denyDialFailed's
// is (see that function's doc comment): each emitting site keeps
// ruleSourceUpstreamProtocolMismatch written out on its own line, which is
// what docs/AUDIT-ACTIONS.md's rule_source table cites.
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
// every other host gets a plain 400 carrying the same sentence. A 400, not the
// 502 every other dial-shaped refusal answers with: a protocol mismatch never
// succeeds on retry, and 502 is exactly the class of error both AWS SDKs (and
// most others) retry.
func (p *Proxy) writeUpstreamProtocolMismatch(w http.ResponseWriter, host, msg, cause string) {
	slog.Warn("proxy error returned to the sandbox", "msg", msg, "status", http.StatusBadRequest, "err", cause)
	if isAWSLane(host) {
		writeAWSSDKError(w, http.StatusBadRequest, "UpstreamProtocolMismatchException", cause)
		return
	}
	http.Error(w, msg+": "+cause, http.StatusBadRequest)
}
