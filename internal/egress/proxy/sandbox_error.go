// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// This file is ONE subject: what a proxy error may say to the SANDBOX.
//
// Every sandbox-facing error in the package goes through Proxy.httpError, so
// the two things that must be true of such a body — the process-global
// credential mask, and the topology redaction — are decided here and
// nowhere else. Split out of proxy.go for the 1000-line file-size gate; the
// seam is the trust boundary, not an arbitrary cut.

// httpError writes "<msg>: <err>" to the SANDBOX with the process-global secret
// mask applied to the error text — the same mask the decision-log path uses. A
// proxy error routinely wraps upstream/control-plane text (see resolveInjection's
// status echo), and the sandbox is the one party that must never observe an
// injected credential, so error strings are masked here rather than at each site
// — every sandbox-facing error goes through this helper, so no handler in the
// package holds a raw err.Error().
// The mask is not enough on its own: maskDecisionBytes only replaces
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
