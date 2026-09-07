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
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

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

// maxConcurrentScans bounds how many request bodies may be BUFFERED AND
// EXTRACTED at once, process-wide (one proxy per sidecar process).
//
// It is a memory bound, not a throughput knob (F074). The buffer+extract path
// does not cost one body: contentscan's extractor re-materialises it several
// times over (content array -> []json.RawMessage, each block -> a struct with
// its own string, a generic body -> interface{} boxing), measured at ~5.3x live
// heap and essentially independent of which detectors are on — it is the
// extractor, not the scanning. Meanwhile the wardyn-proxy sidecar runs under a
// HARD 256 MiB cgroup cap with swap pinned equal (internal/runner/docker's
// proxyMemoryMiB, internal/runner/k8s). One in-cap 30 MiB body peaks at ~158
// MiB of live heap; TWO concurrent peak at ~274 MiB — already over the cap, and
// an OOM-killed proxy sidecar takes the run's only network path with it. The
// agent inside the sandbox picks both the body sizes and the concurrency, so
// nothing else bounds this.
//
// Waiting (rather than skipping the scan) is deliberate: a queued request is
// still fully inspected, so load can never turn into unscanned egress. The wait
// is bounded by scanQueueWait below — NOT by anything above this call.
const maxConcurrentScans = 1

// scanSlots is maxConcurrentScans' semaphore. Package-level because the bound
// it enforces is the PROCESS's cgroup memory cap, not any one Proxy's.
var scanSlots = make(chan struct{}, maxConcurrentScans)

// maxRetainedScanBytes bounds the total buffered request bytes inspection may
// hold live at once, counted for as long as the buffer is REACHABLE — not just
// while it is being scanned.
//
// TRUST BOUNDARY (F074 fix-up): the scan slot above bounds the buffer+extract
// WINDOW; it says nothing about the buffer's LIFETIME. scanBufferedBody hands
// its caller a re-readable copy of the whole body and the caller then streams
// it through RoundTrip, so the slot was already released while up to
// maxLLMScanBody (32 MiB) stayed live per in-flight request. N requests stalled
// on a slow upstream therefore retained N x 32 MiB with NO slot held: one 32
// MiB body extracting (~170 MiB at the measured 5.3x) plus three already-scanned
// ones waiting on the upstream (96 MiB) is ~266 MiB against the sidecar's hard
// 256 MiB cgroup cap — the very arithmetic maxConcurrentScans exists to
// prevent, reached around it.
//
// 64 MiB leaves room beside one in-flight extraction under that cap, and the
// budget is charged AFTER the scan peak (see scanBufferedBody) so the two do
// not double-count the same request's peak. A var so tests can shrink it
// instead of allocating tens of MiB.
var maxRetainedScanBytes = 64 << 20 // 64 MiB

// scanRetained is that budget. Package-level for the same reason scanSlots is:
// the ceiling is the PROCESS's.
var scanRetained = &byteBudget{limit: func() int { return maxRetainedScanBytes }}

// byteBudget is a context-bounded semaphore over BYTES (the slot semaphore
// counts requests, which is the wrong unit for a memory bound).
//
// Acquisition is serialized by construction: scanBufferedBody charges the
// budget while still holding its scan slot, and maxConcurrentScans is 1, so
// there is never more than one acquirer and a partial acquisition cannot
// deadlock against another. The `used == 0` escape hatch keeps a single body
// larger than the whole budget from waiting forever on a budget only it could
// free.
type byteBudget struct {
	limit    func() int
	mu       sync.Mutex
	used     int
	released chan struct{}
}

// acquire charges n bytes, waiting (ctx-bounded) for room. It reports whether
// the charge was taken; false means the caller must FAIL CLOSED, exactly as an
// expired scan-slot wait does.
func (b *byteBudget) acquire(ctx context.Context, n int) bool {
	tick := time.NewTicker(scanRetainPollInterval)
	defer tick.Stop()
	for {
		b.mu.Lock()
		if b.used == 0 || b.used+n <= b.limit() {
			b.used += n
			b.mu.Unlock()
			return true
		}
		if b.released == nil {
			b.released = make(chan struct{}, 1)
		}
		waiter := b.released
		b.mu.Unlock()
		select {
		case <-waiter:
		case <-tick.C: // re-check: a wakeup may have been coalesced
		case <-ctx.Done():
			return false
		}
	}
}

// release returns n bytes. Idempotent per call site via the closure
// scanBufferedBody hands out, so a caller's `defer release()` cannot
// double-credit.
func (b *byteBudget) release(n int) {
	b.mu.Lock()
	b.used -= n
	if b.used < 0 {
		b.used = 0
	}
	waiter := b.released
	b.mu.Unlock()
	if waiter != nil {
		select {
		case waiter <- struct{}{}:
		default:
		}
	}
}

// inUse reports the bytes currently charged (tests observe the LIFETIME claim
// with it).
func (b *byteBudget) inUse() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// scanRetainPollInterval re-checks the budget after a coalesced wakeup. Only
// reached under memory pressure.
const scanRetainPollInterval = 25 * time.Millisecond

// scanQueueWait bounds how long a request may wait for a scan slot.
//
// It has to be bounded HERE, because nothing above this call bounds it: the
// agent-facing listener that serves handlePlain and /wardyn/llm/* sets
// ReadTimeout 0 — streaming bodies and CONNECT tunnels need it (NewServer,
// server.go) — and only the inner MITM server carries a whole-request deadline.
// With maxConcurrentScans at 1 and the slot held across io.ReadAll of a
// SANDBOX-controlled body, a single slow-loris POST would otherwise park every
// other inspected request of the run in the semaphore send forever, each
// retaining a goroutine and a socket in a 256 MiB sidecar (the retention class
// F079 names, reached through the inspection path and triggerable by the
// untrusted sandbox).
//
// Expiring the wait fails CLOSED — Deny + 502, like the read-error arm beside
// it — so the bound cannot become a way to get a body forwarded unscanned. The
// value is generous relative to a healthy scan (an in-cap body extracts and
// scans in well under a second) and short relative to a hung one, so it fires
// on abuse rather than on load.
const scanQueueWait = 30 * time.Second

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

	// llmRouteTarget picks the resolver by what the host actually IS: a
	// CONTROL-PLANE-authored gateway gets gatewayTarget's relaxed per-request
	// vet (vetTrustedHost); every other host — including the PUBLIC vendor host
	// when no gateway is configured at all — gets egressTarget's SSRF-guarded
	// p.vetHost, exactly as on every other forward-egress path
	// (W23-S1-4 / W19-W19d-3 covered the corp-upstream branch; folding the
	// gateway vet into egressTarget too used to lift the private-IP guard for
	// the gateway HOSTNAME on evaluate/serveMITMRequest as well).
	target, err := p.llmRouteTarget(host, port)
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
	bodyReader, scanSummary, releaseBody, blocked := p.inspectLLM(w, r, host, port, rest, channel)
	// The buffered body stays charged to maxRetainedScanBytes until the upstream
	// round trip has consumed it (F074).
	defer releaseBody()
	if blocked {
		return
	}
	// The brokered credential is guaranteed present here (headerFor ok above), so
	// the sandbox credential is always stripped and the brokered one injected.
	p.forwardInspectedLLM(w, r, host, port, rest, target, &hdr, ruleSourceLLM, bodyReader, scanSummary)
}

// llmRouteTarget resolves the brokered LLM route's dial target, choosing the
// vet by what host IS rather than by which route asked.
//
// TRUST BOUNDARY (F087 — read before widening): gatewayTarget/vetTrustedHost is
// the RELAXED vet. It admits RFC1918/ULA/CGNAT by design, because an
// operator-configured internal model gateway is expected to live there, and it
// is safe ONLY because that host was typed into the control plane at boot.
// Applying it unconditionally to every host proxyLLMRequest dials extended the
// relaxation to `api.anthropic.com`/`api.openai.com` on a run with NO gateway
// configured, so a poisoned/split-horizon resolver answering RFC1918 for the
// public vendor host got the startup-minted brokered credential delivered to
// it — the exact DNS-rebinding case the unconditional private-IP guard exists
// for, and the guard v0.6.6 had on this path. A host that is not in
// p.gatewayVendor is an ordinary host: it takes egressTarget.
//
// This is what THREAT-MODEL.md residual #29 and OPERATIONS.md already state —
// "only the brokered /wardyn/llm/* route (Proxy.gatewayTarget) resolves or
// dials the GATEWAY with the relaxed per-request vet" — the code now matches.
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
	// Build the upstream target as a STRUCTURED url.URL, never by concatenating
	// `rest` into a string that http.NewRequestWithContext then re-PARSES.
	//
	// TRUST BOUNDARY (F035): `rest` is the PERCENT-DECODED path (r.URL.Path),
	// and it is the same value classifyLLM keys the inspection decision on. Fed
	// back through a URL parser, a decoded "#" becomes a FRAGMENT and a decoded
	// "?" becomes a QUERY, so `POST /wardyn/llm/anthropic/v1/messages%23z`
	// classified as "v1/messages#z" (scanNone — streamed through unscanned)
	// while the wire carried `POST /v1/messages`: the scanner and the upstream
	// disagreed about where the path ends, which is a bypass of block mode from
	// inside the sandbox. Setting URL.Path (an already-decoded field) makes the
	// request re-encode those bytes as %23/%3F, so the path the classifier
	// judged is byte-for-byte the path that is sent.
	upstreamURL := &url.URL{
		Scheme:   "https",
		Host:     hostport,
		Path:     "/" + rest,
		RawQuery: r.URL.RawQuery,
	}
	outReq, err := http.NewRequestWithContext(
		context.WithValue(r.Context(), vettedIPKey{}, target),
		r.Method, upstreamURL.String(), bodyReader)
	if err != nil {
		p.emitLLMDecision(r, host, port, egress.Deny, ruleSource, nil)
		p.httpError(w, "build llm request", err, http.StatusBadGateway)
		return
	}
	copyHeader(outReq.Header, r.Header)
	removeHopByHop(outReq.Header)
	if hdr != nil {
		// One definition of "the sandbox's own credential headers", shared with
		// the plain forward lane's injector.apply (inject.go, F104).
		stripSandboxCredentials(outReq.Header)
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
		// The truncation, on the wire (F075): the flag survives an earlier skip
		// reason claiming SkipReason, and the counts let an auditor tell a
		// capped scan from one that found exactly maxFindings.
		FindingsCapped:  res.FindingsCapped || res.SkipReason == "findings_capped",
		FindingsPastCap: res.FindingsDropped,
	}
	if s.FindingsCapped {
		// The COUNTED pre-cap total, not len(Findings)+FindingsDropped: under
		// mode=block capFindings keeps a block-relevant past-cap finding back
		// INTO Findings while still counting it as dropped, so that sum
		// double-counts every keep-back (measured: 1,500 for a 1,200-finding
		// body). See contentscan.Result.FindingsSeen.
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
		s.Action = "error"
	case (res.Skipped || res.FindingsCapped) && len(res.Findings) > 0 &&
		(res.FindingsCapped || res.SkipReason == "findings_capped" ||
			res.SkipReason == "scan_budget" || res.SkipReason == "attachment_decode_error"):
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
		return r.Body, p.skipSummary("skipped", "uninspected_channel", channel), noRelease, false
	default: // scanNone: not prompt-bearing — stream through, stay quiet
		return r.Body, nil, noRelease, false
	}
}

// bodyBearingMethod reports whether a method may carry a request body Wardyn
// would want to inspect.
//
// TRUST BOUNDARY (F088/F112): this is the ONE definition, shared by
// hasScannableBody and by both LLM endpoint classifiers, because the two used
// to disagree — hasScannableBody accepted POST/PUT/PATCH while the classifiers
// opened with `if method != http.MethodPost { return scanNone }`. The sandbox
// picks the verb as freely as it picks the suffix, so `PUT /v1/messages` with a
// secret in the body reached the vendor with the operator's brokered credential
// under mode=block, allowed, with scanSummary=nil — audit-indistinguishable
// from a bodiless GET /v1/models. Closing the suffix axis (F112's fail-closed
// default) while leaving the verb axis open just moved the same bypass one
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
// The returned release MUST be deferred by the caller: it is what gives the
// buffer back to maxRetainedScanBytes, and the buffer stays charged until it
// runs (F074 — the scan slot only ever bounded the extract window). It is
// always non-nil and safe to call more than once.
func (p *Proxy) scanBufferedBody(w http.ResponseWriter, r *http.Request, channel contentscan.Channel, readErrMsg string, emit func(egress.Decision, string, *egress.ScanSummary)) (io.Reader, *egress.ScanSummary, func(), bool) {
	// Take a scan slot BEFORE buffering: the slot is what bounds live heap
	// against the sidecar's cgroup cap, so it has to be held across the
	// io.ReadAll as well as the extract+scan (see maxConcurrentScans).
	//
	// The wait is taken through the request context AND a wall-clock bound
	// (scanQueueWait) because nothing above this call deadlines it, and it fails
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
	// Charge the buffer to the LIFETIME budget before handing it back. Done
	// here, after the read and the scan peak, for two reasons: the size is only
	// known now, and the extraction peak this request is about to leave behind
	// is what the slot above already accounted for. Still under the slot, so
	// there is exactly one acquirer (see byteBudget). Expiry fails CLOSED — the
	// same Deny + 502 the slot wait takes — so memory pressure can never turn
	// into a body forwarded unscanned.
	charge := func() (func(), bool) {
		n := len(buffered)
		if !scanRetained.acquire(ctx, n) {
			emit(egress.Deny, ruleSourceLLM, nil)
			p.httpError(w, readErrMsg, ctx.Err(), http.StatusBadGateway)
			return noRelease, false
		}
		var once sync.Once
		return func() { once.Do(func() { scanRetained.release(n) }) }, true
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
			p.skipSummary("skipped", "body_oversize", channel), release, false
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
// extractor; every other body-bearing request carries content in a shape we
// cannot parse yet, so it is marked uninspected rather than silently allowed —
// see the default arm's F112/F088 note.
func classifyAnthropicLLM(method, rest string) int {
	// The quiet answer belongs to methods that carry NO body (GET/HEAD/DELETE/…),
	// not to "anything that is not a POST" (F088/F112 — bodyBearingMethod is the
	// one definition hasScannableBody uses too). The named arms below stay
	// POST-only because POST is the only verb the vendor documents for them, so
	// a PUT/PATCH to the same path is exactly an unrecognised body-bearing
	// request and falls to the fail-closed default.
	if !bodyBearingMethod(method) {
		return scanNone
	}
	r := strings.Trim(rest, "/")
	switch {
	case method == http.MethodPost && (r == "messages" || strings.HasSuffix(r, "/messages")):
		return scanMessages
	case method == http.MethodPost && strings.HasSuffix(r, "/count_tokens"):
		return scanMessages
	default:
		// FAIL-CLOSED DEFAULT (F112): every OTHER POST on this route is
		// uninspected, not quiet. /v1/messages/batches and the legacy /v1/complete
		// used to be the only named ones, so the vendor's CONTENT-UPLOAD surface
		// fell to the old scanNone default: POST /v1/files is a multipart upload
		// of arbitrary user bytes (documented 500 MB ceiling) and it was forwarded
		// with the operator's brokered credential, unscanned, carrying NO scan
		// block at all — a row indistinguishable in audit from GET /v1/models.
		// Worse, under block + on_scanner_error=block the strict operator's one
		// hard control refused `batches` as uninspected_channel while forwarding
		// `files`: the control was bypassable by choosing a different suffix.
		//
		// An enumerated allowlist cannot hold this line, because the SANDBOX picks
		// the whole suffix (handleLocalRoute dispatches on a bare prefix match and
		// forwards <rest> verbatim) and the VENDOR adds endpoints without asking
		// us. So the default is the honest answer — "prompt-bearing but no
		// extractor yet" — and a new arm above is what earns silence.
		//
		// F088 second axis: the same is true of the VERB. `PUT /v1/messages` is
		// not a documented Anthropic call, so it lands here rather than on the
		// scanMessages arm — uninspected and refused under fail-closed blocking,
		// instead of the silent brokered forward it used to get.
		return scanOpaque
	}
}

// classifyOpenAILLM decides how a request to the OpenAI route is inspected.
// chat/completions is scanned with the openai.chat extractor; responses and
// embeddings carry prompt/input text in shapes we do not parse yet, so they are
// honestly marked uninspected rather than silently allowed.
func classifyOpenAILLM(method, rest string) int {
	// Same two rules as classifyAnthropicLLM: only a bodiless method is quiet
	// (F088/F112), and the named arm is POST-only so any other body-bearing verb
	// on the same path is an unrecognised call, not a scanned one.
	if !bodyBearingMethod(method) {
		return scanNone
	}
	r := strings.Trim(rest, "/")
	switch {
	case method == http.MethodPost && (r == "chat/completions" || strings.HasSuffix(r, "/chat/completions")):
		return scanMessages
	default:
		// FAIL-CLOSED DEFAULT (F112), same rule as classifyAnthropicLLM: /responses,
		// /embeddings and the legacy /completions used to be enumerated here while
		// the vendor's own OpenAPI spec also defines POST /v1/files and the
		// multipart /v1/audio/{transcriptions,translations} uploads plus
		// /v1/audio/speech — all of which fell to the old scanNone default and
		// streamed through with the brokered credential and no scan block. The
		// enumeration also had to carry each endpoint's BARE spelling (F088: with
		// no gateway prefix configured, `rest` for POST /wardyn/llm/openai/responses
		// is exactly "responses", which a suffix-only arm missed) — a second way
		// the same list could silently lose an endpoint. The default answers both.
		return scanOpaque
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
	// Public: <service>.<region>.amazonaws.com, and the dual-stack
	// <service>.<region>.api.aws form AWS publishes beside it. Both tails are
	// AWS-owned zones; the region shape stays the anchor that keeps the
	// customer-squattable S3 virtual-host labels out.
	if len(labels) == 4 && bedrockServiceLabel(labels[0]) && awsRegionLabel.MatchString(labels[1]) &&
		((labels[2] == "amazonaws" && labels[3] == "com") || (labels[2] == "api" && labels[3] == "aws")) {
		return true
	}
	// PrivateLink: the bedrock service rides a MIDDLE label (or the tail of the
	// vpce label), under AWS's own vpce zone.
	if !strings.HasSuffix(h, ".vpce.amazonaws.com") {
		return false
	}
	for _, l := range labels {
		if bedrockServiceLabel(l) {
			return true
		}
		// Hyphen-glued: `vpce-<id>-bedrock-agent-runtime`. The service name must
		// start at a hyphen boundary — `notbedrock-runtime` is not Bedrock.
		if i := strings.Index(l, "-bedrock"); i > 0 && bedrockServiceLabel(l[i+1:]) {
			return true
		}
	}
	return false
}

// bedrockServiceLabel reports whether l is one of the Bedrock service labels AWS
// publishes in its service-endpoint reference
// (https://docs.aws.amazon.com/general/latest/gr/bedrock.html), with or without
// the `-fips` variant.
//
// F113 — this used to be the two literals `bedrock-runtime` and `bedrock`, so
// six of AWS's eight published labels fell out of isLLMHost: every `-fips`
// endpoint (a FedRAMP-High workload is generally REQUIRED to use one, and
// GovCloud publishes `bedrock-runtime-fips.us-gov-{west,east}-1.amazonaws.com`)
// and the whole agent family, including `bedrock-agent-runtime` — the
// InvokeAgent DATA plane, i.e. prompt-bearing model traffic. The consequence is
// on the honesty side: proxy.go emits the one-time `llm.scan.blind` coverage row
// only under isLLMHost, so a CONNECT to a FIPS Bedrock endpoint was opaque AND
// unflagged — an audit trail showing an egress.allow and no blind row anywhere,
// which reads as "no model tunnel happened", against a THREAT-MODEL.md that
// says those tunnels "stay opaque and flagged llm.scan.blind". It also mislabels
// the MITM decision row for such a host as corp-artifact rather than model
// traffic.
//
// The enumeration is deliberately exhaustive-by-name rather than a
// `strings.HasPrefix(l, "bedrock")`: isBedrockHost's callers treat a match as
// "this is model traffic", and a prefix test would admit any future
// AWS-adjacent label — and, inside `.vpce.amazonaws.com`, any label an operator
// happened to name that way — without anyone re-reading the security note above.
func bedrockServiceLabel(l string) bool {
	switch strings.TrimSuffix(l, "-fips") {
	case "bedrock", // control plane
		"bedrock-runtime",                 // InvokeModel data plane
		"bedrock-agent",                   // agents build-time
		"bedrock-agent-runtime",           // InvokeAgent data plane
		"bedrock-data-automation",         // data automation build-time
		"bedrock-data-automation-runtime": // data automation data plane
		return true
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
	//
	// F066 — the cap SUPPRESSES the coverage signal; it does not make the tunnel
	// inspectable, and this function exists precisely so "audit never implies
	// inspection that did not happen". Returning silently made the 65th
	// uninspected model tunnel read exactly like no model tunnel at all, on the
	// one stream internal/api/healthz.go delegates coverage reporting to. So
	// account for it where the sink ALREADY accounts for decision records it
	// could not deliver: decisionSink.dropped feeds the periodic synthetic
	// `egress.decisions.dropped:<n>` summary (reportDropped/droppedSummaryLog)
	// and close()'s "closed with N dropped records". A blind row we refuse to
	// emit IS an unrecorded decision, so it belongs on that counter — one
	// mechanism, no second counter, no new audit string — plus an operator-side
	// log line naming the host and the cap, which a count cannot carry.
	if len(p.blindHosts) >= maxBlindHosts {
		p.blindMu.Unlock()
		p.sink.dropped.Add(1)
		slog.Warn("llm.scan.blind coverage suppressed: per-run blind-host cap reached",
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
			Action:   "blind",
		},
	})
}
