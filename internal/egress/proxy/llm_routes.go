// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The brokered LLM routes (/wardyn/llm/anthropic/, /wardyn/llm/openai/):
// reverse-proxying to the api-key vendor host (or an operator-configured
// internal gateway — see llmUpstream/vetTrustedHost in proxy.go/config.go),
// credential injection, outbound content inspection, and the request/response
// classifiers that decide what gets scanned. Split out of local_routes.go at
// the 1000-line gate — a real seam (this is the ONE path a configured
// WARDYN_ANTHROPIC_BASE_URL/WARDYN_OPENAI_BASE_URL changes), not a size dodge.

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
	// llmAnthropicPrefix selects the Anthropic LLM passthrough. The remainder
	// after this prefix is appended to the Anthropic API base.
	llmAnthropicPrefix = "/wardyn/llm/anthropic/"
	anthropicHost      = "api.anthropic.com"
	// llmOpenAIPrefix selects the OpenAI reverse-proxy passthrough (Codex).
	llmOpenAIPrefix = "/wardyn/llm/openai/"
	openaiHost      = "api.openai.com"
	// maxBlindHosts bounds the per-run opaque-tunnel dedup set (emitLLMBlindOnce).
	maxBlindHosts = 64
	ruleSourceLLM = "brokered:llm"
	// ruleSourceLLMBlocked marks an LLM request refused by content inspection;
	// ruleSourceLLMBlind marks an opaque CONNECT to an LLM host that inspection
	// could not see into (honest coverage signal).
	ruleSourceLLMBlocked = "scan:blocked"
	ruleSourceLLMBlind   = "scan:opaque-tunnel"
	// ruleSourceLLMMITM marks a request inspected via TLS-MITM interception of an
	// otherwise-opaque CONNECT tunnel (the subscription-OAuth path).
	ruleSourceLLMMITM = "scan:mitm"
)

// handleLLMAnthropic proxies /wardyn/llm/anthropic/<rest> to
// https://api.anthropic.com/<rest> — or an operator-configured internal
// gateway (Config.LLMUpstreams) — with the brokered Anthropic credential.
func (p *Proxy) handleLLMAnthropic(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, llmAnthropicPrefix)
	host, port, prefix := p.llmUpstream(anthropicHost)
	p.proxyLLMRequest(w, r, host, port, joinLLMPath(prefix, rest), contentscan.ChannelAnthropicMessages)
}

// handleLLMOpenAI proxies /wardyn/llm/openai/<rest> to https://api.openai.com/<rest>
// — or an operator-configured internal gateway — with the brokered OpenAI
// credential (the Codex reverse-proxy route).
func (p *Proxy) handleLLMOpenAI(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, llmOpenAIPrefix)
	host, port, prefix := p.llmUpstream(openaiHost)
	p.proxyLLMRequest(w, r, host, port, joinLLMPath(prefix, rest), contentscan.ChannelOpenAIChat)
}

// joinLLMPath prepends an internal gateway's path prefix (from its base URL)
// onto rest. Empty prefix (the unset-gateway default) returns rest unchanged —
// the byte-identical-to-today case.
func joinLLMPath(prefix, rest string) string {
	if prefix == "" {
		return rest
	}
	return strings.TrimPrefix(prefix, "/") + "/" + rest
}

// DRAFT (M2 canon pending)
const (
	// llmNoCredentialDetail is the brokered-LLM 404's detail when the control
	// plane composed none: it says what the route IS, replacing "no LLM
	// credential is brokered for <host>", which named a host the reader could
	// do nothing with.
	//
	// DRAFT (M2 canon pending)
	llmNoCredentialDetail = "no credential is configured for this brokered LLM route"

	// llmBelowPolicyClause is appended to EVERY brokered-LLM 404 detail. A
	// missing model credential is not a first-use approval and not a policy
	// tightening a person can widen — an agent that retries, or a member
	// looking for an Approve button, burns time on a door that doesn't exist.
	//
	// DRAFT (M2 canon pending)
	llmBelowPolicyClause = "this is below policy: it cannot be approved, and no policy edit changes it."
)

// llm404Detail is the brokered-LLM 404's self-explaining detail: what the
// control plane knows about this deployment's model posture
// (Config.LLMUnavailableDetail), else the generic route sentence — and the
// below-policy clause either way.
func llm404Detail(configured string) string {
	detail := strings.TrimSpace(configured)
	if detail == "" {
		detail = llmNoCredentialDetail
	}
	return detail + " — " + llmBelowPolicyClause
}

// proxyLLMRequest is the shared reverse-proxy + inspection path for a brokered
// LLM upstream. It applies the startup-minted credential, strips every sandbox-
// supplied credential header, optionally inspects the body (blocking BEFORE the
// allow decision is recorded), and forwards to host:port/<rest> over the vetted
// IP. Shared by both the /wardyn/llm/* local routes and the TLS-MITM CONNECT
// interception (serveMITM), so host/port/rest are passed explicitly.
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

	// llmRouteTarget picks the resolver by what the host actually IS: a
	// CONTROL-PLANE-authored gateway gets gatewayTarget's relaxed per-request
	// vet; every other host — including the PUBLIC vendor host with no
	// gateway configured — gets egressTarget's SSRF-guarded vet, as on every
	// other forward-egress path.
	target, err := p.llmRouteTarget(host, port)
	if err != nil {
		// A refused/unreachable configured gateway is vetTrustedHost's OWN
		// GUARD refusal, not an SSRF-shaped denial and not a lost dial —
		// distinguished in the decision log so it reads as "the gateway is
		// misconfigured", not a network fault the operator can't fix by
		// editing this gateway's own config.
		source := ruleSourceLLM
		if errors.Is(err, errGatewayVet) {
			source = ruleSourceGatewayVetFailed
		}
		p.emitLLMDecision(r, host, port, egress.Deny, source, nil)
		// AWS lane: a modelled, valid-JSON error body — an AWS SDK hands plain
		// text straight to a JSON parser and crashes on it.
		p.httpErrorAWSAware(w, host, "llm upstream vet failed", err, false, http.StatusInternalServerError, "InternalServerException")
		return
	}

	// OPTIONAL outbound content inspection (see inspectLLM). On a confident BLOCK
	// (or a fail-closed uninspectable channel) it writes the 403 itself and
	// returns blocked=true. Otherwise it returns the body to forward and a
	// content-free scan summary to attach (non-nil only when there is something
	// to report — a clean turn stays quiet).
	bodyReader, scanSummary, releaseBody, blocked := p.inspectLLM(w, r, host, port, rest, channel)
	// The buffered body stays charged to maxRetainedScanBytes until the upstream
	// round trip has consumed it.
	defer releaseBody()
	if blocked {
		return
	}
	// The brokered credential is guaranteed present here (headerFor ok above), so
	// the sandbox credential is always stripped and the brokered one injected.
	p.forwardInspectedLLM(w, r, host, port, rest, target, &hdr, hdr.name, ruleSourceLLM, bodyReader, scanSummary)
}

// llmRouteTarget resolves the brokered LLM route's dial target, choosing the
// vet by what host IS rather than by which route asked.
//
// Trust boundary: gatewayTarget/vetTrustedHost is the RELAXED vet, admitting
// RFC1918/ULA/CGNAT because a configured internal gateway is expected to live
// there — safe ONLY because that host was typed into the control plane at
// boot. Applying it unconditionally would extend the relaxation to
// api.anthropic.com/api.openai.com on a run with NO gateway configured: a
// poisoned/split-horizon resolver answering RFC1918 for the public vendor
// host would get the startup-minted brokered credential delivered to it — the
// DNS-rebinding case the unconditional private-IP guard exists to close. A
// host not in p.gatewayVendor takes egressTarget.
func (p *Proxy) llmRouteTarget(host string, port int) (string, error) {
	if _, isGateway := p.gatewayVendor[strings.TrimSuffix(strings.ToLower(host), ".")]; isGateway {
		return p.gatewayTarget(host, port)
	}
	target, _, err := p.egressTarget(host, port)
	return target, err
}

// forwardInspectedLLM is the shared credential-strip + forward-and-respond tail
// for a brokered/MITM'd LLM upstream, used by both proxyLLMRequest and
// serveMITMRequest. It builds the upstream request to host[:port]/<rest> over
// the pinned target (port omitted from the URL at 443 — the default port and
// the byte-identical-to-today shape when no gateway is configured), sanitizes
// hop-by-hop headers, applies the credential (hdr != nil: strip EVERY
// sandbox-supplied credential header, so a sandbox x-api-key cannot substitute
// the brokered credential when the rule injects under a different header,
// then inject; hdr == nil: preserve the agent's own resident credential,
// inspect-only), records the allow decision (scanSummary may be nil = quiet),
// and streams the response back. ruleSource is the decision-log source.
func (p *Proxy) forwardInspectedLLM(w http.ResponseWriter, r *http.Request, host string, port int, rest, target string, hdr *injectedHeader, ownedHeader string, ruleSource string, bodyReader io.Reader, scanSummary *egress.ScanSummary) {
	scheme, defaultPort := p.upstreamSchemeFor(host, port)
	hostport := host
	if port != defaultPort {
		hostport = net.JoinHostPort(host, strconv.Itoa(port))
	}
	// Build the upstream target as a STRUCTURED url.URL, never by concatenating
	// `rest` into a string that http.NewRequestWithContext then re-PARSES.
	//
	// Trust boundary: `rest` is the PERCENT-DECODED path (r.URL.Path), the
	// same value classifyLLM keys the inspection decision on. Fed back through
	// a URL parser, a decoded "#" becomes a FRAGMENT and a decoded "?" a QUERY,
	// so a path like ".../v1/messages%23z" would classify as
	// "v1/messages#z" (unscanned) while the wire carried "/v1/messages" — the
	// scanner and upstream disagreeing about where the path ends, a block-mode
	// bypass. Setting URL.Path (already-decoded) makes the request re-encode
	// those bytes, so the path the classifier judged is the path that is sent.
	upstreamURL := &url.URL{
		Scheme:   scheme,
		Host:     hostport,
		Path:     "/" + rest,
		RawQuery: r.URL.RawQuery,
	}
	outReq, err := http.NewRequestWithContext(context.WithValue(r.Context(), vettedIPKey{}, target), r.Method, upstreamURL.String(), bodyReader)
	if err != nil {
		p.emitLLMDecision(r, host, port, egress.Deny, ruleSource, nil)
		p.httpError(w, "build llm request", err, http.StatusBadGateway)
		return
	}
	copyHeader(outReq.Header, r.Header)
	removeHopByHop(outReq.Header)
	applyCredential(outReq.Header, ownedHeader, hdr)
	outReq.Host = hostport
	outReq.Header.Del("Host")

	// The allow decision is emitted only AFTER a successful round-trip: a
	// failed upstream dial must NOT over-report an allow.
	resp, err := p.roundTripUpstream(outReq)
	if err != nil {
		seen := &egress.DecisionLog{Request: p.reqOf(r, host, port), Scan: scanSummary}
		if p.refuseH2Mismatch(w, err, ruleSourceUpstreamProtocolMismatch, seen, host, "llm upstream error") {
			return
		}
		if p.sink != nil {
			p.sink.emit(p.denyDialFailed("builtin:dial-failed", p.reqOf(r, host, port), host, err, scanSummary))
		}
		// AWS lane: withStage=true — the one site that shares its Cause with
		// the decision log above, verbatim (both a genuine dial failure).
		p.httpErrorAWSAware(w, host, "llm upstream error", err, true, http.StatusInternalServerError, "InternalServerException")
		return
	}
	p.emitLLMAllowWithFault(r, host, port, ruleSource, scanSummary, "/"+rest, resp)
	defer func() { _ = resp.Body.Close() }()

	p.relayUpstream(w, r, host, port, resp, ruleSource)
}

// coverageInspectable / coverageOpaque describe whether the LLM transport for a
// decision could be inspected. Recorded on every scan summary so audit reports
// per-mode coverage honestly.
const (
	coverageInspectable = "inspectable"
	coverageOpaque      = "tunneled-opaque"
)

// scanSummaryFrom builds a CONTENT-FREE decision summary from a scan result.
// overrideAction (e.g. "block") wins; otherwise the action is derived from the
// result (error > skip-with-findings > skip > alert) — a scan that hit
// findings_capped, scan_budget, or attachment_decode_error but still produced
// findings alerts (see below), not "skip". It never copies raw matched bytes.
func scanSummaryFrom(res contentscan.Result, serr error, eng *contentscan.Engine, overrideAction string, channel contentscan.Channel) *egress.ScanSummary {
	s := &egress.ScanSummary{
		Scanned:    res.Scanned,
		Coverage:   coverageInspectable,
		Mode:       string(eng.Mode()),
		Channel:    string(channel),
		Skipped:    res.Skipped,
		SkipReason: res.SkipReason,
		// The truncation, on the wire: the flag survives an earlier skip
		// reason claiming SkipReason, and the counts let an auditor tell a
		// capped scan from one that found exactly maxFindings.
		FindingsCapped:  res.FindingsCapped || res.SkipReason == "findings_capped",
		FindingsPastCap: res.FindingsDropped,
	}
	if s.FindingsCapped {
		// The COUNTED pre-cap total, not len(Findings)+FindingsDropped: under
		// mode=block capFindings keeps a block-relevant past-cap finding back
		// INTO Findings while still counting it dropped, so that sum
		// double-counts every keep-back. See contentscan.Result.FindingsSeen.
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
		// This checks findings BEFORE `res.Skipped` and resolves to "alert"
		// whenever a real secret is found alongside a budget/decode limit —
		// resolving Skipped first would flip Action from "alert" to "skip"
		// exactly when a real secret was also found (Action is the literal
		// audit-action suffix; a SIEM rule keyed on llm.scan.alert would lose
		// it). A skipped scan that still carries findings is the loudest scan
		// there is; the truncation/decode-limit rides on Skipped/SkipReason,
		// not Action. span_oversize is deliberately excluded: it already read
		// "skip" with findings on base, so including it would be a behaviour
		// change outside this lane's scope.
		s.Action = "alert"
	case res.Skipped:
		s.Action = "skip"
	default:
		s.Action = "alert"
	}
	return s
}

// inspectLLM runs optional outbound content inspection for an LLM route request
// (Anthropic or OpenAI) and returns the body to forward, a content-free scan
// summary to attach (nil = nothing to report), and blocked=true when it has
// already written a 403/error response (caller must return). The single
// decision point for per-endpoint coverage: messages/count_tokens (Anthropic)
// and chat/completions (OpenAI) are scanned; other prompt-bearing subpaths are
// honestly marked uninspected (never silently allowed); non-prompt paths
// stream through quietly. host names the upstream for honest logging.
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
		// Prompt-bearing subpath we cannot parse yet (e.g. /v1/messages/batches,
		// OpenAI /v1/responses): emit an HONEST uninspected-channel skip rather
		// than a silent allow, and refuse it under fail-closed blocking.
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
// Trust boundary: the ONE definition, shared by hasScannableBody and both LLM
// endpoint classifiers, so they can never disagree — a second definition
// would let the sandbox pick the verb as freely as it picks the suffix (e.g.
// `PUT /v1/messages` with a secret reaching the vendor under mode=block,
// allowed, audit-indistinguishable from a bodiless GET). Closing the suffix
// axis while leaving the verb axis open would just move the same bypass one
// keystroke sideways.
func bodyBearingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return true
	default:
		return false
	}
}

// hasScannableBody reports whether a forward-proxy request carries a body worth
// inspecting (a body-bearing method with a non-empty/unknown-length body).
func hasScannableBody(r *http.Request) bool {
	return bodyBearingMethod(r.Method) && r.Body != nil && r.ContentLength != 0
}

// inspectForwardBody scans a GENERIC (non-LLM) plaintext-HTTP forward body. Like
// the LLM scanMessages path: buffer (cap), block before forwarding on a confident
// finding, else return the buffered body + a content-free summary. blocked=true
// means a 403/error was already written.
func (p *Proxy) inspectForwardBody(w http.ResponseWriter, r *http.Request, host string, port int) (io.Reader, *egress.ScanSummary, func(), bool) {
	return p.scanBufferedBody(w, r, contentscan.ChannelGeneric, "read body",
		func(d egress.Decision, ruleSource string, scan *egress.ScanSummary) {
			p.emitLLMDecision(r, host, port, d, ruleSource, scan)
		})
}

// scanBufferedBody is the shared buffer→oversize→scan→block→summarize core for
// both the LLM scanMessages path and the generic forward path. It buffers up to
// maxLLMScanBody (fail-closed on oversize only when block+on_scanner_error=block,
// else forwarding the FULL untruncated body with an honest body_oversize skip),
// scans for the channel, and either writes the 403 (blocked=true) or returns the
// buffered body plus a content-free summary. emit records the decision for the
// caller's transport (port 443 LLM route vs the generic forward port).
//
// The returned release MUST be deferred by the caller: it gives the buffer
// back to maxRetainedScanBytes, and the buffer stays charged until it runs
// (the scan slot only ever bounded the extract window). Always non-nil and
// safe to call more than once.
func (p *Proxy) scanBufferedBody(w http.ResponseWriter, r *http.Request, channel contentscan.Channel, readErrMsg string, emit func(egress.Decision, string, *egress.ScanSummary)) (io.Reader, *egress.ScanSummary, func(), bool) {
	// Take a scan slot BEFORE buffering: it bounds live heap against the
	// sidecar's cgroup cap, so it must be held across the io.ReadAll as well
	// as the extract+scan (see maxConcurrentScans).
	//
	// The wait is taken through the request context AND a wall-clock bound
	// (scanQueueWait) since nothing above this call deadlines it, and it fails
	// CLOSED on either: a request that could not be inspected is denied, never
	// forwarded unscanned.
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
	// Charge the buffer to the LIFETIME budget before handing it back, after
	// the read and scan peak (the size is only known now), still under the
	// slot so there is exactly one acquirer. Expiry fails CLOSED — the same
	// Deny + 502 the slot wait takes.
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

// skipSummary builds a content-free "scanning did not run" summary (oversize /
// uninspected channel) for the inspectable transport.
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
// refused by content inspection. The body carries COUNTS + category enums + a
// reason ONLY — never the matched content — mirroring writeApprovalPending.
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
// content-free scan summary. host is the upstream model host; port is the REAL
// destination port — a gateway on :8443 or an artifact mirror MITM'd on its
// configured port must not be recorded as 443 (a wrong `port` detail field is
// a dishonest audit row).
func (p *Proxy) emitLLMDecision(r *http.Request, host string, port int, decision egress.Decision, ruleSource string, scan *egress.ScanSummary) {
	if p.sink == nil {
		return
	}
	log := decisionLog(p.reqOf(r, host, port), decision, ruleSource)
	log.Scan = scan
	p.sink.emit(log)
}

// emitLLMBlindOnce emits a single llm.scan.bypass signal per LLM host: an
// inspection-enabled run reached host over an opaque CONNECT tunnel that cannot
// be inspected (no TLS-MITM yet). The CONNECT itself is allowed separately; this
// is purely the honest coverage signal so audit never implies inspection that
// did not happen.
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
	// Bound the dedup map: an agent under a permissive allowlist could
	// otherwise enumerate distinct hostnames to grow it without limit. Past
	// the cap we stop tracking/emitting new blind signals — this SUPPRESSES
	// the coverage signal, it does not make the tunnel inspectable, so the
	// drop is accounted for where the sink already accounts for decision
	// records it couldn't deliver: decisionSink.dropped feeds the periodic
	// synthetic dropped-decisions summary — one mechanism, no second counter
	// — plus an operator-side log line naming the host and the cap.
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
