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

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/egress"
)

const (
	// ruleSourceRequireTLS marks this lane's own refusal: require_tls set but the
	// transport is cleartext — a `policy:` value since the authored rule denied it, not the evaluator.
	ruleSourceRequireTLS = "policy:require-tls"
	// injectRequireTLSBody is the 403 body, with the host substituted.
	//
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
	// A host name on the wire is ASCII: a client sends an international name in its xn-- form. The
	// transport maps any other spelling to an ASCII name before it pools a connection, names the TLS
	// server and offers a certificate, so the request would go to a host this lane never judged.
	if r.URL != nil && strings.ContainsFunc(r.URL.Host, func(c rune) bool { return c >= 0x80 }) {
		http.Error(w, "the request's host name is not ASCII; send an international name in its xn-- form", http.StatusBadRequest)
		return
	}
	if p.refuseADOPlain(w, r) || p.refuseAzurePlain(w, r) || p.refusePATAPIPlain(w, r) || p.refuseGitHubAPIPlain(w, r) ||
		p.refuseAzureARMPlain(w, r) || p.refuseAWSSigV4Plain(w, r) || p.refuseKubeAPIPlain(w, r) {
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

	recordPath := r.URL.Path
	if strings.HasPrefix(recordPath, routeMCPBroker) {
		// A harness that ignores NO_PROXY sends its MCP broker address through this lane: the record
		// carries the broker's redacted form, so the harness's token never reaches the audit.
		token, key, _ := parseMCPBrokerPath(recordPath)
		harness, _ := p.mcp.bind(token)
		recordPath = mcpRedactedPath(harness, key)
	}
	decision, target, log := p.evaluate(r.Context(), host, port, r.Method, recordPath)
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
		setEgressRefusalHeadersWithReason(w, egressRefusalPending, host, decisionReason(log))
		writeApprovalPending(w, log)
		return
	}
	r, refused := p.plainRefusedAfterPolicy(w, r, host, port, log) // r carries an approved hold to the forward
	if refused {
		return
	}

	// The run's allowed_models judges a model host's request on this lane as on the others.
	releaseModels, modelRefused := p.allowedModelRefused(w, r, host, port, strings.TrimPrefix(r.URL.Path, "/"))
	defer releaseModels()
	if modelRefused {
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
		p.emitLLMBlindOnce(r.Context(), host, port, r.Method)
	}
	outReq := r.Clone(context.WithValue(r.Context(), vettedIPKey{}, target))
	// RequestURI must be empty for client requests.
	outReq.RequestURI = ""
	// The transport pools and dials by this address, so it is the judged host and port and never
	// another spelling of them. An empty port ("host:") is judged as port 0, and left as written it
	// would ride a connection a signed call left open to the scheme's default port.
	outReq.URL.Host = upstreamURLHost(host, port, defaultPortForScheme(r.URL.Scheme))
	if bodyOverride != nil {
		// Forward the buffered, re-readable body; bytes are unchanged so ContentLength still matches.
		outReq.Body = io.NopCloser(bodyOverride)
	}
	removeHopByHop(outReq.Header)
	p.stripGatewayHeaders(outReq.Header)
	p.stripGatewayHeaders(outReq.Trailer) // r.Clone keeps the sandbox's trailers
	if rt := p.gatewayRouteFor(host, port, r.URL.Path); rt != nil {
		p.forwardPlainGatewayRoute(w, r, outReq, rt, host, port, log)
		return
	}

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
		p.emitDialledAllow(*log, host)
	}
	defer func() { _ = resp.Body.Close() }()

	p.relay(w, resp)
}

// forwardPlainGatewayRoute is the plain lane's tail for a request a configured identity route
// covers, the same the brokered and terminated lanes give it (forwardInspectedLLM): the run's
// token and a fresh call id go on, or nothing is sent, and the answer is always scrubbed. A route
// configured to sign never sends unsigned, since a lenient gateway would take the call as
// anonymous. The request goes out in the scheme those lanes use (upstreamSchemeFor), not the one
// the sandbox picked, so the token never rides cleartext to a TLS gateway.
func (p *Proxy) forwardPlainGatewayRoute(w http.ResponseWriter, r, outReq *http.Request, rt *GatewayIdentityRoute, host string, port int, log *egress.DecisionLog) {
	ruleSource := ""
	if log != nil {
		ruleSource = log.RuleSource
	}
	// The authority is the judged host and port in the outgoing scheme's own form, as
	// forwardInspectedLLM builds it. The transport pools, remembers HTTP/2 and offers a client
	// certificate by this address, and the scheme the sandbox picked may imply another port.
	scheme, defaultPort := p.upstreamSchemeFor(host, port)
	outReq.URL.Scheme, outReq.URL.Host = scheme, upstreamURLHost(host, port, defaultPort)
	outReq.Host = outReq.URL.Host
	p.applyInjection(outReq, host, port)
	callID := uuid.NewString()
	if !p.attachGatewayIdentity(w, r, outReq.Header, rt, host, port, ruleSource, callID) {
		return
	}
	outReq.Header.Set(gatewayCallIDHeader, callID)
	if log != nil {
		log.GatewayCallID = callID
	}
	resp, err := p.roundTripUpstream(outReq)
	if err != nil {
		p.failUpstream(w, err, log, host, "upstream error")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if log != nil {
		log.UpstreamFault = p.bedrockUpstreamFault(host, r.URL.Path, resp)
		p.emitDialledAllow(*log, host)
	}
	p.relayGatewayRoute(w, r, host, port, resp, ruleSource, outReq.Header.Get(gatewayIdentityHeader), callID)
}

// plainRefusedAfterPolicy is handlePlain's refusals once the policy allows the host, and reports whether
// it answered. The request it returns is the one to forward (hostRuleRefused).
func (p *Proxy) plainRefusedAfterPolicy(w http.ResponseWriter, r *http.Request, host string, port int, log *egress.DecisionLog) (*http.Request, bool) {
	ruleSource := ""
	if log != nil {
		ruleSource = log.RuleSource
	}
	// A route's host and port is sent only what a route covers (gatewayOffRouteRefused), and this
	// runs before a hold can ask anyone about a call that will not be sent.
	if p.gatewayOffRouteRefused(w, r, host, port, r.URL.Path, ruleSource) {
		return r, true
	}
	// A request the host rules hold asks a person only now, once the policy allows the host (host_call_hold.go).
	r, refused := p.hostRuleRefused(w, r, host, port)
	if refused {
		return r, true
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
		return r, true
	}
	return r, false
}
