// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The PLAIN forward lane: an absolute-form request URI the sandbox sends straight to
// the proxy listener (no CONNECT). Its own rules for port defaulting, inspection routing,
// and injection live here so any divergence from the tunnel (mitm.go) stays visible.

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
	// ruleSourceRequireTLS marks this lane's own refusal: require_tls set but the
	// transport is cleartext — a `policy:` value since the authored rule denied it, not the evaluator.
	ruleSourceRequireTLS = "policy:require-tls"
	// injectRequireTLSBody is the 403 body, with the host substituted.
	//
	// DRAFT (M2 canon pending).
	injectRequireTLSBody = "credential injection for %s requires TLS: " +
		"this rule sets require_tls and the request was plain HTTP"
)

// defaultPortForScheme is the port an absolute-form URI implies when its authority is
// silent on one. The decision row, allowlist match, and dial must all agree on this port,
// or an https:// request could be evaluated and dialed as port 80.
func defaultPortForScheme(scheme string) int {
	if strings.EqualFold(scheme, "https") {
		return 443
	}
	return 80
}

// servePlain is the plain lane's entry; a host covered by the run's ADO grant is refused
// here, before evaluation or injection, since this lane skips the REST gate (refuseADOPlain).
func (p *Proxy) servePlain(w http.ResponseWriter, r *http.Request) {
	if p.refuseADOPlain(w, r) || p.refuseAzurePlain(w, r) {
		return
	}
	p.handlePlain(w, r)
}

func (p *Proxy) handlePlain(w http.ResponseWriter, r *http.Request) {
	// The host lives in the absolute-form URL, not the Host header.
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
		// memoed=true for the same reason handleConnect passes it — a retry must see the
		// same 403 as the first attempt.
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

	// require_tls: the operator declared this host's credential may ride only TLS, and this
	// request is cleartext — refuse the REQUEST before inspection, since a refusal ends it and
	// scanning first would waste budget. Writes its own 403 since this is an operator-fixable mistake.
	if p.inject.requiresTLS(host) && !strings.EqualFold(r.URL.Scheme, "https") {
		if log != nil {
			p.sink.emit(decisionLog(log.Request, egress.Deny, ruleSourceRequireTLS))
		}
		setEgressRefusalHeadersWithReason(w, egressRefusalDenied, host, ruleSourceRequireTLS)
		http.Error(w, fmt.Sprintf(injectRequireTLSBody, host), http.StatusForbidden)
		return
	}

	// Content inspection: two lanes converge here based on host. A model host (isLLMHost)
	// whose channel we can parse takes the LLM per-endpoint classifier — handleConnect
	// parity, so a prompt isn't forwarded unscanned in mode=block. Every other host gets
	// only the optional generic body inspection (inspect_forward_egress), a no-op when
	// disabled or bodiless.
	var (
		bodyOverride io.Reader
		summary      *egress.ScanSummary
		blocked      bool
	)
	// releaseBody returns inspected bytes to maxRetainedScanBytes; must outlive the RoundTrip reading them.
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
	// Honest coverage, same as handleConnect's opaque-tunnel marker: an LLM host we could
	// NOT inspect on this lane had its body seen by nothing — say so once rather than let
	// the allow imply coverage.
	if scanning && p.isLLMHost(host) && channel == contentscan.ChannelGeneric &&
		summary == nil && hasScannableBody(r) {
		p.emitLLMBlindOnce(host)
	}
	outReq := r.Clone(context.WithValue(r.Context(), vettedIPKey{}, target))
	// RequestURI must be empty for client requests.
	outReq.RequestURI = ""
	if bodyOverride != nil {
		// Forward the buffered, re-readable body; bytes are unchanged so ContentLength still matches.
		outReq.Body = io.NopCloser(bodyOverride)
	}
	removeHopByHop(outReq.Header)

	// Credential injection: plain HTTP only, exact-allow host only, and only on a transport
	// that may carry the credential — see injectableTransport.
	p.applyInjection(outReq, host, port)

	// Forward over the pinned transport, dialing the vetted ip:port from the request
	// context (vettedIPKey) so the host is never re-resolved.
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
