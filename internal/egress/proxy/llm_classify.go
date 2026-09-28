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

// classifyAnthropicLLM decides how a request to the Anthropic route is
// inspected. count_tokens shares the Messages schema, so it uses the same
// extractor; every other body-bearing request is marked uninspected rather
// than silently allowed.
func classifyAnthropicLLM(method, rest string) int {
	// Only a bodiless method (GET/HEAD/DELETE/…) is quiet; the named arms below
	// are POST-only, so a PUT/PATCH to the same path falls to the fail-closed
	// default rather than being treated as scanned.
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
		// Fail-closed: the enumerated arms above cannot be trusted to cover the
		// vendor's whole content-upload surface (the sandbox forwards <rest>
		// verbatim, and the vendor adds endpoints without asking us), so an
		// unnamed POST is marked uninspected rather than silently allowed — e.g.
		// a multipart upload to POST /v1/files would otherwise carry the
		// brokered credential unscanned with no audit trail.
		return scanOpaque
	}
}

// classifyOpenAILLM decides how a request to the OpenAI route is inspected.
// chat/completions is scanned with the openai.chat extractor; responses and
// embeddings carry text in shapes we do not parse yet, so they are honestly
// marked uninspected rather than silently allowed.
func classifyOpenAILLM(method, rest string) int {
	// Same two rules as classifyAnthropicLLM: bodiless is quiet, and the named
	// arm is POST-only.
	if !bodyBearingMethod(method) {
		return scanNone
	}
	r := strings.Trim(rest, "/")
	switch {
	case method == http.MethodPost && (r == "chat/completions" || strings.HasSuffix(r, "/chat/completions")):
		return scanMessages
	default:
		// Fail-closed default, same rule as classifyAnthropicLLM: OpenAI's own
		// spec has other upload endpoints (/v1/files, /v1/audio/*) an allowlist
		// could miss, and a suffix-only arm could miss an endpoint's bare
		// spelling too (no gateway prefix configured), so anything unnamed is
		// marked uninspected rather than silently forwarded.
		return scanOpaque
	}
}

// isLLMHost reports whether host is a recognised model-API upstream (used to
// emit honest opaque-tunnel coverage for CONNECT traffic). A method, not a
// free function, so a configured internal gateway's host counts too.
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
// us-gov-west-1, ap-southeast-4). It starts with two letters on purpose, to
// separate a region from a customer-squattable S3 label (s3, s3-us-west-2)
// inside amazonaws.com; see isBedrockHost.
var awsRegionLabel = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-\d+$`)

// isBedrockHost reports whether h (already lowercased, trailing dot trimmed)
// is an AWS Bedrock data-plane or control-plane endpoint: the public regional
// form, or the PrivateLink/VPC-endpoint form
// `vpce-<id>[-<az>].bedrock-runtime.<region>.vpce.amazonaws.com` (also
// hyphen-glued), which contains but does not start with the service name, so a
// prefix match alone would miss private-endpoint model traffic.
//
// Security (read before loosening): NOT a substring match — every arm is
// anchored on AWS-owned DNS an attacker cannot register, since a match here
// routes the body through the unscanned inspectLLM path instead of
// inspectForwardBody. A look-alike domain or the legacy S3 virtual-host
// (attacker-chosen bucket names) must stay out.
func isBedrockHost(h string) bool {
	labels := strings.Split(h, ".")
	// Public: <service>.<region>.amazonaws.com, plus the dual-stack
	// <service>.<region>.api.aws form; both are AWS-owned zones.
	if len(labels) == 4 && bedrockServiceLabel(labels[0]) && awsRegionLabel.MatchString(labels[1]) &&
		((labels[2] == "amazonaws" && labels[3] == "com") || (labels[2] == "api" && labels[3] == "aws")) {
		return true
	}
	// PrivateLink: the bedrock service rides a middle label, under AWS's own
	// vpce zone.
	if !strings.HasSuffix(h, ".vpce.amazonaws.com") {
		return false
	}
	for _, l := range labels {
		if bedrockServiceLabel(l) {
			return true
		}
		// Hyphen-glued: `vpce-<id>-bedrock-agent-runtime`; must start at a
		// hyphen boundary so `notbedrock-runtime` does not match.
		if i := strings.Index(l, "-bedrock"); i > 0 && bedrockServiceLabel(l[i+1:]) {
			return true
		}
	}
	return false
}

// bedrockServiceLabel reports whether l is one of the Bedrock service labels
// AWS publishes in its service-endpoint reference
// (https://docs.aws.amazon.com/general/latest/gr/bedrock.html), with or
// without the `-fips` variant.
//
// The enumeration must cover every published label, including each `-fips`
// variant and the whole agent family: missing one leaves a CONNECT to that
// endpoint opaque AND unflagged (no `llm.scan.bypass` row), reading as "no
// model tunnel happened" in the audit trail. It is exhaustive-by-name rather
// than `strings.HasPrefix(l, "bedrock")`, since a prefix test would admit any
// future AWS-adjacent label as model traffic without a re-read of the
// security note on isBedrockHost.
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
