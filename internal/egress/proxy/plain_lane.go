// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The PLAIN forward lane: an absolute-form request URI the sandbox sends
// straight to the proxy listener (no CONNECT), which ServeHTTP routes here.
//
// Split out of proxy.go at the 1000-line gate, and a real seam rather than a
// size dodge: handleConnect and its TLS-terminated continuation already live
// beside each other in mitm.go, while this lane's own rules — what port an
// absolute-form URI means, which inspection core its host earns, and where the
// generic injector runs — were the one piece of that story still folded into
// the pipeline file. F103/F104/F141 were all instances of this lane silently
// diverging from the tunnel; keeping it in one place is how the divergence
// stays visible.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/egress"
)

const (
	// ruleSourceRequireTLS marks the one decision this lane makes on its own: a
	// request refused because its injection rule declares require_tls
	// (egress.InjectionRule) and the transport is cleartext. It is a `policy:`
	// value because the operator's authored rule is what denied it — not a
	// builtin, and not the evaluator, which allowed the host before this arm ran.
	ruleSourceRequireTLS = "policy:require-tls"
	// injectRequireTLSBody is the 403 body, with the host substituted.
	//
	// DRAFT (M2 canon pending).
	injectRequireTLSBody = "credential injection for %s requires TLS: " +
		"this rule sets require_tls and the request was plain HTTP"
)

// defaultPortForScheme is the port an absolute-form request URI means when its
// authority carries no explicit one.
//
// The port a decision row states has to be the port the proxy actually dials
// (docs/AUDIT-ACTIONS.md lists `port` as an egress.* detail field), and the
// allowlist has to be matched against the same one (F141): hardcoding port 80
// here would evaluate, vet and dial `POST https://api.anthropic.com/v1/messages`
// as port 80 — the policy port matched against 80, the audit row recording 80,
// and the transport running TLS against :80 — while the request plainly names
// the https origin.
func defaultPortForScheme(scheme string) int {
	if strings.EqualFold(scheme, "https") {
		return 443
	}
	return 80
}

// servePlain is the plain forward lane's entry. A host the run's Azure DevOps
// grant covers is refused here, before evaluation or any injection, because
// this lane never runs the REST gate (refuseADOPlain).
func (p *Proxy) servePlain(w http.ResponseWriter, r *http.Request) {
	if p.refuseADOPlain(w, r) {
		return
	}
	p.handlePlain(w, r)
}

// handlePlain forwards an absolute-URI plain HTTP request.
func (p *Proxy) handlePlain(w http.ResponseWriter, r *http.Request) {
	// A forward-proxy request carries an absolute URI; the host lives in the
	// URL, not just the Host header.
	if r.URL == nil || r.URL.Host == "" {
		http.Error(w, "proxy requires absolute-form request URI", http.StatusBadRequest)
		return
	}
	host, port := splitHostPort(r.URL.Host, defaultPortForScheme(r.URL.Scheme))

	decision, target, log := p.evaluate(r.Context(), host, port, r.Method, r.URL.Path)
	switch decision {
	case egress.Deny:
		if log != nil {
			p.sink.emit(*log)
		}
		// memoed=true for the same reason handleConnect passes it: evaluate's memo
		// arm is ahead of egressTarget, so a log-less Deny reaches THIS lane too,
		// and a memoed retry here must get the same 403 as the first attempt.
		p.writeEgressDeny(w, host, port, log, true)
		return
	case egress.Pending:
		if log != nil {
			p.sink.emit(*log)
		}
		setEgressRefusalHeaders(w, egressRefusalPending, host)
		writeApprovalPending(w, log)
		return
	}

	// 4. require_tls (F110's residual half): the operator declared that THIS
	// host's brokered credential may ride only a transport the proxy runs TLS on,
	// and this request is cleartext. Unlike injectableTransport's rules — which
	// are the proxy's own reading of a transport and therefore withhold the
	// credential silently — an authored require_tls refuses the REQUEST, so the
	// agent sees why instead of debugging a 401 from the upstream.
	//
	// It runs BEFORE the inspection block below, not beside the injection it
	// guards: a refusal is the end of this request, so buffering and scanning its
	// body first would spend the scan budget on bytes nothing forwards — and the
	// blind-coverage marker (emitLLMBlindOnce) would post a row saying a body went
	// UNINSPECTED to an upstream that never received it. A transport the operator
	// refused preempts the question of what was in it.
	//
	// It writes its OWN 403 rather than going through writeEgressDeny, which
	// special-cases the two builtin reasons and gives every other one the fixed
	// "egress denied by policy" body (policy.go) — useless for a transport
	// mistake an operator can fix in one line. The refusal HEADERS are the same
	// ones every other refusal on this lane sets, carrying the same rule_source
	// the decision row does, and the deny row REPLACES the allow row below
	// (nothing was forwarded, so an allow would be a false record — the same
	// accuracy rule the dial-failed arm follows).
	if p.inject.requiresTLS(host) && !strings.EqualFold(r.URL.Scheme, "https") {
		if log != nil {
			p.sink.emit(decisionLog(log.Request, egress.Deny, ruleSourceRequireTLS))
		}
		setEgressRefusalHeadersWithReason(w, egressRefusalDenied, host, ruleSourceRequireTLS)
		http.Error(w, fmt.Sprintf(injectRequireTLSBody, host), http.StatusForbidden)
		return
	}

	// Content inspection. TWO lanes converge here and the host decides which,
	// with the same questions handleConnect/serveMITMRequest ask:
	//
	//   - A MODEL host (isLLMHost) whose channel we can parse takes the LLM
	//     per-endpoint classifier — the handleConnect parity this lane never had
	//     (F103/F141). An absolute-form `POST https://api.anthropic.com/v1/messages`
	//     is the SAME prompt egress as the tunnel, so without this classifier it
	//     would forward with the brokered credential, unscanned EVEN IN mode=block,
	//     under a single `allow / policy:allowed / scan=nil` row: no scan event, no
	//     blind marker, nothing an auditor could tell apart from a GET. inspectLLM writes its own
	//     403 and its own scan:blocked decision when it refuses.
	//   - Every other host keeps the OPTIONAL generic inspection of a custom
	//     (non-LLM) HTTP connector's body — the walled-garden extension, opt-in via
	//     inspect_forward_egress. When disabled (the default) or for bodiless
	//     methods this is a no-op and the path below is byte-for-byte the original
	//     streaming forward. A confident block writes the 403 itself (before the
	//     allow decision is emitted).
	var (
		bodyOverride io.Reader
		summary      *egress.ScanSummary
		blocked      bool
	)
	// releaseBody returns the inspected body's bytes to maxRetainedScanBytes; it
	// has to outlive the RoundTrip that reads them.
	releaseBody := func() {}
	defer func() { releaseBody() }()
	channel := p.channelForHost(host)
	scanning := p.scanner != nil && p.scanner.Mode() != contentscan.ModeOff
	switch {
	case scanning && p.isLLMHost(host) && channel != contentscan.ChannelGeneric:
		bodyOverride, summary, releaseBody, blocked = p.inspectLLM(w, r, host, port, strings.TrimPrefix(r.URL.Path, "/"), channel)
	case scanning && p.scanner.InspectForwardEgress() && hasScannableBody(r):
		bodyOverride, summary, releaseBody, blocked = p.inspectForwardBody(w, r, host, port)
	}
	if blocked {
		return
	}
	if summary != nil && log != nil {
		log.Scan = summary
	}
	// Honest coverage, same rule as handleConnect's opaque-tunnel marker: an LLM
	// host we could NOT inspect on this lane (Bedrock/SigV4 and any gateway whose
	// channel is generic, when the operator has not opted generic bodies in)
	// carried a body nothing looked at. Say so once per host rather than letting
	// the bare allow imply coverage.
	if scanning && p.isLLMHost(host) && channel == contentscan.ChannelGeneric &&
		summary == nil && hasScannableBody(r) {
		p.emitLLMBlindOnce(host)
	}
	outReq := r.Clone(context.WithValue(r.Context(), vettedIPKey{}, target))
	// RequestURI must be empty for client requests.
	outReq.RequestURI = ""
	if bodyOverride != nil {
		// Forward the buffered (re-readable) body; bytes are unchanged so the
		// cloned ContentLength still matches.
		outReq.Body = io.NopCloser(bodyOverride)
	}
	// Strip hop-by-hop headers before forwarding.
	removeHopByHop(outReq.Header)

	// 5. Credential injection (plain HTTP only, exact-allow host only, and only
	// on a transport that may carry the credential — see injectableTransport).
	p.applyInjection(outReq, host, port)

	// 6. Forward to the vetted target over the pinned transport. Its DialContext
	// dials the vetted ip:port carried on the request context (vettedIPKey), so the
	// host is never re-resolved. Invoked only post-allow+vet.
	resp, err := p.roundTripUpstream(outReq)
	if err != nil {
		p.failUpstream(w, err, log, host, "upstream error")
		return
	}
	if log != nil {
		log.UpstreamFault = p.bedrockUpstreamFault(host, r.URL.Path, resp)
		p.sink.emit(*log)
	}
	defer func() { _ = resp.Body.Close() }()

	relay(w, resp)
}
