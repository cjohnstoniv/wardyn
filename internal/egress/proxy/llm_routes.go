// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The brokered LLM routes (/wardyn/llm/anthropic/, /wardyn/llm/openai/):
// reverse-proxying to the api-key vendor host (or an operator-configured
// internal gateway, see llmUpstream/vetTrustedHost), credential injection,
// outbound content inspection, and the classifiers deciding what gets
// scanned.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/egress"
)

const (
	// llmAnthropicPrefix selects the Anthropic LLM passthrough.
	llmAnthropicPrefix = "/wardyn/llm/anthropic/"
	anthropicHost      = "api.anthropic.com"
	// llmOpenAIPrefix selects the OpenAI reverse-proxy passthrough (Codex).
	llmOpenAIPrefix = "/wardyn/llm/openai/"
	openaiHost      = "api.openai.com"
	// maxBlindHosts bounds the per-run opaque-tunnel dedup set (emitLLMBlindOnce).
	maxBlindHosts = 64
	ruleSourceLLM = "brokered:llm"
	// ruleSourceLLMBlocked: refused by content inspection. ruleSourceLLMBlind:
	// opaque CONNECT to an LLM host inspection couldn't see into.
	ruleSourceLLMBlocked = "scan:blocked"
	ruleSourceLLMBlind   = "scan:opaque-tunnel"
	// ruleSourceLLMMITM marks a request inspected via TLS-MITM interception
	// of an otherwise-opaque CONNECT tunnel.
	ruleSourceLLMMITM = "scan:mitm"
)

// handleLLMAnthropic proxies /wardyn/llm/anthropic/<rest> to
// https://api.anthropic.com/<rest> (or a configured internal gateway) with
// the brokered Anthropic credential.
func (p *Proxy) handleLLMAnthropic(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, llmAnthropicPrefix)
	host, port, prefix := p.llmUpstream(anthropicHost)
	r = withRouteShape(r, shapeAnthropic)
	if p.llmRequestRefused(w, r, host, port, rest) {
		return
	}
	p.proxyLLMRequest(w, r, host, port, joinLLMPath(prefix, rest), contentscan.ChannelAnthropicMessages)
}

// handleLLMOpenAI proxies /wardyn/llm/openai/<rest> to
// https://api.openai.com/<rest> (or a configured internal gateway) with the
// brokered OpenAI credential (the Codex reverse-proxy route).
func (p *Proxy) handleLLMOpenAI(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, llmOpenAIPrefix)
	host, port, prefix := p.llmUpstream(openaiHost)
	r = withRouteShape(r, shapeOpenAI)
	if p.llmRequestRefused(w, r, host, port, rest) {
		return
	}
	p.proxyLLMRequest(w, r, host, port, joinLLMPath(prefix, rest), contentscan.ChannelOpenAIChat)
}

// llmRequestRefused answers a brokered LLM request that may not be forwarded
// and reports whether it did. allowed_methods applies here as on the forward
// and MITM lanes, with the same 403 and policy:method decision. rest is sent
// as-is after the gateway's path prefix, so a "." or ".." segment (see
// isDotSegment), a backslash or a control character in it is refused with a
// 400, never normalised and forwarded.
func (p *Proxy) llmRequestRefused(w http.ResponseWriter, r *http.Request, host string, port int, rest string) bool {
	if !p.evaluator.MethodAllowed(r.Method) {
		log := decisionLog(p.reqOf(r, host, port), egress.Deny, "policy:method")
		if p.sink != nil {
			p.sink.emit(log)
		}
		p.writeEgressDenyLLM(w, p.shapeOf(r, host), host, port, &log)
		return true
	}
	if strings.ContainsFunc(rest, func(c rune) bool { return c == '\\' || c < 0x20 || c == 0x7f }) ||
		slices.ContainsFunc(strings.Split(rest, "/"), isDotSegment) {
		p.emitLLMDecision(r, host, port, egress.Deny, ruleSourceLLM, nil)
		if shape := p.shapeOf(r, host); shape != shapePlain {
			writeProviderError(w, shape, http.StatusBadRequest, reasonUnsupportedPath, "wardyn: unsupported llm request path", nil)
			return true
		}
		http.Error(w, "unsupported llm request path", http.StatusBadRequest)
		return true
	}
	return false
}

// joinLLMPath prepends an internal gateway's path prefix (from its base URL)
// onto rest. Empty prefix (unset-gateway default) returns rest unchanged.
func joinLLMPath(prefix, rest string) string {
	if prefix == "" {
		return rest
	}
	return strings.TrimPrefix(prefix, "/") + "/" + rest
}

const (
	// llmNoCredentialDetail is the brokered-LLM 404's detail when the control
	// plane composed none.
	//
	llmNoCredentialDetail = "no credential is configured for this brokered LLM route"

	// llmBelowPolicyClause is appended to EVERY brokered-LLM 404 detail: a
	// missing model credential is below policy, not something to retry or approve.
	//
	llmBelowPolicyClause = "this is below policy: it cannot be approved, and no policy edit changes it."
)

// llm404Detail is the brokered-LLM 404's self-explaining detail: the
// deployment's model posture (Config.LLMUnavailableDetail) or the generic
// route sentence, plus the below-policy clause either way.
func llm404Detail(configured string) string {
	detail := strings.TrimSpace(configured)
	if detail == "" {
		detail = llmNoCredentialDetail
	}
	return detail + " — " + llmBelowPolicyClause
}

// proxyLLMRequest is the shared reverse-proxy + inspection path for a
// brokered LLM upstream: applies the startup-minted credential, strips every
// sandbox-supplied credential header, optionally inspects the body (blocking
// BEFORE the allow decision is recorded), and forwards to host:port/<rest>
// over the vetted IP. Shared by the /wardyn/llm/* local routes and the
// TLS-MITM CONNECT interception (serveMITM).
func (p *Proxy) proxyLLMRequest(w http.ResponseWriter, r *http.Request, host string, port int, rest string, channel contentscan.Channel) {
	// A local model server's gate runs before anything else (local_model.go);
	// one with no auth configured is served with no credential at all.
	lm := p.localModelAt(host, port)
	if lm != nil {
		release, refused := p.localModelRefused(w, r, host, port, rest, lm)
		defer release()
		if refused {
			return
		}
	}
	hdr, ok, ierr := p.inject.resolveCtx(r.Context(), host)
	if !ok && lm == nil {
		p.emitLLMDecision(r, host, port, egress.Deny, ruleSourceLLM, nil)
		extra := map[string]any{"wardyn": "no_llm_credential", "detail": llm404Detail(p.llmUnavailableDetail)}
		if shape := p.shapeOf(r, host); shape != shapePlain {
			dest := p.hostPortLabel(shape, host, port)
			writeProviderError(w, shape, http.StatusNotFound, reasonCredentialUnavailable,
				brokeredMessage(reasonCredentialUnavailable, dest, ruleSourceLLM, extra["detail"].(string), ""), extra)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		body, _ := json.Marshal(extra)
		_, _ = w.Write(body)
		return
	}
	if ierr != nil && r.Context().Err() != nil {
		return // the caller hung up mid re-resolve and was never refused
	}

	// A CONTROL-PLANE-authored gateway gets gatewayTarget's relaxed vet; every
	// other host gets egressTarget's SSRF-guarded vet. Vetted before a failed
	// refresh is reported: an egress refusal of the host is the fault to fix
	// first, and a refreshed credential would not get past it.
	target, err := p.llmRouteTarget(host, port)
	if err != nil {
		// A refused/unreachable configured gateway is vetTrustedHost's own
		// guard refusal, distinguished in the log as misconfiguration, not a
		// network fault.
		source := ruleSourceLLM
		if errors.Is(err, errGatewayVet) {
			source = ruleSourceGatewayVetFailed
		}
		p.emitLLMDecision(r, host, port, egress.Deny, source, nil)
		// An SDK hands plain text straight to a JSON parser and crashes on it.
		p.httpErrorShaped(w, p.shapeOf(r, host), host, "llm upstream vet failed", err, http.StatusBadGateway, vetFault(port, source, err))
		return
	}

	// A rule exists but its credential could not be refreshed: the control plane's answer, not the
	// below-policy 404 above, so the row carries the reason and the sandbox gets the MITM lane's error.
	if ierr != nil {
		if p.sink != nil {
			log := decisionLog(p.reqOf(r, host, port), egress.Deny, ruleSourceLLM)
			log.Cause = "credential refresh failed: " + p.dialFailureCause(ierr)
			p.sink.emit(log)
		}
		p.httpErrorShaped(w, p.shapeOf(r, host), host, "llm credential refresh failed", ierr, http.StatusBadGateway,
			brokeredFault{port: port, ruleSource: ruleSourceLLM, reason: reasonCredentialUnavailable})
		return
	}

	// On a confident BLOCK (or fail-closed uninspectable channel) inspectLLM
	// writes the 403 itself and returns blocked=true.
	bodyReader, scanSummary, releaseBody, blocked := p.inspectLLM(w, r, host, port, rest, channel)
	defer releaseBody()
	if blocked {
		return
	}
	// A rule that pins its credential to a method-and-path set withholds it from every other request
	// on this route too (the strip still runs: ownedHeader is hdr.name either way).
	inject, owned := &hdr, hdr.name
	if !p.inject.allowsInjection(host, r.Method, "/"+rest, r.URL.RawQuery) {
		inject = nil
	}
	if !ok { // a local model with no auth: the sandbox's own credential headers are still stripped
		inject, owned = nil, "Authorization"
	}
	p.forwardInspectedLLM(w, r, host, port, rest, target, inject, owned, ruleSourceLLM, false, bodyReader, scanSummary)
}

// llmRouteTarget resolves the brokered LLM route's dial target, choosing the
// vet by what host IS rather than which route asked.
//
// TRUST BOUNDARY: gatewayTarget/vetTrustedHost is the RELAXED vet, admitting
// RFC1918/ULA/CGNAT — safe ONLY because that host was typed into the control
// plane at boot. Applying it unconditionally would let a poisoned/split-horizon
// resolver answering RFC1918 for a public vendor host get the brokered
// credential delivered to it (DNS rebinding). A host that is no configured
// gateway's (gatewayVendorOf) takes egressTarget.
func (p *Proxy) llmRouteTarget(host string, port int) (string, error) {
	if _, isGateway := p.gatewayVendorOf(host); isGateway {
		return p.gatewayTarget(host, port)
	}
	target, _, err := p.egressTarget(host, port)
	return target, err
}

// upstreamURLHost is the host of an upstream URL: the port is left off when it
// is the scheme's default, and an IPv6 literal is bracketed either way, since a
// bare one's colons read as a port.
func upstreamURLHost(host string, port, defaultPort int) string {
	switch {
	case port != defaultPort:
		return net.JoinHostPort(host, strconv.Itoa(port))
	case strings.Contains(host, ":"):
		return "[" + host + "]"
	}
	return host
}

// forwardInspectedLLM is the shared credential-strip + forward-and-respond
// tail for a brokered/MITM'd LLM upstream, used by proxyLLMRequest and
// serveMITMRequest. Builds the upstream request over the pinned target,
// sanitizes hop-by-hop headers, applies the credential (hdr != nil: strip
// every sandbox-supplied credential header then inject; hdr == nil: preserve
// the agent's own resident credential, inspect-only), records the allow
// decision, and streams the response back.
func (p *Proxy) forwardInspectedLLM(w http.ResponseWriter, r *http.Request, host string, port int, rest, target string, hdr *injectedHeader, ownedHeader string, ruleSource string, nsOff bool, bodyReader io.Reader, scanSummary *egress.ScanSummary) {
	scheme, defaultPort := p.upstreamSchemeFor(host, port)
	hostport := upstreamURLHost(host, port, defaultPort)
	// TRUST BOUNDARY: `rest` is the PERCENT-DECODED path classifyLLM keys the
	// inspection decision on. Setting URL.Path (not concatenating into a
	// string re-parsed later) makes the request re-encode those bytes, so a
	// decoded "#"/"?" can't be reinterpreted as a fragment/query and desync
	// the scanned path from the sent path — a block-mode bypass otherwise.
	upstreamURL := &url.URL{
		Scheme:   scheme,
		Host:     hostport,
		Path:     "/" + rest,
		RawQuery: r.URL.RawQuery,
	}
	// send forwards one attempt carrying hdr and records its allow. On false it has answered.
	send := func(body io.Reader, hdr *injectedHeader) (*http.Response, bool) {
		outReq, err := http.NewRequestWithContext(context.WithValue(r.Context(), vettedIPKey{}, target), r.Method, upstreamURL.String(), body)
		if err != nil {
			p.emitLLMDecision(r, host, port, egress.Deny, ruleSource, nil)
			p.httpErrorShaped(w, p.shapeOf(r, host), host, "build llm request", err, http.StatusBadGateway,
				brokeredFault{port: port, ruleSource: ruleSource, reason: reasonUpstreamError})
			return nil, false
		}
		copyHeader(outReq.Header, r.Header)
		removeHopByHop(outReq.Header)
		applyCredential(outReq.Header, ownedHeader, hdr)
		outReq.Host = hostport
		outReq.Header.Del("Host")

		// Emitted only AFTER a successful round-trip: a failed dial must NOT
		// over-report an allow.
		resp, err := p.roundTripUpstream(outReq)
		if err != nil {
			seen := &egress.DecisionLog{Request: p.reqOf(r, host, port), Scan: scanSummary}
			if p.refuseH2Mismatch(w, p.shapeOf(r, host), err, ruleSourceUpstreamProtocolMismatch, seen, host, "llm upstream error") {
				return nil, false
			}
			if p.sink != nil {
				p.sink.emit(p.denyDialFailed("builtin:dial-failed", p.reqOf(r, host, port), host, err, scanSummary))
			}
			// No reason: classified from err, and the message ends with the decision log's Cause above.
			p.httpErrorShaped(w, p.shapeOf(r, host), host, "llm upstream error", err, http.StatusBadGateway,
				brokeredFault{port: port, ruleSource: "builtin:dial-failed"})
			return nil, false
		}
		p.emitLLMAllowWithFault(r, host, port, ruleSource, scanSummary, "/"+rest, resp)
		if nsOff { // once, however many times a credential heal sends
			p.emitLLMDecision(r, host, port, egress.Allow, ruleSourceGitNSOff, nil)
			nsOff = false
		}
		return resp, true
	}
	resp, ok := send(bodyReader, hdr)
	if !ok {
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Azure DevOps refused the injected header itself: it may be stale (the per-host cache outlived
	// it). Heal once, before anything is relayed; a second refusal takes the ordinary path below.
	if ruleSource == ruleSourceADO && hdr != nil && adoCredentialRefused(resp) {
		replay := adoReplayBody(r)
		fresh, retry, err := p.healADOHeader(r.Context(), host, *hdr, replay != nil)
		if err != nil {
			drainClose(resp)
			if r.Context().Err() == nil { // a caller that hung up was never refused
				p.refuseADOCredential(w, r, host, port, err)
			}
			return
		}
		if retry {
			drainClose(resp)
			if p.sink != nil {
				p.sink.emit(decisionLog(p.reqOf(r, host, port), egress.Allow, ruleSourceADOReresolved))
			}
			again, ok := send(replay(), &fresh)
			if !ok {
				return
			}
			resp = again
		}
	}
	if lm := p.localModelAt(host, port); lm != nil && ruleSource == ruleSourceLLM {
		if err := filterLocalInventory(resp, rest, lm); err != nil {
			p.refuseLocalModel(w, r, host, port, http.StatusBadGateway, reasonUpstreamError, err.Error())
			return
		}
	}
	p.relayUpstream(w, r, host, port, resp, ruleSource)
}

// adoReplayBody is how an Azure DevOps REST request's body is sent a second time, or nil when it
// can't be: only a request with no body, or one whose whole body the gate already buffered
// (adoPeekBody, at most adoscope.MaxBodyPeek), replays. Nothing is buffered here for a retry.
func adoReplayBody(r *http.Request) func() io.Reader {
	switch {
	case r.GetBody != nil:
		return func() io.Reader {
			b, _ := r.GetBody()
			return b
		}
	case r.Body == nil || r.Body == http.NoBody:
		return func() io.Reader { return http.NoBody }
	}
	return nil
}

// coverageInspectable / coverageOpaque describe whether the LLM transport for
// a decision could be inspected, recorded on every scan summary.
const (
	coverageInspectable = "inspectable"
	coverageOpaque      = "tunneled-opaque"
)

// scanSummaryFrom builds a CONTENT-FREE decision summary from a scan result.
// overrideAction wins; otherwise the action derives from the result. Never
// copies raw matched bytes.
func scanSummaryFrom(res contentscan.Result, serr error, eng *contentscan.Engine, overrideAction string, channel contentscan.Channel) *egress.ScanSummary {
	s := &egress.ScanSummary{
		Scanned:    res.Scanned,
		Coverage:   coverageInspectable,
		Mode:       string(eng.Mode()),
		Channel:    string(channel),
		Skipped:    res.Skipped,
		SkipReason: res.SkipReason,
		// Counts let an auditor tell a capped scan from one that found exactly maxFindings.
		FindingsCapped:  res.FindingsCapped || res.SkipReason == "findings_capped",
		FindingsPastCap: res.FindingsDropped,
	}
	if s.FindingsCapped {
		// The COUNTED pre-cap total: under mode=block, capFindings keeps a
		// block-relevant past-cap finding back INTO Findings while still
		// counting it dropped, so len(Findings)+FindingsDropped would double-count.
		s.FindingsTotal = res.FindingsSeen
	}
	for _, f := range res.Findings {
		s.Findings = append(s.Findings, egress.ScanFinding{
			Detector:  f.Detector,
			Category:  string(f.Category),
			FieldPath: f.FieldPath,
			Offset:    f.Offset,
			Length:    f.Length,
			Severity:  string(f.Severity),
			Sample:    f.Sample,
		})
	}
	switch {
	case overrideAction != "":
		s.Action = overrideAction
	case serr != nil || (res.Skipped && res.SkipReason == "parse_error"):
		s.Action = "fail"
	case (res.Skipped || res.FindingsCapped) && len(res.Findings) > 0 &&
		(res.FindingsCapped || res.SkipReason == "findings_capped" ||
			res.SkipReason == "scan_budget" || res.SkipReason == "attachment_decode_error"):
		// Checked before res.Skipped: a real secret found alongside a
		// budget/decode limit still alerts, so a SIEM rule keyed on
		// llm.scan.alert doesn't lose it. span_oversize is deliberately
		// excluded (already "skip" with findings on base).
		s.Action = "alert"
	case res.Skipped:
		s.Action = "skip"
	default:
		s.Action = "alert"
	}
	return s
}

// inspectLLM runs optional outbound content inspection for an LLM route
// request and returns the body to forward, a content-free scan summary to
// attach (nil = nothing to report), and blocked=true when it has already
// written a 403/error response. messages/count_tokens (Anthropic) and
// chat/completions (OpenAI) are scanned; other prompt-bearing subpaths are
// honestly marked uninspected; non-prompt paths stream through quietly.
//
// The returned release must be deferred by the caller — see scanBufferedBody.
func (p *Proxy) inspectLLM(w http.ResponseWriter, r *http.Request, host string, port int, rest string, channel contentscan.Channel) (io.Reader, *egress.ScanSummary, func(), bool) {
	noRelease := func() {}
	if p.scanner == nil || p.scanner.Mode() == contentscan.ModeOff {
		return r.Body, nil, noRelease, false
	}
	switch classifyLLM(channel, r.Method, rest) {
	case scanMessages:
		return p.scanBufferedBody(w, r, host, channel, "read llm body",
			func(d egress.Decision, ruleSource string, scan *egress.ScanSummary) {
				p.emitLLMDecision(r, host, port, d, ruleSource, scan)
			})
	case scanOpaque:
		// Prompt-bearing subpath we can't parse yet: emit an honest
		// uninspected-channel skip, refused under fail-closed blocking.
		if p.scanner.BlocksOnError() {
			p.emitLLMDecision(r, host, port, egress.Deny, ruleSourceLLMBlocked, p.skipSummary("block", "uninspected_channel", channel))
			writeScanBlockedShaped(w, p.shapeOf(r, host), 0, nil, "uninspected_channel")
			return nil, nil, noRelease, true
		}
		return r.Body, p.skipSummary("skip", "uninspected_channel", channel), noRelease, false
	default: // scanNone: not prompt-bearing — stream through, stay quiet
		return r.Body, nil, noRelease, false
	}
}

// bodyBearingMethod reports whether a method may carry a request body Wardyn
// would want to inspect.
//
// TRUST BOUNDARY: the ONE definition, shared by hasScannableBody and both LLM
// endpoint classifiers, so a sandbox can't bypass scanning by picking the
// verb as freely as the suffix.
func bodyBearingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return true
	default:
		return false
	}
}

// hasScannableBody reports whether a forward-proxy request carries a body
// worth inspecting (body-bearing method, non-empty/unknown-length body).
func hasScannableBody(r *http.Request) bool {
	return bodyBearingMethod(r.Method) && r.Body != nil && r.ContentLength != 0
}

// inspectForwardBody scans a GENERIC (non-LLM) plaintext-HTTP forward body:
// buffer, block before forwarding on a confident finding, else return the
// buffered body + a content-free summary. blocked=true means a 403/error was
// already written.
func (p *Proxy) inspectForwardBody(w http.ResponseWriter, r *http.Request, host string, port int) (io.Reader, *egress.ScanSummary, func(), bool) {
	return p.scanBufferedBody(w, r, host, contentscan.ChannelGeneric, "read body",
		func(d egress.Decision, ruleSource string, scan *egress.ScanSummary) {
			p.emitLLMDecision(r, host, port, d, ruleSource, scan)
		})
}

// scanBufferedBody is the shared buffer→oversize→scan→block→summarize core
// for both the LLM scanMessages path and the generic forward path. Buffers up
// to maxLLMScanBody (fail-closed on oversize only when
// block+on_scanner_error=block, else forwarding the FULL untruncated body
// with an honest body_oversize skip), scans for the channel, and either
// writes the 403 (blocked=true) or returns the buffered body plus a
// content-free summary.
//
// The returned release MUST be deferred by the caller: gives the buffer back
// to maxRetainedScanBytes. Always non-nil and safe to call more than once.
func (p *Proxy) scanBufferedBody(w http.ResponseWriter, r *http.Request, host string, channel contentscan.Channel, readErrMsg string, emit func(egress.Decision, string, *egress.ScanSummary)) (io.Reader, *egress.ScanSummary, func(), bool) {
	shape := p.shapeOf(r, host)
	noRelease := func() {}
	// Read before the scan slot, charged to the engine's retained budget for
	// the buffer's whole life (scanBudget.readBody), so a slow body holds only
	// the bytes it has sent. A budget wait that expires fails CLOSED. A body a
	// door already holds charged (holdChargedBody) is scanned as it is.
	unread := r.Body
	buffered, held := r.Context().Value(chargedBodyKey{}).([]byte)
	release, rerr := noRelease, error(nil)
	if held {
		unread = http.NoBody
	} else {
		buffered, release, rerr = p.scan.readBody(r.Context(), r.Body, r.ContentLength, int64(maxLLMScanBody))
	}
	if rerr != nil {
		emit(egress.Deny, ruleSourceLLM, nil)
		status := http.StatusBadRequest
		if errors.Is(rerr, errScanBusy) {
			status = http.StatusBadGateway
		}
		p.scanFault(w, shape, readErrMsg, rerr, status)
		return nil, nil, noRelease, true
	}
	if len(buffered) > maxLLMScanBody {
		if p.scanner.BlocksOnError() {
			release()
			emit(egress.Deny, ruleSourceLLMBlocked, p.skipSummary("block", "body_oversize", channel))
			writeScanBlockedShaped(w, shape, 0, nil, "body_oversize")
			return nil, nil, noRelease, true
		}
		return io.MultiReader(bytes.NewReader(buffered), unread),
			p.skipSummary("skip", "body_oversize", channel), release, false
	}
	// The scan slot (maxConcurrentScans) bounds extraction's live heap against
	// the sidecar's cgroup cap. Waited through the request ctx AND a wall-clock
	// bound (scanQueueWait); fails CLOSED on either, since an uninspectable
	// request must not be forwarded.
	free, err := p.scan.takeSlot(r.Context())
	if err != nil {
		release()
		emit(egress.Deny, ruleSourceLLM, nil)
		p.scanFault(w, shape, readErrMsg, err, http.StatusBadGateway)
		return nil, nil, noRelease, true
	}
	res, _, serr := p.scanner.ScanRequestContext(r.Context(), channel, buffered)
	free()
	if p.scanner.ShouldBlock(res) {
		release()
		emit(egress.Deny, ruleSourceLLMBlocked, scanSummaryFrom(res, serr, p.scanner, "block", channel))
		writeScanBlockedShaped(w, shape, len(res.Findings), categoriesOf(res.Findings), res.SkipReason)
		return nil, nil, noRelease, true
	}
	var sum *egress.ScanSummary
	if len(res.Findings) > 0 || res.Skipped || serr != nil {
		sum = scanSummaryFrom(res, serr, p.scanner, "", channel)
	}
	return bytes.NewReader(buffered), sum, release, false
}

// skipSummary builds a content-free "scanning did not run" summary
// (oversize/uninspected channel) for the inspectable transport.
func (p *Proxy) skipSummary(action, reason string, channel contentscan.Channel) *egress.ScanSummary {
	return &egress.ScanSummary{
		Scanned:    false,
		Coverage:   coverageInspectable,
		Mode:       string(p.scanner.Mode()),
		Action:     action,
		Skipped:    true,
		SkipReason: reason,
		Channel:    string(channel),
	}
}

// categoriesOf returns the sorted, de-duplicated finding categories.
func categoriesOf(findings []contentscan.Finding) []string {
	set := map[string]struct{}{}
	for _, f := range findings {
		set[string(f.Category)] = struct{}{}
	}
	return slices.Sorted(maps.Keys(set))
}

// scanFault answers a request scanBufferedBody could not take in, so did not
// forward: the scan queue or the retain budget expired (502, a load condition
// worth a retry), or the body could not be read (400). shape is shapeOf(r,
// host): a provider or AWS error body, or the plain lane's text.
func (p *Proxy) scanFault(w http.ResponseWriter, shape errShape, readErrMsg string, err error, status int) {
	switch {
	case shape != shapePlain:
		writeProviderError(w, shape, status, reasonNotInspected, "wardyn: "+reasonNotInspected+": "+readErrMsg+": "+p.dialFailureCause(err), nil)
	case status == http.StatusBadRequest:
		http.Error(w, readErrMsg, status)
	default:
		p.httpError(w, readErrMsg, err, status)
	}
}

// writeScanBlocked returns the 403 the sandbox client sees when a request is
// refused by content inspection. TRUST BOUNDARY: the body carries counts +
// category enums + a reason only, never the matched content.
func writeScanBlocked(w http.ResponseWriter, findings int, categories []string, reason string) {
	writeScanBlockedShaped(w, shapePlain, findings, categories, reason)
}

// writeScanBlockedShaped is writeScanBlocked in the LLM lane's provider error
// shape, the {"wardyn":...} fields riding along as extras.
func writeScanBlockedShaped(w http.ResponseWriter, shape errShape, findings int, categories []string, reason string) {
	type scanBlock struct {
		Wardyn     string   `json:"wardyn"`
		Findings   int      `json:"findings"`
		Categories []string `json:"categories,omitempty"`
		Reason     string   `json:"reason,omitempty"`
	}
	b := scanBlock{Wardyn: "llm_content_blocked", Findings: findings, Categories: categories, Reason: reason}
	if shape != shapePlain {
		msg := "wardyn: " + reasonContentBlocked + ": the request was refused by content inspection"
		if reason != "" {
			msg += " (" + reason + ")"
		}
		extra := map[string]any{"wardyn": b.Wardyn, "findings": b.Findings}
		if len(categories) > 0 {
			extra["categories"] = categories
		}
		if reason != "" {
			extra["reason"] = reason
		}
		writeProviderError(w, shape, http.StatusForbidden, reasonContentBlocked, msg, extra)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(b)
}

// emitLLMDecision emits a decision log for an LLM or MITM route carrying a
// content-free scan summary. port is the REAL destination port — a gateway
// or MITM'd mirror must not be recorded as 443.
func (p *Proxy) emitLLMDecision(r *http.Request, host string, port int, decision egress.Decision, ruleSource string, scan *egress.ScanSummary) {
	if p.sink == nil {
		return
	}
	log := decisionLog(p.reqOf(r, host, port), decision, ruleSource)
	log.Scan = scan
	p.sink.emit(log)
}

// emitLLMBlindOnce emits a single llm.scan.bypass signal per LLM host: an
// opaque tunnel (or plain-HTTP forward) inspection couldn't see into, keyed on
// host and port, with the real port and method on the row. The request is allowed
// separately; this is purely the honest coverage signal.
func (p *Proxy) emitLLMBlindOnce(host string, port int, method string) {
	if p.sink == nil {
		return
	}
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	key := net.JoinHostPort(h, strconv.Itoa(port))
	p.blindMu.Lock()
	if p.blindHosts == nil {
		p.blindHosts = make(map[string]struct{})
	}
	if _, seen := p.blindHosts[key]; seen {
		p.blindMu.Unlock()
		return
	}
	// Bound the dedup map against hostname enumeration; past the cap this
	// SUPPRESSES the coverage signal (doesn't make the tunnel inspectable),
	// counted via decisionSink.dropped plus an operator log line.
	if len(p.blindHosts) >= maxBlindHosts {
		p.blindMu.Unlock()
		p.sink.dropped.Add(1)
		slog.Warn("llm.scan.bypass coverage suppressed: per-run blind-host cap reached",
			"host", key, "cap", maxBlindHosts, "rule_source", ruleSourceLLMBlind)
		return
	}
	p.blindHosts[key] = struct{}{}
	p.blindMu.Unlock()

	p.sink.emit(egress.DecisionLog{
		Request: egress.Request{
			RunID:  p.runID,
			Host:   h,
			Port:   port,
			Method: method,
			Time:   p.now(),
		},
		Decision:   egress.Allow,
		RuleSource: ruleSourceLLMBlind,
		Scan: &egress.ScanSummary{
			Scanned:  false,
			Coverage: coverageOpaque,
			Mode:     string(p.scanner.Mode()),
			Action:   "bypass",
		},
	})
}
