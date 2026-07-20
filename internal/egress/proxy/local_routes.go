// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// Brokered LOCAL routes served by the proxy listener itself (origin-form only;
// see ServeHTTP for the security gating). They let the sandbox obtain its
// broker-minted credentials WITHOUT ever holding the run token: the proxy holds
// the run token in its config and injects it when forwarding internal API calls
// to the control plane. The sandbox-supplied Authorization header is always
// stripped before injection so the sandbox cannot smuggle or replace brokered
// credentials.
const (
	localRoutePrefix = "/wardyn/"

	routeMint           = "/wardyn/v1/credentials/mint"
	routeApprovals      = "/wardyn/v1/approvals/"
	routeRecordings     = "/wardyn/v1/recordings/"
	routeScanResults    = "/wardyn/v1/scan-results/"
	routeVerifyResults  = "/wardyn/v1/verify-results/"
	routeComposeResults = "/wardyn/v1/compose-results/"
	// routeSSOToken carries the AWS SSO session captured by an `aws sso login`
	// container-login run (uploaded by wardyn-aws-sso). Same brokered shape as the
	// scan/verify/compose result uploads.
	routeSSOToken = "/wardyn/v1/sso-token/"
	routeLLM      = "/wardyn/llm/"

	// llmAnthropicPrefix selects the Anthropic LLM passthrough. The remainder
	// after this prefix is appended to the Anthropic API base.
	llmAnthropicPrefix = "/wardyn/llm/anthropic/"
	anthropicHost      = "api.anthropic.com"
	// llmOpenAIPrefix selects the OpenAI reverse-proxy passthrough (Codex).
	llmOpenAIPrefix = "/wardyn/llm/openai/"
	openaiHost      = "api.openai.com"

	// maxBlindHosts bounds the per-run opaque-tunnel dedup set (emitLLMBlindOnce).
	maxBlindHosts = 64

	// rule_source values emitted for the brokered routes (audit pipeline).
	ruleSourceMint           = "brokered:mint"
	ruleSourceApprovals      = "brokered:approvals"
	ruleSourceRecordings     = "brokered:recording"
	ruleSourceScanResults    = "brokered:scan-result"
	ruleSourceVerifyResults  = "brokered:verify-result"
	ruleSourceComposeResults = "brokered:compose-result"
	ruleSourceSSOToken       = "brokered:sso-token"
	ruleSourceLLM            = "brokered:llm"
	// ruleSourceLLMBlocked marks an LLM request refused by content inspection;
	// ruleSourceLLMBlind marks an opaque CONNECT to an LLM host that inspection
	// could not see into (honest coverage signal).
	ruleSourceLLMBlocked = "scan:blocked"
	ruleSourceLLMBlind   = "scan:opaque-tunnel"
	// ruleSourceLLMMITM marks a request inspected via TLS-MITM interception of an
	// otherwise-opaque CONNECT tunnel (the subscription-OAuth path).
	ruleSourceLLMMITM = "scan:mitm"
	// ruleSourceArtifactMITM marks a corp artifact-registry request TLS-MITM'd only
	// to inject the operator's registry token on the wire — NOT an LLM/inspection
	// path, so the decision log reads honestly (no scan coverage implied).
	ruleSourceArtifactMITM = "artifact:mitm"

	// maxBrokeredBody caps mint/approvals forward bodies. LLM bodies are
	// unbounded (streamed) — Anthropic is the size authority there.
	maxBrokeredBody = 10 << 20 // 10 MiB
	// maxRecordingBody caps recording uploads (PTY casts compress poorly but
	// are text; 100 MiB is generous for a session).
	maxRecordingBody = 100 << 20
	// maxScanResultBody caps scan-result uploads. ScanFacts is bounded by the
	// scanner's manifest-count + per-file caps, so this is a generous DoS ceiling.
	maxScanResultBody = 8 << 20
)

// maxLLMScanBody bounds how much of an LLM request body the proxy will buffer in
// order to inspect it. A body larger than this is forwarded UNSCANNED (fail-open,
// or refused when block+on_scanner_error=block) and recorded as body_oversize — we
// never truncate (that would corrupt the request). A var (not const) so tests can
// exercise the oversize path without allocating tens of MiB.
var maxLLMScanBody = 32 << 20 // 32 MiB

// handleLocalRoute dispatches an origin-form /wardyn/... request to its
// brokered handler. Unknown /wardyn/... paths are 404.
func (p *Proxy) handleLocalRoute(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == routeMint:
		p.handleBrokerMint(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, routeApprovals):
		p.handleBrokerApproval(w, r)
	case r.Method == http.MethodPut && strings.HasPrefix(path, routeRecordings):
		p.handleBrokerRecording(w, r)
	case r.Method == http.MethodPut && strings.HasPrefix(path, routeScanResults):
		p.handleBrokerScanResult(w, r)
	case r.Method == http.MethodPut && strings.HasPrefix(path, routeVerifyResults):
		p.handleBrokerVerifyResult(w, r)
	case r.Method == http.MethodPut && strings.HasPrefix(path, routeComposeResults):
		p.handleBrokerComposeResult(w, r)
	case r.Method == http.MethodPut && strings.HasPrefix(path, routeSSOToken):
		p.handleBrokerSSOToken(w, r)
	case strings.HasPrefix(path, llmAnthropicPrefix):
		p.handleLLMAnthropic(w, r)
	case strings.HasPrefix(path, llmOpenAIPrefix):
		p.handleLLMOpenAI(w, r)
	case strings.HasPrefix(path, routeGitBroker):
		p.handleGitBroker(w, r)
	default:
		http.Error(w, "unknown brokered route", http.StatusNotFound)
	}
}

// handleBrokerMint forwards POST /wardyn/v1/credentials/mint to the control
// plane's internal mint endpoint with the run token injected. The response
// (status + body) is passed through verbatim.
func (p *Proxy) handleBrokerMint(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBrokeredBody))
	if err != nil {
		http.Error(w, "read request body", http.StatusBadRequest)
		return
	}
	resp, err := p.forwardToControlPlane(r.Context(), http.MethodPost,
		"/api/v1/internal/credentials/mint", body, r.Header.Get("Content-Type"))
	if err != nil {
		p.emitLocalDecision(r, egress.Deny, ruleSourceMint, nil)
		http.Error(w, "control plane error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Capture the upstream body so we can both pass it through and inspect a
	// 409 for the approval id (decision-log enrichment).
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxBrokeredBody))

	var approvalID *uuid.UUID
	if resp.StatusCode == http.StatusConflict {
		approvalID = extractApprovalID(respBody)
	}
	p.emitLocalDecision(r, decisionForStatus(resp.StatusCode), ruleSourceMint, approvalID)

	passThrough(w, resp, respBody)
}

// handleBrokerApproval forwards GET /wardyn/v1/approvals/{id} to the control
// plane's internal approval endpoint with the run token injected.
func (p *Proxy) handleBrokerApproval(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, routeApprovals)
	// Only a bare UUID segment is a valid approval id. Parsing (not just a
	// no-slash check) makes the token-injected forward path structurally
	// traversal-proof regardless of URL re-parsing behavior.
	if _, err := uuid.Parse(id); err != nil {
		http.Error(w, "invalid approval id", http.StatusNotFound)
		return
	}
	resp, err := p.forwardToControlPlane(r.Context(), http.MethodGet,
		"/api/v1/internal/approvals/"+id, nil, "")
	if err != nil {
		p.emitLocalDecision(r, egress.Deny, ruleSourceApprovals, nil)
		http.Error(w, "control plane error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxBrokeredBody))
	p.emitLocalDecision(r, decisionForStatus(resp.StatusCode), ruleSourceApprovals, nil)
	passThrough(w, resp, respBody)
}

// handleBrokerRecording forwards PUT /wardyn/v1/recordings/{runID} to the
// control plane's internal recording-upload endpoint with the run token
// injected. The control plane rejects cross-run uploads (403: token run id
// must match the path run id), so the sandbox can deliver ONLY its own cast —
// this is the multi-node-safe delivery path that replaces the shared volume
// (which leaked recordings across same-uid agent containers).
func (p *Proxy) handleBrokerRecording(w http.ResponseWriter, r *http.Request) {
	p.forwardBrokeredUpload(w, r, routeRecordings, "/api/v1/internal/recordings/",
		ruleSourceRecordings, "read recording body", maxRecordingBody)
}

// forwardBrokeredUpload is the shared PUT-upload path for the brokered
// recording/scan-result/verify-result routes: parse the {runID} from the path,
// read the capped body, forward to the control plane with the run token injected,
// emit the decision, and pass the response through verbatim. The sandbox query
// string is deliberately NOT forwarded — the run→workspace linkage comes from
// trusted control-plane state, never sandbox input.
func (p *Proxy) forwardBrokeredUpload(w http.ResponseWriter, r *http.Request, prefix, cpPathPrefix, ruleSource, readErrMsg string, maxBody int64) {
	id := strings.TrimPrefix(r.URL.Path, prefix)
	if _, err := uuid.Parse(id); err != nil {
		http.Error(w, "invalid run id", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, readErrMsg, http.StatusBadRequest)
		return
	}
	resp, err := p.forwardToControlPlane(r.Context(), http.MethodPut,
		cpPathPrefix+id, body, r.Header.Get("Content-Type"))
	if err != nil {
		p.emitLocalDecision(r, egress.Deny, ruleSource, nil)
		http.Error(w, "control plane error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxBrokeredBody))
	p.emitLocalDecision(r, decisionForStatus(resp.StatusCode), ruleSource, nil)
	passThrough(w, resp, respBody)
}

// handleBrokerScanResult forwards PUT /wardyn/v1/scan-results/{runID} to the
// control plane's internal scan-result-upload endpoint with the run token
// injected — the exact sibling of handleBrokerRecording. The control plane
// rejects cross-run uploads (403: token run id must match the path run id), so a
// governed scan run can deliver ONLY its own facts. The sandbox's query string
// is deliberately NOT forwarded (like recordings): the run→workspace linkage the
// control plane needs must come from TRUSTED state, never from sandbox input.
func (p *Proxy) handleBrokerScanResult(w http.ResponseWriter, r *http.Request) {
	p.forwardBrokeredUpload(w, r, routeScanResults, "/api/v1/internal/scan-results/",
		ruleSourceScanResults, "read scan result body", maxScanResultBody)
}

// handleBrokerVerifyResult forwards PUT /wardyn/v1/verify-results/{runID} to the
// control plane's internal verify-result endpoint with the run token injected —
// the exact sibling of handleBrokerScanResult. Cross-run uploads are rejected
// control-plane-side; the run→workspace linkage comes from trusted state.
func (p *Proxy) handleBrokerVerifyResult(w http.ResponseWriter, r *http.Request) {
	p.forwardBrokeredUpload(w, r, routeVerifyResults, "/api/v1/internal/verify-results/",
		ruleSourceVerifyResults, "read verify result body", maxScanResultBody)
}

// handleBrokerSSOToken forwards PUT /wardyn/v1/sso-token/{runID} to the control
// plane's internal sso-token endpoint with the run token injected — the exact
// sibling of handleBrokerScanResult. It carries the AWS SSO session
// wardyn-aws-sso captured in the sandbox back to the control plane. Cross-run
// uploads are rejected control-plane-side (token run id must match the path run
// id); the sandbox-supplied Authorization is stripped, the run token injected.
func (p *Proxy) handleBrokerSSOToken(w http.ResponseWriter, r *http.Request) {
	p.forwardBrokeredUpload(w, r, routeSSOToken, "/api/v1/internal/sso-token/",
		ruleSourceSSOToken, "read sso token body", maxScanResultBody)
}

// handleBrokerComposeResult forwards PUT /wardyn/v1/compose-results/{runID} to the
// control plane's internal compose-result endpoint with the run token injected —
// the exact sibling of handleBrokerScanResult. It carries the AI Run Composer's
// in-sandbox claude proposal JSON back to the waiting RunClaudeCompose. Cross-run
// uploads are rejected control-plane-side (token run id must match the path run
// id); the sandbox-supplied Authorization is stripped, the run token injected.
func (p *Proxy) handleBrokerComposeResult(w http.ResponseWriter, r *http.Request) {
	p.forwardBrokeredUpload(w, r, routeComposeResults, "/api/v1/internal/compose-results/",
		ruleSourceComposeResults, "read compose result body", maxScanResultBody)
}

// forwardToControlPlane builds and sends a request to the control plane,
// injecting the run token as Authorization. The control-plane host is resolved
// and IP-vetted once, and the vetted target is pinned on the request context so
// the shared transport dials it directly (no re-resolution). The inbound
// sandbox Authorization is never carried here — this request is constructed
// fresh, and only the run token is set.
func (p *Proxy) forwardToControlPlane(ctx context.Context, method, path string, body []byte, contentType string) (*http.Response, error) {
	if p.controlPlaneURL == "" {
		return nil, fmt.Errorf("control plane url not configured")
	}
	// The control-plane URL is TRUSTED operator configuration (same trust
	// boundary as the run token the proxy already holds), NOT an agent-chosen
	// target. It legitimately resolves to a private-network address (wardynd on
	// a Docker/k8s internal net). The agent-SSRF private/reserved-IP guard
	// (invariant 3) must therefore NOT apply here — it exists to stop the
	// SANDBOX from reaching internal/metadata IPs via the forward-proxy path,
	// not to stop the proxy from reaching its own control plane. We still
	// resolve+pin the IP so the dial cannot be re-pointed mid-request.
	target, err := p.resolveTrustedURL(p.controlPlaneURL)
	if err != nil {
		return nil, err
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(
		context.WithValue(ctx, vettedIPKey{}, target),
		method, p.controlPlaneURL+path, rdr)
	if err != nil {
		return nil, err
	}
	// Inject the run token ONLY toward the control plane.
	req.Header.Set("Authorization", "Bearer "+p.runToken.Get())
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return p.localClient.Do(req)
}

// handleLLMAnthropic proxies /wardyn/llm/anthropic/<rest> to
// https://api.anthropic.com/<rest> with the brokered Anthropic credential.
func (p *Proxy) handleLLMAnthropic(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, llmAnthropicPrefix)
	p.proxyLLMRequest(w, r, anthropicHost, rest, contentscan.ChannelAnthropicMessages)
}

// handleLLMOpenAI proxies /wardyn/llm/openai/<rest> to https://api.openai.com/<rest>
// with the brokered OpenAI credential (the Codex reverse-proxy route).
func (p *Proxy) handleLLMOpenAI(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, llmOpenAIPrefix)
	p.proxyLLMRequest(w, r, openaiHost, rest, contentscan.ChannelOpenAIChat)
}

// proxyLLMRequest is the shared reverse-proxy + inspection path for a brokered
// LLM upstream. It applies the startup-minted credential, strips every sandbox-
// supplied credential header, optionally inspects the body (blocking BEFORE the
// allow decision is recorded), and forwards to host/<rest> over the vetted IP.
// It is used by both the /wardyn/llm/* local routes and the TLS-MITM CONNECT
// interception (serveMITM), so host/rest are passed explicitly.
func (p *Proxy) proxyLLMRequest(w http.ResponseWriter, r *http.Request, host, rest string, channel contentscan.Channel) {
	hdr, ok := p.inject.headerFor(host)
	if !ok {
		p.emitLLMDecision(r, host, egress.Deny, ruleSourceLLM, nil)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, `{"wardyn":"no_llm_credential","detail":"no LLM credential is brokered for %s"}`, host)
		return
	}

	target, err := p.vetURL("https://" + host)
	if err != nil {
		p.emitLLMDecision(r, host, egress.Deny, ruleSourceLLM, nil)
		http.Error(w, "llm upstream vet failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	// OPTIONAL outbound content inspection (see inspectLLM). On a confident BLOCK
	// (or a fail-closed uninspectable channel) it writes the 403 itself and
	// returns blocked=true. Otherwise it returns the body to forward and a
	// content-free scan summary to attach (non-nil only when there is something
	// to report — a clean turn stays quiet).
	bodyReader, scanSummary, blocked := p.inspectLLM(w, r, host, rest, channel)
	if blocked {
		return
	}
	// The brokered credential is guaranteed present here (headerFor ok above), so
	// the sandbox credential is always stripped and the brokered one injected.
	p.forwardInspectedLLM(w, r, host, rest, target, &hdr, ruleSourceLLM, bodyReader, scanSummary)
}

// forwardInspectedLLM is the shared credential-strip + forward-and-respond tail
// for a brokered/MITM'd LLM upstream, used by both proxyLLMRequest and
// serveMITMRequest. It builds the upstream request to host/<rest> over the pinned
// target, sanitizes hop-by-hop headers, applies the credential (hdr != nil: strip
// EVERY sandbox-supplied credential header — not just Authorization, so a sandbox
// x-api-key cannot substitute the brokered credential when the rule injects under
// a different header — then inject; hdr == nil: preserve the agent's own resident
// credential, inspect-only), records the allow decision (scanSummary may be nil =
// quiet), and streams the response back. ruleSource is the decision-log source.
func (p *Proxy) forwardInspectedLLM(w http.ResponseWriter, r *http.Request, host, rest, target string, hdr *injectedHeader, ruleSource string, bodyReader io.Reader, scanSummary *egress.ScanSummary) {
	upstreamURL := "https://" + host + "/" + rest
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}
	outReq, err := http.NewRequestWithContext(
		context.WithValue(r.Context(), vettedIPKey{}, target),
		r.Method, upstreamURL, bodyReader)
	if err != nil {
		p.emitLLMDecision(r, host, egress.Deny, ruleSource, nil)
		http.Error(w, "build llm request: "+err.Error(), http.StatusBadGateway)
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
	outReq.Host = host
	outReq.Header.Del("Host")

	p.emitLLMDecision(r, host, egress.Allow, ruleSource, scanSummary)

	resp, err := p.transport.RoundTrip(outReq)
	if err != nil {
		http.Error(w, "llm upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	dst := w.Header()
	copyHeader(dst, resp.Header)
	removeHopByHop(dst)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
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
// result (error > skipped > alert). It never copies raw matched bytes.
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
func (p *Proxy) inspectLLM(w http.ResponseWriter, r *http.Request, host, rest string, channel contentscan.Channel) (io.Reader, *egress.ScanSummary, bool) {
	if p.scanner == nil || p.scanner.Mode() == contentscan.ModeOff {
		return r.Body, nil, false
	}
	switch classifyLLM(channel, r.Method, rest) {
	case scanMessages:
		return p.scanBufferedBody(w, r, channel, "read llm body",
			func(d egress.Decision, ruleSource string, scan *egress.ScanSummary) {
				p.emitLLMDecision(r, host, d, ruleSource, scan)
			})
	case scanOpaque:
		// Prompt-bearing subpath we cannot parse yet (e.g. /v1/messages/batches,
		// OpenAI /v1/responses): emit an HONEST uninspected-channel skip rather
		// than a silent allow, and refuse it under fail-closed blocking.
		if p.scanner.BlocksOnError() {
			p.emitLLMDecision(r, host, egress.Deny, ruleSourceLLMBlocked, p.skipSummary("block", "uninspected_channel", channel))
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
			p.emitLLMDecisionPort(r, host, port, d, ruleSource, scan)
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
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
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
// emit honest opaque-tunnel coverage for CONNECT traffic).
func isLLMHost(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if h == anthropicHost || h == openaiHost {
		return true
	}
	// AWS Bedrock runtime endpoints are regional.
	return strings.HasPrefix(h, "bedrock-runtime.") || strings.HasPrefix(h, "bedrock.")
}

// emitLLMDecision emits a decision log for an LLM route (port 443) carrying a
// content-free scan summary. host is the upstream model host.
func (p *Proxy) emitLLMDecision(r *http.Request, host string, decision egress.Decision, ruleSource string, scan *egress.ScanSummary) {
	p.emitLLMDecisionPort(r, host, 443, decision, ruleSource, scan)
}

// emitLLMDecisionPort is emitLLMDecision with an explicit port — used by the
// generic forward path, where a connector may be on port 80 or a custom port and
// recording 443 would be dishonest.
func (p *Proxy) emitLLMDecisionPort(r *http.Request, host string, port int, decision egress.Decision, ruleSource string, scan *egress.ScanSummary) {
	if p.sink == nil {
		return
	}
	log := egress.DecisionLog{
		Request: egress.Request{
			RunID:  p.runID,
			Host:   host,
			Port:   port,
			Method: strings.ToUpper(r.Method),
			Path:   r.URL.Path,
			Time:   p.now(),
		},
		Decision:   decision,
		RuleSource: ruleSource,
		Scan:       scan,
	}
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

// vetURL resolves the host of rawURL through the SSRF guard and returns the
// pinned "ip:port" dial target. The port defaults to the URL scheme's default.
// Fails closed on any unparseable URL or blocked address.
func (p *Proxy) vetURL(rawURL string) (string, error) {
	host, port, err := hostPortFromURL(rawURL)
	if err != nil {
		return "", err
	}
	guard := VetHost(host, p.res)
	if guard.Denied {
		return "", fmt.Errorf("host %q denied: %s", host, guard.Reason)
	}
	return net.JoinHostPort(guard.IP.String(), strconv.Itoa(port)), nil
}

// resolveTrustedURL resolves the host of a TRUSTED rawURL (the operator-
// configured control-plane endpoint) to a pinned "ip:port" dial target WITHOUT
// applying the private/reserved-IP denial. Unlike vetURL, this is used only for
// the proxy's own control-plane forwarding, where a private-network address is
// expected and legitimate. Resolution still pins a single IP (TOCTOU / DNS-
// rebinding guard); a literal IP is used as-is. Fails closed on any unparseable
// URL or unresolvable host.
func (p *Proxy) resolveTrustedURL(rawURL string) (string, error) {
	host, port, err := hostPortFromURL(rawURL)
	if err != nil {
		return "", err
	}
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if h == "" {
		return "", fmt.Errorf("trusted url %q has empty host", rawURL)
	}
	// Literal IP: pin directly (no denial — trusted destination).
	if ip := net.ParseIP(h); ip != nil {
		return net.JoinHostPort(ip.String(), strconv.Itoa(port)), nil
	}
	res := p.res
	if res == nil {
		res = netResolver{}
	}
	ips, err := res.LookupIP(h)
	if err != nil {
		return "", fmt.Errorf("resolve trusted host %q: %w", h, err)
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("trusted host %q resolved to no addresses", h)
	}
	return net.JoinHostPort(ips[0].String(), strconv.Itoa(port)), nil
}

// hostPortFromURL extracts the lowercased host and port from a base URL,
// defaulting the port from the scheme (http=80, https=443).
func hostPortFromURL(rawURL string) (host string, port int, err error) {
	u := rawURL
	scheme := ""
	if i := strings.Index(u, "://"); i >= 0 {
		scheme = strings.ToLower(u[:i])
		u = u[i+3:]
	}
	// Strip any path/query — only the authority matters.
	if i := strings.IndexAny(u, "/?#"); i >= 0 {
		u = u[:i]
	}
	if u == "" {
		return "", 0, fmt.Errorf("url %q has no host", rawURL)
	}
	defaultPort := 80
	if scheme == "https" {
		defaultPort = 443
	}
	host, port = splitHostPort(u, defaultPort)
	if host == "" {
		return "", 0, fmt.Errorf("url %q has empty host", rawURL)
	}
	return host, port, nil
}

// passThrough writes a forwarded control-plane response verbatim: status code
// and body, with hop-by-hop headers stripped.
func passThrough(w http.ResponseWriter, resp *http.Response, body []byte) {
	dst := w.Header()
	copyHeader(dst, resp.Header)
	removeHopByHop(dst)
	// The captured body length is authoritative; drop any upstream
	// Content-Length that may not match after capping.
	dst.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// decisionForStatus maps a forwarded control-plane status to an egress
// decision for the brokered decision log: 2xx is allow, anything else deny.
// (A 409 pending is recorded as deny here only for the decision-log decision
// field; the approval_id is carried separately so audit can correlate.)
func decisionForStatus(status int) egress.Decision {
	if status >= 200 && status < 300 {
		return egress.Allow
	}
	return egress.Deny
}

// extractApprovalID pulls "approval_id" out of a control-plane 409 body, if
// present and parseable. Best-effort: a malformed body yields nil.
func extractApprovalID(body []byte) *uuid.UUID {
	var m struct {
		ApprovalID string `json:"approval_id"`
	}
	if err := json.Unmarshal(body, &m); err != nil || m.ApprovalID == "" {
		return nil
	}
	id, err := uuid.Parse(m.ApprovalID)
	if err != nil {
		return nil
	}
	return &id
}

// emitLocalDecision records a DecisionLog for a brokered local route so it
// lands in audit via the existing decisions pipeline. The host recorded is the
// brokered upstream target (control plane host for mint/approvals, the
// Anthropic host for the LLM route).
func (p *Proxy) emitLocalDecision(r *http.Request, decision egress.Decision, ruleSource string, approvalID *uuid.UUID) {
	if p.sink == nil {
		return
	}
	host := ""
	switch ruleSource {
	case ruleSourceLLM:
		host = anthropicHost
	default:
		if h, _, err := hostPortFromURL(p.controlPlaneURL); err == nil {
			host = h
		}
	}
	log := egress.DecisionLog{
		Request: egress.Request{
			RunID:  p.runID,
			Host:   host,
			Method: strings.ToUpper(r.Method),
			Path:   r.URL.Path,
			Time:   p.now(),
		},
		Decision:   decision,
		RuleSource: ruleSource,
		ApprovalID: approvalID,
	}
	p.sink.emit(log)
}
