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
	"io"
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// defaultPortForScheme is the port an absolute-form request URI means when its
// authority carries no explicit one.
//
// F141: handlePlain hardwired 80, so `POST https://api.anthropic.com/v1/messages`
// on the forward lane was evaluated, VETTED and DIALLED as port 80 — the policy
// port matched against 80, the audit row RECORDED 80, and the transport then ran
// a TLS handshake against :80 — while the request plainly named the https origin.
// The port a decision row states has to be the port the proxy actually dials
// (docs/AUDIT-ACTIONS.md lists `port` as an egress.* detail field), and the
// allowlist has to be matched against the same one.
func defaultPortForScheme(scheme string) int {
	if strings.EqualFold(scheme, "https") {
		return 443
	}
	return 80
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
		p.writeEgressDeny(w, host, port, log)
		return
	case egress.Pending:
		if log != nil {
			p.sink.emit(*log)
		}
		setEgressRefusalHeaders(w, egressRefusalPending, host)
		writeApprovalPending(w, log)
		return
	}

	// Content inspection. TWO lanes converge here and the host decides which,
	// with the same questions handleConnect/serveMITMRequest ask:
	//
	//   - A MODEL host (isLLMHost) whose channel we can parse takes the LLM
	//     per-endpoint classifier — the handleConnect parity this lane never had
	//     (F103/F141). An absolute-form `POST https://api.anthropic.com/v1/messages`
	//     is the SAME prompt egress as the tunnel, and it used to be forwarded with
	//     the brokered credential, unscanned EVEN IN mode=block, under a single
	//     `allow / policy:allowed / scan=nil` row: no scan event, no blind marker,
	//     nothing an auditor could tell apart from a GET. inspectLLM writes its own
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
	channel := p.channelForHost(host)
	scanning := p.scanner != nil && p.scanner.Mode() != contentscan.ModeOff
	switch {
	case scanning && p.isLLMHost(host) && channel != contentscan.ChannelGeneric:
		bodyOverride, summary, blocked = p.inspectLLM(w, r, host, port, strings.TrimPrefix(r.URL.Path, "/"), channel)
	case scanning && p.scanner.InspectForwardEgress() && hasScannableBody(r):
		bodyOverride, summary, blocked = p.inspectForwardBody(w, r, host, port)
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
	p.inject.apply(outReq, host, port)

	// 6. Forward to the vetted target over the pinned transport. Its DialContext
	// dials the vetted ip:port carried on the request context (vettedIPKey), so the
	// host is never re-resolved. Invoked only post-allow+vet.
	resp, err := p.transport.RoundTrip(outReq)
	if err != nil {
		// The allow decision is emitted only AFTER a successful round-trip (same
		// accuracy fix as handleConnect, E3): a failed upstream dial must NOT
		// over-report an allow. Emit a dial-failed deny (carrying any scan
		// summary) instead.
		if log != nil {
			dl := decisionLog(log.Request, egress.Deny, "builtin:dial-failed")
			dl.Scan = log.Scan
			p.sink.emit(dl)
		}
		p.httpError(w, "upstream error", err, http.StatusBadGateway)
		return
	}
	if log != nil {
		p.sink.emit(*log)
	}
	defer func() { _ = resp.Body.Close() }()

	relay(w, resp)
}
