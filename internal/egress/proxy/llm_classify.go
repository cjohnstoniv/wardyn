// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
)

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
// see the default arm's note above.
func classifyAnthropicLLM(method, rest string) int {
	// The quiet answer belongs to methods that carry NO body (GET/HEAD/DELETE/…),
	// not to "anything that is not a POST" (bodyBearingMethod is the
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
		// Fail-closed default: every OTHER POST on this route is
		// uninspected, not quiet — the enumerated arms above cannot be trusted to
		// cover the vendor's whole content-upload surface, because the SANDBOX
		// picks the whole suffix (handleLocalRoute dispatches on a bare prefix
		// match and forwards <rest> verbatim) and the VENDOR adds endpoints
		// without asking us. A default that quietly fell to scanNone for anything
		// unnamed would forward a multipart upload of arbitrary user bytes (POST
		// /v1/files, documented 500 MB ceiling) with the operator's brokered
		// credential, unscanned, carrying NO scan block at all — a row
		// indistinguishable in audit from GET /v1/models — and would let a strict
		// operator's block + on_scanner_error=block control be bypassed by
		// choosing an unnamed suffix (refused as `batches`, forwarded as
		// `files`). So the default is the honest answer — "prompt-bearing but no
		// extractor yet" — and a new arm above is what earns silence.
		//
		// The same is true of the VERB. `PUT /v1/messages` is
		// not a documented Anthropic call, so it lands here rather than on the
		// scanMessages arm — uninspected and refused under fail-closed blocking
		// rather than silently forwarded.
		return scanOpaque
	}
}

// classifyOpenAILLM decides how a request to the OpenAI route is inspected.
// chat/completions is scanned with the openai.chat extractor; responses and
// embeddings carry prompt/input text in shapes we do not parse yet, so they are
// honestly marked uninspected rather than silently allowed.
func classifyOpenAILLM(method, rest string) int {
	// Same two rules as classifyAnthropicLLM: only a bodiless method is quiet
	// and the named arm is POST-only so any other body-bearing verb
	// on the same path is an unrecognised call, not a scanned one.
	if !bodyBearingMethod(method) {
		return scanNone
	}
	r := strings.Trim(rest, "/")
	switch {
	case method == http.MethodPost && (r == "chat/completions" || strings.HasSuffix(r, "/chat/completions")):
		return scanMessages
	default:
		// Fail-closed default, same rule as classifyAnthropicLLM: an
		// enumerated allowlist of endpoints cannot be trusted to cover the
		// vendor's whole content-upload surface. The vendor's own OpenAPI spec
		// defines POST /v1/files and the multipart
		// /v1/audio/{transcriptions,translations} uploads plus /v1/audio/speech
		// beside /responses, /embeddings and /completions — an enumeration naming
		// only the chat-adjacent endpoints would stream the rest through with the
		// brokered credential and no scan block. An enumeration also has to carry
		// each endpoint's BARE spelling (with no gateway prefix configured,
		// `rest` for POST /wardyn/llm/openai/responses is exactly "responses",
		// which a suffix-only arm would miss) — a second way the same list could
		// silently lose an endpoint. The default answers both.
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
// CONTAINS but does not START WITH the service name, so a prefix match alone
// would drop private-endpoint model traffic out of isLLMHost's opaque-tunnel
// coverage — the one call an auditor most wants to see.
//
// Security (read before loosening): this is NOT a substring match. Every arm is
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
// The enumeration must cover every AWS-published label, including each
// `-fips` variant (a FedRAMP-High workload is generally REQUIRED to use one,
// and GovCloud publishes
// `bedrock-runtime-fips.us-gov-{west,east}-1.amazonaws.com`) and the whole
// agent family, including `bedrock-agent-runtime` — the InvokeAgent DATA
// plane, i.e. prompt-bearing model traffic. Missing any of them costs
// honesty, not just coverage: proxy.go emits the one-time `llm.scan.blind`
// coverage row only under isLLMHost, so an unrecognised label leaves a
// CONNECT to that endpoint opaque AND unflagged — an audit trail showing an
// egress.allow and no blind row anywhere, which reads as "no model tunnel
// happened", against a THREAT-MODEL.md that says those tunnels "stay opaque
// and flagged llm.scan.blind". It also mislabels the MITM decision row for
// such a host as corp-artifact rather than model traffic.
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
