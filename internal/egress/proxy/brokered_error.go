// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
)

// TRUST BOUNDARY: this file is the one place the proxy words a refusal for an
// LLM SDK. An SDK hands a plain-text body straight to a JSON parser and reports
// "JSON Parse error" with the cause lost, so every refusal on an Anthropic,
// OpenAI or AWS route (the brokered /wardyn/llm/* routes and a terminated
// tunnel to an LLM, Bedrock or SSO portal host) is that provider's own error
// shape, carrying a fixed reason and the host. The message is built from the
// same masked, topology-redacted pieces as httpError's body, never a raw error.

// The fixed reasons a refusal carries. docs/UI-SANDBOXES.md ("A refusal an
// SDK reads") lists them with their status per shape, so they are not renamed.
const (
	reasonEgressDenied          = "egress_denied"
	reasonTLSUntrusted          = "tls_untrusted"
	reasonUpstreamUnreachable   = "upstream_unreachable"
	reasonCredentialUnavailable = "credential_unavailable"
	reasonUpstreamError         = "upstream_error"
	// Refusals of the request itself, not of its connection: a path the route
	// will not forward, a body content inspection blocked, and a body the proxy
	// could not take in to inspect, so did not forward.
	reasonUnsupportedPath = "unsupported_path"
	reasonContentBlocked  = "llm_content_blocked"
	reasonNotInspected    = "request_not_inspected"
)

// egressHeaderWardynReason carries the fixed reason on the forward lane, where
// a CONNECT client discards the body.
const egressHeaderWardynReason = "X-Wardyn-Reason"

// errShape is which error body a refusal is written in.
type errShape int

const (
	shapePlain errShape = iota // forward lane: JSON {"wardyn":{...}}, or text where no JSON is promised
	shapeAnthropic
	shapeOpenAI
	shapeAWS // Bedrock or the SSO portal: writeAWSSDKError's {"__type","message"}
)

func shapeOfChannel(ch contentscan.Channel) errShape {
	switch ch {
	case contentscan.ChannelAnthropicMessages:
		return shapeAnthropic
	case contentscan.ChannelOpenAIChat:
		return shapeOpenAI
	}
	return shapePlain
}

// shapeOfHost is the shape for a request to host: an AWS endpoint answers in
// the AWS SDKs' shape, and a gateway or mirror of an LLM vendor in that
// vendor's.
func (p *Proxy) shapeOfHost(host string) errShape {
	if isAWSLane(host) {
		return shapeAWS
	}
	return shapeOfChannel(p.channelForHost(host))
}

// shapeFor is the shape for a request to host on a route of channel ch: the
// host's own shape (AWS, or the vendor a gateway fronts) wins, so one route
// answers in one shape, else the route's channel's.
func (p *Proxy) shapeFor(host string, ch contentscan.Channel) errShape {
	if s := p.shapeOfHost(host); s != shapePlain {
		return s
	}
	return shapeOfChannel(ch)
}

// brokeredFault names a refusal for the SDK-facing body: Reason "" means
// classify the dial error.
type brokeredFault struct {
	port       int
	ruleSource string
	reason     string
}

// classifyUpstreamErr maps a failed upstream leg to a reason: a certificate the
// runner does not trust, a leg that never connected, or anything else.
func classifyUpstreamErr(err error) string {
	var unknownAuth x509.UnknownAuthorityError
	var certInvalid x509.CertificateInvalidError
	var hostname x509.HostnameError
	if errors.As(err, &unknownAuth) || errors.As(err, &certInvalid) || errors.As(err, &hostname) {
		return reasonTLSUntrusted
	}
	var netErr net.Error
	switch dialStage(err) {
	case "tcp dial", "upstream proxy connect", "tls handshake":
		return reasonUpstreamUnreachable
	}
	if errors.As(err, &netErr) {
		return reasonUpstreamUnreachable
	}
	return reasonUpstreamError
}

// reasonStatus is the HTTP status a reason is answered with. A refusal that
// answers the same on every retry is a 403, since no SDK retries a 4xx other
// than 408, 409 and 429 (the Anthropic and OpenAI SDKs retry those and 5xx,
// the AWS SDKs 5xx and throttling): egress_denied, and tls_untrusted, a
// certificate the proxy will not trust however often it is asked. On the AWS
// lane a credential that could not be refreshed is 401 (writeSSOUnauthorized's
// precedent) and any other fault 500, AWS's own InternalServerException.
func reasonStatus(shape errShape, reason string) int {
	switch {
	case reason == reasonEgressDenied || reason == reasonTLSUntrusted:
		return http.StatusForbidden
	case shape != shapeAWS:
		return http.StatusBadGateway
	case reason == reasonCredentialUnavailable:
		return http.StatusUnauthorized
	}
	return http.StatusInternalServerError
}

// faultHint is the fix an operator or developer reads after host. err is the
// failure behind the reason, nil for a policy deny.
func faultHint(host, reason, ruleSource string, err error) string {
	switch reason {
	case reasonEgressDenied:
		kind := blockKindOf(err)
		switch {
		case ruleSource == "policy:method":
			return "the run's policy does not allow this HTTP method"
		case ruleSource == "builtin:private-ip" || kind == blockPrivate:
			return "declare it in internal_hosts"
		case kind != blockNone:
			return "its address is in a range no internal_hosts entry lifts"
		}
		return "add it to the egress allowlist"
	case reasonTLSUntrusted:
		var hostname x509.HostnameError
		var invalid x509.CertificateInvalidError
		switch {
		case errors.As(err, &hostname):
			return "its certificate does not name this host"
		case errors.As(err, &invalid):
			return "its certificate is not valid (expired, not yet valid, or not for this use)"
		}
		return "its certificate is not from a trusted CA: the runner or daemon needs WARDYN_TRUSTED_CA_FILE"
	case reasonUpstreamUnreachable:
		return "the sandbox's proxy could not connect to it"
	case reasonCredentialUnavailable:
		// On the portal the credential is the owner's SSO session the proxy injects: a
		// sign-in inside the sandbox is not the fix, so the hint must not read like one.
		if isAWSSSOPortalHost(strings.TrimSuffix(strings.ToLower(host), ".")) {
			return "Wardyn could not refresh the SSO session it injects for this host; signing in inside the sandbox does not change it"
		}
		return "Wardyn could not refresh the brokered credential for this host"
	}
	if errors.Is(err, errGatewayVet) {
		return "the configured gateway's address was refused or did not resolve"
	}
	return "the request to it failed"
}

// awsErrorType is the modelled AWS error a status is answered with.
func awsErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "ValidationException"
	case http.StatusUnauthorized:
		return "UnauthorizedException"
	case http.StatusForbidden:
		return "AccessDeniedException"
	case http.StatusNotFound:
		return "ResourceNotFoundException"
	}
	return "InternalServerException"
}

// hostPortLabel is the destination as the SDK's user reads it. A configured
// gateway is named by the vendor it fronts, never by its own address.
func (p *Proxy) hostPortLabel(host string, port int) string {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if vendor, ok := p.gatewayVendor[h]; ok {
		return vendor + " (via the configured gateway)"
	}
	return p.redactTopology(net.JoinHostPort(host, strconv.Itoa(port)))
}

// brokeredMessage is "wardyn: <reason>: <host:port> (<rule_source>) — <hint>",
// then the masked cause when there is one.
func brokeredMessage(reason, dest, ruleSource, hint, cause string) string {
	msg := "wardyn: " + reason + ": " + dest
	if ruleSource != "" {
		msg += " (" + ruleSource + ")"
	}
	msg += " — " + hint
	if cause != "" {
		msg += ": " + cause
	}
	return msg
}

// providerStatusType is the provider error "type" a status maps to; both
// providers spell these the same.
func providerStatusType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	}
	return "api_error"
}

// writeProviderError writes the shape's error body: Anthropic's
// {"type":"error","error":{"type","message"}}, OpenAI's
// {"error":{"message","type","code"}} or AWS's {"__type","message"}. extra
// fields are merged at the top level of the first two (the {"wardyn":...}
// fields older clients read).
func writeProviderError(w http.ResponseWriter, shape errShape, status int, reason, message string, extra map[string]any) {
	w.Header().Set(egressHeaderWardynReason, reason)
	if shape == shapeAWS {
		writeAWSSDKError(w, status, awsErrorType(status), message)
		return
	}
	typ := providerStatusType(status)
	body := map[string]any{}
	if shape == shapeOpenAI {
		openaiType := typ
		if typ == "not_found_error" {
			openaiType = "invalid_request_error"
		}
		body["error"] = map[string]string{"message": message, "type": openaiType, "code": reason}
	} else {
		body["type"] = "error"
		body["error"] = map[string]string{"type": typ, "message": message}
	}
	for k, v := range extra {
		body[k] = v
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	out, _ := json.Marshal(body)
	_, _ = w.Write(out)
}

// writeBrokeredFault answers a failed leg on an LLM or AWS lane in the shape's
// body. err is masked and topology-redacted before it reaches the message. A
// dial-shaped fault (no reason given) carries its stage, so the message ends
// with its decision row's Cause verbatim.
func (p *Proxy) writeBrokeredFault(w http.ResponseWriter, shape errShape, host string, f brokeredFault, err error) {
	reason, cause := f.reason, p.dialFailureCause(err)
	if reason == "" {
		reason, cause = classifyUpstreamErr(err), p.causeSentence(err)
	}
	status := reasonStatus(shape, reason)
	slog.Warn("proxy error returned to the sandbox", "reason", reason, "status", status, "err", cause)
	writeProviderError(w, shape, status, reason,
		brokeredMessage(reason, p.hostPortLabel(host, f.port), f.ruleSource, faultHint(host, reason, f.ruleSource, err), cause), nil)
}

// vetFault is the fault for a failed egressTarget: a guard refusal is the
// range rule (egress_denied), a resolver fault or any other failure is not.
// ruleSource stays the one the decision row carries, so an operator finds
// the row by what the message names.
func vetFault(port int, ruleSource string, err error) brokeredFault {
	f := brokeredFault{port: port, ruleSource: ruleSource, reason: reasonUpstreamError}
	switch {
	case blockKindOf(err) != blockNone:
		f.reason = reasonEgressDenied
	case errors.Is(err, errHostUnresolved):
		f.reason = reasonUpstreamUnreachable
	}
	return f
}

// httpErrorShaped is httpError for a brokered site: an LLM vendor's or AWS
// host gets its provider's error body, any other host the plain text.
func (p *Proxy) httpErrorShaped(w http.ResponseWriter, host, plainMsg string, err error, status int, f brokeredFault) {
	if shape := p.shapeOfHost(host); shape != shapePlain {
		p.writeBrokeredFault(w, shape, host, f, err)
		return
	}
	p.httpError(w, plainMsg, err, status)
}
