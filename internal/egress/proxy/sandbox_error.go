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

// TRUST BOUNDARY: this file is one subject — what a proxy error may say to
// the SANDBOX. Every sandbox-facing error goes through Proxy.httpError, so
// the credential mask and topology redaction are decided here and nowhere
// else. Split out of proxy.go for the file-size gate; the seam is the boundary.

// httpError writes "<msg>: <err>" to the SANDBOX, secret-masked with the same
// process-global mask the decision-log path uses (proxy errors wrap
// upstream/control-plane text the sandbox must never see).
//
// SECURITY: masking alone isn't enough — a transport error always embeds the
// failed ENDPOINT (control-plane, corp proxy, or a vetted IP private under
// internal_hosts), so the body also loses all topology. Endpoint patterns are
// scoped to operator-configured addresses (sandbox-named hosts survive); the
// IP-literal pass is BLANKET, since once an address is inside an error string
// the proxy can't tell vetted from internal, so it fails closed. The operator
// keeps the full masked text on the sidecar's own log.
func (p *Proxy) httpError(w http.ResponseWriter, msg string, err error, code int) {
	masked := string(maskDecisionBytes([]byte(err.Error())))
	slog.Warn("proxy error returned to the sandbox", "msg", msg, "status", code, "err", masked)
	http.Error(w, msg+": "+p.redactTopology(masked), code)
}

// redactedEndpoint replaces an internal endpoint in a sandbox-facing error.
const redactedEndpoint = "<redacted>"

// ipLiteralRe matches an IP literal with an optional port — topology the
// sandbox has no other way to see.
var ipLiteralRe = regexp.MustCompile(`(\[[0-9a-fA-F:]+\]|\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})(:\d{1,5})?`)

// redactTopology removes internal endpoints from an already secret-masked
// error string: p.topologyRe is scoped to operator-configured endpoints (a
// sandbox-supplied hostname survives); ipLiteralRe is deliberately blanket
// (see httpError for why).
func (p *Proxy) redactTopology(s string) string {
	for _, re := range p.topologyRe {
		s = re.ReplaceAllLiteralString(s, redactedEndpoint)
	}
	return ipLiteralRe.ReplaceAllLiteralString(s, redactedEndpoint)
}

// topologyPatterns compiles the operator-configured endpoints whose
// appearance in a sandbox-facing error is a disclosure: the control-plane
// base URL (with any internal path following it) and the corp upstream proxy
// address. Built once at construction, not per request.
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
// lost it. Every emitting site needs httpError's mask+redaction in the
// DECISION LOG too, since auditScope hands a run's own CREATOR that whole row.

// viaDirect and viaUpstreamProxy are egress.DecisionLog.Via's two class
// tokens — never an address.
const (
	viaDirect        = "direct"
	viaUpstreamProxy = "upstream-proxy"
)

// dialFailureCause returns the masked, topology-redacted sentence naming why
// a dial-shaped refusal happened, since this text also lands in
// egress.DecisionLog.Cause.
func (p *Proxy) dialFailureCause(err error) string {
	masked := string(maskDecisionBytes([]byte(err.Error())))
	return p.redactTopology(masked)
}

// viaHop classifies which hop CLASS a forward-egress dial to host attempted —
// a class token, never the operator's corp-proxy address (see
// egress.DecisionLog.Via).
func (p *Proxy) viaHop(host string) string {
	if p.upstream != nil && !p.bypassUpstream(host) {
		return viaUpstreamProxy
	}
	return viaDirect
}

// dialStage names WHICH LEG of a dial-shaped failure produced err, so Cause
// says what actually failed instead of a blanket "dial failed". A heuristic
// over the error TEXT since the stages share no common error type and
// http.Transport re-wraps them all behind a *url.Error.
//
// ponytail: string-matching, not exhaustive errors.As over every
// net/crypto-tls shape — upgrade if a misclassified stage turns up.
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

// causeSentence builds the stage-prefixed, masked, topology-redacted sentence
// naming why a genuinely DIAL-shaped refusal happened, so the audited cause
// and what the agent's SDK is told (httpErrorAWSAware) match. A non-dial
// refusal wants dialFailureCause alone, since dialStage's vocabulary would
// mislabel it as a dial never attempted.
func (p *Proxy) causeSentence(err error) string {
	return dialStage(err) + ": " + p.dialFailureCause(err)
}

// denyDialFailed builds a dial-shaped deny decision: req is the request the
// earlier ALLOW was computed for (superseded, not over-reported); scan
// forwards any scan summary that allow attached. ruleSource is an argument,
// not hardcoded, so each site spells "builtin:dial-failed" out for
// docs/AUDIT-ACTIONS.md's rule_source table.
func (p *Proxy) denyDialFailed(ruleSource string, req egress.Request, host string, err error, scan *egress.ScanSummary) egress.DecisionLog {
	dl := decisionLog(req, egress.Deny, ruleSource)
	dl.Cause = p.causeSentence(err)
	dl.Via = p.viaHop(host)
	dl.Scan = scan
	return dl
}
