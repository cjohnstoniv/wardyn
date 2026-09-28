// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The PLAIN forward lane: an absolute-form request URI the sandbox sends
// straight to the proxy listener (no CONNECT), which ServeHTTP routes here.
// This lane's own rules — what port an absolute-form URI means, which
// inspection core its host earns, and where the generic injector runs — live
// together here so any divergence from the tunnel (mitm.go) stays visible.

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
	// request refused because its injection rule declares require_tls and the
	// transport is cleartext. A `policy:` value since the operator's authored
	// rule is what denied it, not the evaluator (which already allowed the host).
	ruleSourceRequireTLS = "policy:require-tls"
	// injectRequireTLSBody is the 403 body, with the host substituted.
	//
	// DRAFT (M2 canon pending).
	injectRequireTLSBody = "credential injection for %s requires TLS: " +
		"this rule sets require_tls and the request was plain HTTP"
)

// defaultPortForScheme is the port an absolute-form request URI means when
// its authority carries no explicit one. The decision row, the allowlist
// match and the actual dial must all use the same port — hardcoding 80 here
// would evaluate, vet and dial an https:// request as port 80 while the
// request plainly names the https origin.
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
		// memoed=true for the same reason handleConnect passes it: a memoed
		// retry here must get the same 403 as the first attempt.
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

	// 4. require_tls: the operator declared that THIS host's brokered
	// credential may ride only TLS, and this request is cleartext — an
	// authored require_tls refuses the REQUEST (unlike injectableTransport's
	// rules, which withhold the credential silently). Runs BEFORE the
	// inspection block below: a refusal is the end of this request, so
	// scanning its body first would spend budget on bytes nothing forwards.
	// Writes its own 403 (not writeEgressDeny's fixed body) since this is a
	// transport mistake an operator can fix in one line.
	if p.inject.requiresTLS(host) && !strings.EqualFold(r.URL.Scheme, "https") {
		if log != nil {
			p.sink.emit(decisionLog(log.Request, egress.Deny, ruleSourceRequireTLS))
		}
		setEgressRefusalHeadersWithReason(w, egressRefusalDenied, host, ruleSourceRequireTLS)
		http.Error(w, fmt.Sprintf(injectRequireTLSBody, host), http.StatusForbidden)
		return
	}

	// Content inspection. TWO lanes converge here and the host decides which:
	//   - A MODEL host (isLLMHost) whose channel we can parse takes the LLM
	//     per-endpoint classifier, giving this lane handleConnect's parity —
	//     without it a prompt would forward unscanned even in mode=block.
	//     inspectLLM writes its own 403 when it refuses.
	//   - Every other host keeps the OPTIONAL generic inspection of a custom
	//     HTTP connector's body (inspect_forward_egress). Disabled (default)
	//     or bodiless, this is a no-op and the path is the original streaming
	//     forward.
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
	// Honest coverage, same rule as handleConnect's opaque-tunnel marker: an
	// LLM host we could NOT inspect on this lane carried a body nothing
	// looked at. Say so once per host rather than letting the allow imply coverage.
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

	// 6. Forward over the pinned transport, which dials the vetted ip:port
	// carried on the request context (vettedIPKey) so the host is never
	// re-resolved.
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
