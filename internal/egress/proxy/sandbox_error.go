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

// This file is ONE subject: what a proxy error may say to the SANDBOX. Every
// sandbox-facing error goes through Proxy.httpError, so the credential mask
// and topology redaction are decided here and nowhere else. Split out of
// proxy.go for the 1000-line file-size gate; the seam is the trust boundary.

// httpError writes "<msg>: <err>" to the SANDBOX with the process-global
// secret mask applied — the same mask the decision-log path uses, since a
// proxy error routinely wraps upstream/control-plane text the sandbox must
// never observe.
//
// The mask alone is not enough: it only replaces registered credentials, and
// a Go transport error always embeds the ENDPOINT that failed (the
// control-plane address, the corp upstream proxy, or a vetted destination IP
// that under internal_hosts is itself private) — none of which the sandbox
// is given any other way. So the body also loses all topology while keeping
// the message and diagnosis: endpoint patterns are scoped to the operator's
// configured addresses (a hostname the sandbox itself named survives), while
// the IP-literal pass is a BLANKET redaction, sandbox-named addresses
// included — once an address is inside a transport error string the proxy
// cannot tell a vetted destination from a pinned internal one, so it fails
// closed on the ambiguity. The operator keeps the whole (masked) text on the
// sidecar's own log.
func (p *Proxy) httpError(w http.ResponseWriter, msg string, err error, code int) {
	masked := string(maskDecisionBytes([]byte(err.Error())))
	slog.Warn("proxy error returned to the sandbox", "msg", msg, "status", code, "err", masked)
	http.Error(w, msg+": "+p.redactTopology(masked), code)
}

// redactedEndpoint replaces an internal endpoint in a sandbox-facing error.
const redactedEndpoint = "<redacted>"

// ipLiteralRe matches an IPv4 or bracketed-IPv6 literal with an optional port
// — topology the sandbox is not given anywhere else.
var ipLiteralRe = regexp.MustCompile(`(\[[0-9a-fA-F:]+\]|\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})(:\d{1,5})?`)

// redactTopology removes internal endpoints from an already secret-masked
// error string: p.topologyRe is scoped to operator-configured endpoints (a
// sandbox-supplied hostname survives), while ipLiteralRe is deliberately
// blanket (see httpError for why the ambiguity fails closed).
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
		// Consume the path/query too — the internal API route is part of what
		// leaks from a wrapped transport error.
		out = append(out, regexp.MustCompile(regexp.QuoteMeta(v)+`[^"\s]*`))
	}
	return out
}

// The dial-failed shape: a request policy ALLOWED, where the network then
// lost it. Every emitting site needs the SAME mask+redaction httpError gives
// the sandbox body in the DECISION LOG too (egress.DecisionLog.Cause), since
// auditScope hands a run's own CREATOR that whole row, not just the operator.

// viaDirect and viaUpstreamProxy are egress.DecisionLog.Via's two class
// tokens — never an address (see that field's doc comment).
const (
	viaDirect        = "direct"
	viaUpstreamProxy = "upstream-proxy"
)

// dialFailureCause returns the MASKED, TOPOLOGY-REDACTED sentence naming why a
// dial-shaped refusal happened — the same two passes as httpError, since this
// text also lands in egress.DecisionLog.Cause.
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
// says what actually failed instead of a blanket "dial failed". A heuristic
// over the error TEXT, not a type switch: the various stages don't share one
// error type, and http.Transport re-wraps them all behind a *url.Error.
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
// naming why a genuinely DIAL-shaped refusal happened, so the audited cause
// and what the agent's own SDK is told (httpErrorAWSAware) are the same words.
//
// Callers for a non-dial refusal want dialFailureCause alone: dialStage's
// vocabulary does not apply and would mislabel a refusal as a dial it never
// attempted.
func (p *Proxy) causeSentence(err error) string {
	return dialStage(err) + ": " + p.dialFailureCause(err)
}

// denyDialFailed builds a dial-shaped deny decision: req is the request the
// earlier ALLOW was computed for (superseded here rather than over-reported);
// scan carries forward any scan summary that allow already attached. ruleSource
// is an argument, not hardcoded, so each emitting site keeps
// "builtin:dial-failed" written out in full on its own line for
// docs/AUDIT-ACTIONS.md's rule_source table.
func (p *Proxy) denyDialFailed(ruleSource string, req egress.Request, host string, err error, scan *egress.ScanSummary) egress.DecisionLog {
	dl := decisionLog(req, egress.Deny, ruleSource)
	dl.Cause = p.causeSentence(err)
	dl.Via = p.viaHop(host)
	dl.Scan = scan
	return dl
}
