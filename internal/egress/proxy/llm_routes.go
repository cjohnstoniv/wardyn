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
	p.proxyLLMRequest(w, r, host, port, joinLLMPath(prefix, rest), contentscan.ChannelAnthropicMessages)
}

// handleLLMOpenAI proxies /wardyn/llm/openai/<rest> to
// https://api.openai.com/<rest> (or a configured internal gateway) with the
// brokered OpenAI credential (the Codex reverse-proxy route).
func (p *Proxy) handleLLMOpenAI(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, llmOpenAIPrefix)
	host, port, prefix := p.llmUpstream(openaiHost)
	p.proxyLLMRequest(w, r, host, port, joinLLMPath(prefix, rest), contentscan.ChannelOpenAIChat)
}

// joinLLMPath prepends an internal gateway's path prefix (from its base URL)
// onto rest. Empty prefix (unset-gateway default) returns rest unchanged.
func joinLLMPath(prefix, rest string) string {
	if prefix == "" {
		return rest
	}
	return strings.TrimPrefix(prefix, "/") + "/" + rest
}

// DRAFT (M2 canon pending)
const (
	// llmNoCredentialDetail is the brokered-LLM 404's detail when the control
	// plane composed none.
	//
	// DRAFT (M2 canon pending)
	llmNoCredentialDetail = "no credential is configured for this brokered LLM route"

	// llmBelowPolicyClause is appended to EVERY brokered-LLM 404 detail: a
	// missing model credential is below policy, not something to retry or approve.
	//
	// DRAFT (M2 canon pending)
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
	hdr, ok := p.inject.headerFor(host)
	if !ok {
		p.emitLLMDecision(r, host, port, egress.Deny, ruleSourceLLM, nil)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		body, _ := json.Marshal(map[string]string{
			"wardyn": "no_llm_credential", "detail": llm404Detail(p.llmUnavailableDetail),
		})
		_, _ = w.Write(body)
		return
	}

	// A CONTROL-PLANE-authored gateway gets gatewayTarget's relaxed vet; every
	// other host gets egressTarget's SSRF-guarded vet.
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
		// AWS lane: an AWS SDK hands plain text straight to a JSON parser and crashes on it.
		p.httpErrorAWSAware(w, host, "llm upstream vet failed", err, false, http.StatusInternalServerError, "InternalServerException")
		return
	}

	// On a confident BLOCK (or fail-closed uninspectable channel) inspectLLM
	// writes the 403 itself and returns blocked=true.
	bodyReader, scanSummary, releaseBody, blocked := p.inspectLLM(w, r, host, port, rest, channel)
	defer releaseBody()
	if blocked {
		return
	}
	p.forwardInspectedLLM(w, r, host, port, rest, target, &hdr, hdr.name, ruleSourceLLM, bodyReader, scanSummary)
}

// llmRouteTarget resolves the brokered LLM route's dial target, choosing the
// vet by what host IS rather than which route asked.
//
// TRUST BOUNDARY: gatewayTarget/vetTrustedHost is the RELAXED vet, admitting
// RFC1918/ULA/CGNAT — safe ONLY because that host was typed into the control
// plane at boot. Applying it unconditionally would let a poisoned/split-horizon
// resolver answering RFC1918 for a public vendor host get the brokered
// credential delivered to it (DNS rebinding). A host not in p.gatewayVendor
// takes egressTarget.
func (p *Proxy) llmRouteTarget(host string, port int) (string, error) {
	if _, isGateway := p.gatewayVendor[strings.TrimSuffix(strings.ToLower(host), ".")]; isGateway {
		return p.gatewayTarget(host, port)
	}
	target, _, err := p.egressTarget(host, port)
	return target, err
}

// forwardInspectedLLM is the shared credential-strip + forward-and-respond
// tail for a brokered/MITM'd LLM upstream, used by proxyLLMRequest and
// serveMITMRequest. Builds the upstream request over the pinned target,
// sanitizes hop-by-hop headers, applies the credential (hdr != nil: strip
// every sandbox-supplied credential header then inject; hdr == nil: preserve
// the agent's own resident credential, inspect-only), records the allow
// decision, and streams the response back.
func (p *Proxy) forwardInspectedLLM(w http.ResponseWriter, r *http.Request, host string, port int, rest, target string, hdr *injectedHeader, ownedHeader string, ruleSource string, bodyReader io.Reader, scanSummary *egress.ScanSummary) {
	scheme, defaultPort := p.upstreamSchemeFor(host, port)
	hostport := host
	if port != defaultPort {
		hostport = net.JoinHostPort(host, strconv.Itoa(port))
	}
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
			p.httpError(w, "build llm request", err, http.StatusBadGateway)
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
			if p.refuseH2Mismatch(w, err, ruleSourceUpstreamProtocolMismatch, seen, host, "llm upstream error") {
				return nil, false
			}
			if p.sink != nil {
				p.sink.emit(p.denyDialFailed("builtin:dial-failed", p.reqOf(r, host, port), host, err, scanSummary))
			}
			// withStage=true: shares its Cause with the decision log above verbatim.
			p.httpErrorAWSAware(w, host, "llm upstream error", err, true, http.StatusInternalServerError, "InternalServerException")
			return nil, false
		}
		p.emitLLMAllowWithFault(r, host, port, ruleSource, scanSummary, "/"+rest, resp)
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
		return p.scanBufferedBody(w, r, channel, "read llm body",
			func(d egress.Decision, ruleSource string, scan *egress.ScanSummary) {
				p.emitLLMDecision(r, host, port, d, ruleSource, scan)
			})
	case scanOpaque:
		// Prompt-bearing subpath we can't parse yet: emit an honest
		// uninspected-channel skip, refused under fail-closed blocking.
		if p.scanner.BlocksOnError() {
			p.emitLLMDecision(r, host, port, egress.Deny, ruleSourceLLMBlocked, p.skipSummary("block", "uninspected_channel", channel))
			writeScanBlocked(w, 0, nil, "uninspected_channel")
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
	return p.scanBufferedBody(w, r, contentscan.ChannelGeneric, "read body",
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
func (p *Proxy) scanBufferedBody(w http.ResponseWriter, r *http.Request, channel contentscan.Channel, readErrMsg string, emit func(egress.Decision, string, *egress.ScanSummary)) (io.Reader, *egress.ScanSummary, func(), bool) {
	// Scan slot (maxConcurrentScans) bounds live heap against the sidecar's
	// cgroup cap. Waited through the request ctx AND a wall-clock bound
	// (scanQueueWait); fails CLOSED on either, since an uninspectable request
	// must not be forwarded.
	noRelease := func() {}
	ctx, cancel := context.WithTimeout(r.Context(), scanQueueWait)
	defer cancel()
	select {
	case scanSlots <- struct{}{}:
		defer func() { <-scanSlots }()
	case <-ctx.Done():
		emit(egress.Deny, ruleSourceLLM, nil)
		p.httpError(w, readErrMsg, ctx.Err(), http.StatusBadGateway)
		return nil, nil, noRelease, true
	}

	buffered, rerr := io.ReadAll(io.LimitReader(r.Body, int64(maxLLMScanBody)+1))
	if rerr != nil {
		emit(egress.Deny, ruleSourceLLM, nil)
		http.Error(w, readErrMsg, http.StatusBadRequest)
		return nil, nil, noRelease, true
	}
	// Charge the buffer to the LIFETIME budget after the read/scan peak, still
	// under the slot; expiry fails CLOSED like the slot wait.
	charge := func() (func(), bool) {
		release, ok := retainScanBuffer(ctx, len(buffered))
		if !ok {
			emit(egress.Deny, ruleSourceLLM, nil)
			p.httpError(w, readErrMsg, ctx.Err(), http.StatusBadGateway)
			return noRelease, false
		}
		return release, true
	}
	if len(buffered) > maxLLMScanBody {
		if p.scanner.BlocksOnError() {
			emit(egress.Deny, ruleSourceLLMBlocked, p.skipSummary("block", "body_oversize", channel))
			writeScanBlocked(w, 0, nil, "body_oversize")
			return nil, nil, noRelease, true
		}
		release, ok := charge()
		if !ok {
			return nil, nil, noRelease, true
		}
		return io.MultiReader(bytes.NewReader(buffered), r.Body),
			p.skipSummary("skip", "body_oversize", channel), release, false
	}
	res, _, serr := p.scanner.ScanRequest(channel, buffered)
	if p.scanner.ShouldBlock(res) {
		emit(egress.Deny, ruleSourceLLMBlocked, scanSummaryFrom(res, serr, p.scanner, "block", channel))
		writeScanBlocked(w, len(res.Findings), categoriesOf(res.Findings), res.SkipReason)
		return nil, nil, noRelease, true
	}
	var sum *egress.ScanSummary
	if len(res.Findings) > 0 || res.Skipped || serr != nil {
		sum = scanSummaryFrom(res, serr, p.scanner, "", channel)
	}
	release, ok := charge()
	if !ok {
		return nil, nil, noRelease, true
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

// writeScanBlocked returns the 403 the sandbox client sees when a request is
// refused by content inspection. TRUST BOUNDARY: the body carries counts +
// category enums + a reason only, never the matched content.
func writeScanBlocked(w http.ResponseWriter, findings int, categories []string, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(struct {
		Wardyn     string   `json:"wardyn"`
		Findings   int      `json:"findings"`
		Categories []string `json:"categories,omitempty"`
		Reason     string   `json:"reason,omitempty"`
	}{Wardyn: "llm_content_blocked", Findings: findings, Categories: categories, Reason: reason})
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
// opaque CONNECT tunnel inspection couldn't see into. The CONNECT is allowed
// separately; this is purely the honest coverage signal.
func (p *Proxy) emitLLMBlindOnce(host string) {
	if p.sink == nil {
		return
	}
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	p.blindMu.Lock()
	if p.blindHosts == nil {
		p.blindHosts = make(map[string]struct{})
	}
	if _, seen := p.blindHosts[h]; seen {
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
			"host", h, "cap", maxBlindHosts, "rule_source", ruleSourceLLMBlind)
		return
	}
	p.blindHosts[h] = struct{}{}
	p.blindMu.Unlock()

	p.sink.emit(egress.DecisionLog{
		Request: egress.Request{
			RunID:  p.runID,
			Host:   h,
			Port:   443,
			Method: http.MethodConnect,
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
