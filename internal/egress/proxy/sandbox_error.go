// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"crypto/x509"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

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
	masked := truncateText(string(maskDecisionBytes([]byte(err.Error()))), maxLoggedErrBytes)
	slog.Warn("proxy error returned to the sandbox", "msg", msg, "status", code, "err", masked)
	http.Error(w, msg+": "+p.redactErr(err), code)
}

// redactErr is err's text as the sandbox may see it: secret-masked, with no
// topology, and with no name off a certificate chain. A hostname mismatch
// lists the gateway's own names (redacted for a configured gateway only, since
// its Host says which); an unknown-authority hint and a name-constraint
// refusal carry no Host, so their CA and constraint names go for every host.
//
// An error whose text the upstream wrote (peerTextClass) is a fixed phrase
// instead, and every answer is cut to maxCauseBytes.
func (p *Proxy) redactErr(err error) string {
	if phrase, ok := peerTextClass(err); ok {
		return phrase
	}
	s := err.Error()
	var mismatch x509.HostnameError
	if errors.As(err, &mismatch) && mismatch.Certificate != nil {
		if _, ok := p.gatewayVendorOf(mismatch.Host); ok {
			s = strings.Replace(s, "valid for "+certNames(mismatch)+", not ", "valid for "+redactedEndpoint+", not ", 1)
		}
	}
	s = candidateAuthorityRe.ReplaceAllString(s, `${1}"`+redactedEndpoint+`"`)
	var invalid x509.CertificateInvalidError
	if errors.As(err, &invalid) && invalid.Detail != "" &&
		(invalid.Reason == x509.CANotAuthorizedForThisName || invalid.Reason == x509.UnconstrainedName) {
		s = strings.ReplaceAll(s, invalid.Detail, redactedEndpoint)
	}
	// An error flattened to text (no %w chain) carries the same detail; blank it by its prefix.
	s = constraintDetailRe.ReplaceAllString(s, `${1}`+redactedEndpoint)
	return truncateText(p.redactTopology(string(maskDecisionBytes([]byte(s)))), maxCauseBytes)
}

// maxCauseBytes bounds redactErr's sentence, which is a decision row's Cause
// and the sandbox's error body; maxLoggedErrBytes bounds the operator's log line.
const (
	maxCauseBytes     = 512
	maxLoggedErrBytes = 4096
)

// truncateText cuts s to at most n bytes on a rune boundary, marking the cut.
func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + " [truncated]"
}

// constraintDetailRe matches x509.CertificateInvalidError's text for a name-constraint refusal up to the end
// of its line, for an error whose chain redactErr cannot walk.
var constraintDetailRe = regexp.MustCompile(`(x509: (?:a root or intermediate certificate is not authorized to sign for this name|issuer has name constraints but leaf contains unknown or unconstrained name): )[^\n]*`)

// candidateAuthorityRe matches the quoted CA subject x509.UnknownAuthorityError
// appends as its hint (its fields are unexported, so the text is all there is).
var candidateAuthorityRe = regexp.MustCompile(`(while trying to verify candidate authority certificate )"(?:[^"\\]|\\.)*"`)

// certNames is the name list x509.HostnameError's text prints: the IP SANs
// for an IP host, else the DNS SANs.
func certNames(e x509.HostnameError) string {
	if net.ParseIP(e.Host) != nil {
		ips := make([]string, len(e.Certificate.IPAddresses))
		for i, ip := range e.Certificate.IPAddresses {
			ips[i] = ip.String()
		}
		return strings.Join(ips, ", ")
	}
	return strings.Join(e.Certificate.DNSNames, ", ")
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

// gatewayPatterns matches each configured LLM gateway's host, with any port:
// the sandbox reaches a gateway only through its brokered route, so its
// address in an error (a certificate's hostname mismatch, say) is topology.
// Word-bounded so a short host name does not match inside other text. An
// IPv4 literal is left to ipLiteralRe; an IPv6 one is not, since x509 prints
// it bare ("cannot validate certificate for fd12::7 because ...") and
// ipLiteralRe matches IPv6 only in brackets, so both forms are listed here,
// as configured and canonical.
func gatewayPatterns(ups map[string]llmUpstream) []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, u := range ups {
		switch ip := net.ParseIP(u.host); {
		case u.host == "":
		case ip == nil:
			out = append(out, regexp.MustCompile(`\b`+regexp.QuoteMeta(u.host)+`\b(:\d{1,5})?`))
		case strings.Contains(u.host, ":"):
			for _, h := range slices.Compact([]string{u.host, ip.String()}) {
				q := regexp.QuoteMeta(h)
				out = append(out, regexp.MustCompile(`\[`+q+`\](:\d{1,5})?`), regexp.MustCompile(wordEdges(h, q)))
			}
		}
	}
	return out
}

// wordEdges word-bounds pattern q for literal h on each side where h starts
// or ends with a word character (an IPv6 literal may start or end with ':').
func wordEdges(h, q string) string {
	isWord := func(c byte) bool {
		return c == '_' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
	}
	if isWord(h[0]) {
		q = `\b` + q
	}
	if isWord(h[len(h)-1]) {
		q += `\b`
	}
	return q
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
	return p.redactErr(err)
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
	}
	// Only a dial, or a failure handshakeOver marks as connecting, is before
	// the connection opened. Anything else broke an open connection (a reset,
	// an idle close, an HTTP/2 GOAWAY or stream reset while the answer was
	// awaited), which classifyUpstreamErr calls upstream_error.
	var opErr *net.OpError
	var connecting *connectStageError
	switch {
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return "tcp dial"
	case errors.As(err, &connecting):
		return "dial"
	}
	return "after connect"
}

// causeSentence builds the stage-prefixed, masked, topology-redacted sentence
// naming why a genuinely DIAL-shaped refusal happened, so the audited cause
// and what the agent's SDK is told (writeBrokeredFault) match. A non-dial
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
