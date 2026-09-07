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
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"regexp"
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

// maxLLMScanBody bounds how much of an LLM request body the proxy will buffer in
// order to inspect it. A body larger than this is forwarded UNSCANNED (fail-open,
// or refused when block+on_scanner_error=block) and recorded as body_oversize — we
// never truncate (that would corrupt the request). A var (not const) so tests can
// exercise the oversize path without allocating tens of MiB.
var maxLLMScanBody = 32 << 20 // 32 MiB
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

// proxyLLMRequest is the shared reverse-proxy + inspection path for a brokered
// LLM upstream. It applies the startup-minted credential, strips every sandbox-
// supplied credential header, optionally inspects the body (blocking BEFORE the
// allow decision is recorded), and forwards to host:port/<rest> over the vetted
// IP. It is used by both the /wardyn/llm/* local routes and the TLS-MITM CONNECT
// interception (serveMITM), so host/port/rest are passed explicitly — host is
// ALREADY the resolved dial target (the configured gateway's host, or the
// vendor host unset), never the raw vendor constant.
func (p *Proxy) proxyLLMRequest(w http.ResponseWriter, r *http.Request, host string, port int, rest string, channel contentscan.Channel) {
	hdr, ok := p.inject.headerFor(host)
	if !ok {
		p.emitLLMDecision(r, host, port, egress.Deny, ruleSourceLLM, nil)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, `{"wardyn":"no_llm_credential","detail":"no LLM credential is brokered for %s"}`, host)
		return
	}

	// gatewayTarget is this route's OWN resolver, never egressTarget: upstream-
	// first same as every other forward-egress path, but otherwise the
	// gateway's relaxed per-request vet (vetTrustedHost) — never the
	// SSRF-guarded p.vetHost path egressTarget applies to an ordinary host
	// (W23-S1-4 / W19-W19d-3 covered the corp-upstream branch; folding the
	// gateway vet into egressTarget too used to lift the private-IP guard for
	// the gateway HOSTNAME on evaluate/serveMITMRequest as well).
	target, err := p.gatewayTarget(host, port)
	if err != nil {
		// A refused/unreachable configured gateway is a per-request dial
		// failure, not an SSRF-shaped denial — distinguish it in the decision
		// log so it reads as "the gateway didn't answer", not "brokered:llm".
		source := ruleSourceLLM
		if errors.Is(err, errGatewayVet) {
			source = "builtin:dial-failed"
		}
		p.emitLLMDecision(r, host, port, egress.Deny, source, nil)
		p.httpError(w, "llm upstream vet failed", err, http.StatusBadGateway)
		return
	}

	// OPTIONAL outbound content inspection (see inspectLLM). On a confident BLOCK
	// (or a fail-closed uninspectable channel) it writes the 403 itself and
	// returns blocked=true. Otherwise it returns the body to forward and a
	// content-free scan summary to attach (non-nil only when there is something
	// to report — a clean turn stays quiet).
	bodyReader, scanSummary, blocked := p.inspectLLM(w, r, host, port, rest, channel)
	if blocked {
		return
	}
	// The brokered credential is guaranteed present here (headerFor ok above), so
	// the sandbox credential is always stripped and the brokered one injected.
	p.forwardInspectedLLM(w, r, host, port, rest, target, &hdr, ruleSourceLLM, bodyReader, scanSummary)
}

// forwardInspectedLLM is the shared credential-strip + forward-and-respond tail
// for a brokered/MITM'd LLM upstream, used by both proxyLLMRequest and
// serveMITMRequest. It builds the upstream request to host[:port]/<rest> over
// the pinned target (port omitted from the URL at 443 — the default port and
// the byte-identical-to-today shape when no gateway is configured), sanitizes
// hop-by-hop headers, applies the credential (hdr != nil: strip EVERY
// sandbox-supplied credential header — not just Authorization, so a sandbox
// x-api-key cannot substitute the brokered credential when the rule injects
// under a different header — then inject; hdr == nil: preserve the agent's
// own resident credential, inspect-only), records the allow decision
// (scanSummary may be nil = quiet), and streams the response back. ruleSource
// is the decision-log source.
func (p *Proxy) forwardInspectedLLM(w http.ResponseWriter, r *http.Request, host string, port int, rest, target string, hdr *injectedHeader, ruleSource string, bodyReader io.Reader, scanSummary *egress.ScanSummary) {
	hostport := host
	if port != 443 {
		hostport = net.JoinHostPort(host, strconv.Itoa(port))
	}
	upstreamURL := "https://" + hostport + "/" + rest
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}
	outReq, err := http.NewRequestWithContext(
		context.WithValue(r.Context(), vettedIPKey{}, target),
		r.Method, upstreamURL, bodyReader)
	if err != nil {
		p.emitLLMDecision(r, host, port, egress.Deny, ruleSource, nil)
		p.httpError(w, "build llm request", err, http.StatusBadGateway)
		return
	}
	copyHeader(outReq.Header, r.Header)
	removeHopByHop(outReq.Header)
	if hdr != nil {
		outReq.Header.Del("Authorization")
		outReq.Header.Del("X-Api-Key")
		outReq.Header.Del("Api-Key")
		outReq.Header.Del("X-Auth-Token")
		outReq.Header.Set(hdr.name, hdr.value)
	}
	outReq.Host = hostport
	outReq.Header.Del("Host")

	// The allow decision is emitted only AFTER a successful round-trip (same
	// accuracy fix as handleConnect/handlePlain, E3): a failed upstream dial
	// must NOT over-report an allow. Emit a dial-failed deny (carrying any scan
	// summary) instead.
	resp, err := p.transport.RoundTrip(outReq)
	if err != nil {
		p.emitLLMDecision(r, host, port, egress.Deny, "builtin:dial-failed", scanSummary)
		p.httpError(w, "llm upstream error", err, http.StatusBadGateway)
		return
	}
	p.emitLLMDecision(r, host, port, egress.Allow, ruleSource, scanSummary)
	defer func() { _ = resp.Body.Close() }()

	relay(w, resp)
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
// result (error > skip-with-findings > skipped > alert) — a scan that hit
// findings_capped, scan_budget, or attachment_decode_error but still produced
// findings alerts (B2/B4 below), not "skipped". It never copies raw matched
// bytes.
func scanSummaryFrom(res contentscan.Result, serr error, eng *contentscan.Engine, overrideAction string, channel contentscan.Channel) *egress.ScanSummary {
	s := &egress.ScanSummary{
		Scanned:    res.Scanned,
		Coverage:   coverageInspectable,
		Mode:       string(eng.Mode()),
		Channel:    string(channel),
		Skipped:    res.Skipped,
		SkipReason: res.SkipReason,
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
		s.Action = "error"
	case res.Skipped && len(res.Findings) > 0 &&
		(res.SkipReason == "findings_capped" || res.SkipReason == "scan_budget" ||
			res.SkipReason == "attachment_decode_error"):
		// B2/B4 (F075/F073/F056 fix-up): findings_capped, scan_budget, and
		// attachment_decode_error all set Result.Skipped, and this switch used
		// to resolve `case res.Skipped` before ever reaching "alert" — so the
		// decision's Action flipped from "alert" to "skipped" exactly when a
		// real secret was also found alongside a budget/decode limit
		// (egress.ScanSummary.Action is the literal audit-action suffix,
		// docs/AUDIT-ACTIONS.md:71: llm.scan.alert vs llm.scan.skipped — a SIEM
		// rule keyed on llm.scan.alert lost the detected secret). A skipped
		// scan that still carries findings is the loudest scan there is: it
		// still alerts; the truncation/decode-limit rides on
		// Skipped/SkipReason, not on Action. span_oversize is deliberately
		// excluded: it already read "skipped" with findings on base, so
		// including it would be a behaviour change outside this lane's
		// findings.
		s.Action = "alert"
	case res.Skipped:
		s.Action = "skipped"
	default:
		s.Action = "alert"
	}
	return s
}

// inspectLLM runs optional outbound content inspection for an LLM route request
// (Anthropic or OpenAI) and returns the body to forward, a content-free scan
// summary to attach (nil = nothing to report), and blocked=true when it has
// already written a 403/error response (caller must return). It is the single
// decision point for per-endpoint coverage: messages/count_tokens (Anthropic) and
// chat/completions (OpenAI) are scanned; other prompt-bearing subpaths are
// honestly marked uninspected (never silently allowed); non-prompt paths stream
// through quietly. host names the upstream for honest per-host decision logging.
func (p *Proxy) inspectLLM(w http.ResponseWriter, r *http.Request, host string, port int, rest string, channel contentscan.Channel) (io.Reader, *egress.ScanSummary, bool) {
	if p.scanner == nil || p.scanner.Mode() == contentscan.ModeOff {
		return r.Body, nil, false
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
			return nil, nil, true
		}
		return r.Body, p.skipSummary("skipped", "uninspected_channel", channel), false
	default: // scanNone: not prompt-bearing — stream through, stay quiet
		return r.Body, nil, false
	}
}

// hasScannableBody reports whether a forward-proxy request carries a body worth
// inspecting (a body-bearing method with a non-empty/unknown-length body).
func hasScannableBody(r *http.Request) bool {
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
	default:
		return false
	}
	return r.Body != nil && r.ContentLength != 0
}

// inspectForwardBody scans a GENERIC (non-LLM) plaintext-HTTP forward body. Like
// the LLM scanMessages path: buffer (cap), block before forwarding on a confident
// finding, else return the buffered body + a content-free summary. blocked=true
// means a 403/error was already written.
func (p *Proxy) inspectForwardBody(w http.ResponseWriter, r *http.Request, host string, port int) (io.Reader, *egress.ScanSummary, bool) {
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
func (p *Proxy) scanBufferedBody(w http.ResponseWriter, r *http.Request, channel contentscan.Channel, readErrMsg string, emit func(egress.Decision, string, *egress.ScanSummary)) (io.Reader, *egress.ScanSummary, bool) {
	buffered, rerr := io.ReadAll(io.LimitReader(r.Body, int64(maxLLMScanBody)+1))
	if rerr != nil {
		emit(egress.Deny, ruleSourceLLM, nil)
		http.Error(w, readErrMsg, http.StatusBadRequest)
		return nil, nil, true
	}
	if len(buffered) > maxLLMScanBody {
		if p.scanner.BlocksOnError() {
			emit(egress.Deny, ruleSourceLLMBlocked, p.skipSummary("block", "body_oversize", channel))
			writeScanBlocked(w, 0, nil, "body_oversize")
			return nil, nil, true
		}
		return io.MultiReader(bytes.NewReader(buffered), r.Body), p.skipSummary("skipped", "body_oversize", channel), false
	}
	res, _, serr := p.scanner.ScanRequest(channel, buffered)
	if p.scanner.ShouldBlock(res) {
		emit(egress.Deny, ruleSourceLLMBlocked, scanSummaryFrom(res, serr, p.scanner, "block", channel))
		writeScanBlocked(w, len(res.Findings), categoriesOf(res.Findings), res.SkipReason)
		return nil, nil, true
	}
	var sum *egress.ScanSummary
	if len(res.Findings) > 0 || res.Skipped || serr != nil {
		sum = scanSummaryFrom(res, serr, p.scanner, "", channel)
	}
	return bytes.NewReader(buffered), sum, false
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

// LLM-route inspection disposition for a request under /wardyn/llm/anthropic/.
const (
	scanNone     = iota // not prompt-bearing (GET, /v1/models, …): stream, no signal
	scanMessages        // {system,messages} schema (/v1/messages, …/count_tokens)
	scanOpaque          // prompt-bearing but no extractor yet (…/batches): honest skip
)

// classifyLLM dispatches to the per-provider endpoint classifier.
func classifyLLM(channel contentscan.Channel, method, rest string) int {
	switch channel {
	case contentscan.ChannelAnthropicMessages:
		return classifyAnthropicLLM(method, rest)
	case contentscan.ChannelOpenAIChat:
		return classifyOpenAILLM(method, rest)
	default:
		return scanNone
	}
}

// classifyAnthropicLLM decides how a request to the Anthropic route is inspected.
// count_tokens shares the Messages schema, so it is scanned with the same
// extractor; batches carries N prompts in a different shape we cannot parse yet,
// so it is marked uninspected rather than silently allowed.
func classifyAnthropicLLM(method, rest string) int {
	if method != http.MethodPost {
		return scanNone
	}
	r := strings.Trim(rest, "/")
	switch {
	case r == "messages" || strings.HasSuffix(r, "/messages"):
		return scanMessages
	case strings.HasSuffix(r, "/count_tokens"):
		return scanMessages
	case r == "batches" || strings.HasSuffix(r, "/batches"):
		return scanOpaque
	case r == "complete" || strings.HasSuffix(r, "/complete"):
		// Legacy text-completions endpoint: carries the prompt in a shape we don't
		// parse yet — mark uninspected rather than forwarding the brokered
		// credential silently unscanned via the scanNone default.
		return scanOpaque
	default:
		return scanNone
	}
}

// classifyOpenAILLM decides how a request to the OpenAI route is inspected.
// chat/completions is scanned with the openai.chat extractor; responses and
// embeddings carry prompt/input text in shapes we do not parse yet, so they are
// honestly marked uninspected rather than silently allowed.
func classifyOpenAILLM(method, rest string) int {
	if method != http.MethodPost {
		return scanNone
	}
	r := strings.Trim(rest, "/")
	switch {
	case r == "chat/completions" || strings.HasSuffix(r, "/chat/completions"):
		return scanMessages
	case strings.HasSuffix(r, "/responses") || strings.HasSuffix(r, "/embeddings"):
		return scanOpaque
	case r == "completions" || strings.HasSuffix(r, "/completions"):
		// Legacy (non-chat) completions: prompt shape we don't parse yet — mark
		// uninspected, not silently allowed via scanNone. Checked AFTER
		// chat/completions above so that route still scans as Messages.
		return scanOpaque
	default:
		return scanNone
	}
}

// isLLMHost reports whether host is a recognised model-API upstream (used to
// emit honest opaque-tunnel coverage for CONNECT traffic). A method (not a
// free function) so a configured internal gateway's host — reachable only
// through THIS proxy's own gatewayVendor table — counts too.
func (p *Proxy) isLLMHost(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if h == anthropicHost || h == openaiHost {
		return true
	}
	if _, ok := p.gatewayVendor[h]; ok {
		return true
	}
	return isBedrockHost(h)
}

// awsRegionLabel matches an AWS region label (us-east-1, eu-west-3,
// us-gov-west-1, ap-southeast-4). It starts with two LETTERS on purpose — that
// is what separates a region from a virtual-hosted S3 label (s3,
// s3-website-us-east-1, s3-us-west-2), which is customer-squattable inside
// amazonaws.com; see isBedrockHost.
var awsRegionLabel = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-\d+$`)

// isBedrockHost reports whether h (already lowercased, trailing dot trimmed) is
// an AWS Bedrock data-plane (bedrock-runtime) or control-plane (bedrock)
// endpoint — the public regional form OR the PrivateLink/VPC-endpoint form
// `vpce-<id>[-<az>].bedrock-runtime.<region>.vpce.amazonaws.com` (and the
// hyphen-glued `vpce-<id>-bedrock-runtime.<region>.vpce.amazonaws.com`), which
// CONTAINS but does not START WITH the service name — so the old prefix
// matcher dropped private-endpoint model traffic out of isLLMHost's
// opaque-tunnel coverage, the one call an auditor most wants to see.
//
// SECURITY (read before loosening): this is NOT a substring match. Every arm is
// anchored on AWS-OWNED DNS an attacker cannot register — `<region>.amazonaws.com`
// for the public form, `.vpce.amazonaws.com` for the private one — because
// isLLMHost's true weight is in serveMITMRequest, where it picks the inspection
// path for a tunnel that is ALREADY being TLS-terminated: an LLM classification
// routes the body through inspectLLM (ChannelGeneric ⇒ unscanned) instead of
// inspectForwardBody, which honours inspect_forward_egress. A host that could
// talk its way in here would be an operator's corp artifact host silently
// opting itself OUT of forward-body inspection. (It buys nothing else: the
// CONNECT was allowed or denied by policy before isLLMHost is ever consulted,
// and MITM eligibility is isMITMHost's exact-hostname allowlist, never this.)
//
// So `bedrock-runtime.evil.com`, `x-bedrock.attacker.net`,
// `bedrock-runtime.us-east-1.amazonaws.com.evil.com` and the legacy S3
// virtual-host `bedrock-runtime.s3.amazonaws.com` (bucket names ARE
// attacker-chosen) all stay out — the first three fail the AWS suffix, the
// last fails the region shape. Inside `.vpce.amazonaws.com` the service label
// is assigned by AWS (a customer-hosted PrivateLink service is named
// `vpce-svc-<hex>`), so a bedrock component there is genuinely Bedrock's.
func isBedrockHost(h string) bool {
	labels := strings.Split(h, ".")
	bedrock := func(l string) bool { return l == "bedrock-runtime" || l == "bedrock" }
	// Public: bedrock-runtime.<region>.amazonaws.com / bedrock.<region>.amazonaws.com.
	if len(labels) == 4 && bedrock(labels[0]) &&
		labels[2] == "amazonaws" && labels[3] == "com" && awsRegionLabel.MatchString(labels[1]) {
		return true
	}
	// PrivateLink: the bedrock service rides a MIDDLE label (or the tail of the
	// vpce label), under AWS's own vpce zone.
	if !strings.HasSuffix(h, ".vpce.amazonaws.com") {
		return false
	}
	for _, l := range labels {
		if bedrock(l) || strings.HasSuffix(l, "-bedrock-runtime") || strings.HasSuffix(l, "-bedrock") {
			return true
		}
	}
	return false
}

// emitLLMDecision emits a decision log for an LLM or MITM route carrying a
// content-free scan summary. host is the upstream model host; port is the REAL
// destination port — a gateway on :8443 or an artifact mirror MITM'd on its
// configured port must not be recorded as 443 (docs/AUDIT-ACTIONS.md lists
// `port` as an egress.* detail field, and a wrong one is a dishonest row).
func (p *Proxy) emitLLMDecision(r *http.Request, host string, port int, decision egress.Decision, ruleSource string, scan *egress.ScanSummary) {
	if p.sink == nil {
		return
	}
	log := decisionLog(p.reqOf(r, host, port), decision, ruleSource)
	log.Scan = scan
	p.sink.emit(log)
}

// emitLLMBlindOnce emits a single llm.scan.blind signal per LLM host: an
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
	// Bound the dedup map: an agent under a permissive allowlist could otherwise
	// enumerate distinct (e.g. bedrock-runtime.*) hostnames to grow it without
	// limit. Past the cap we stop tracking/emitting new blind signals.
	if len(p.blindHosts) >= maxBlindHosts {
		p.blindMu.Unlock()
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
			Action:   "blind",
		},
	})
}
