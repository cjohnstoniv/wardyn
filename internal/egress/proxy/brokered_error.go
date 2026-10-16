// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
)

// TRUST BOUNDARY: this file is the one place the proxy words a refusal for an
// LLM SDK. An SDK hands a plain-text body straight to a JSON parser and reports
// "JSON Parse error" with the cause lost, so every refusal on an Anthropic or
// OpenAI route (the brokered /wardyn/llm/* routes and a terminated tunnel to an
// LLM host) is that provider's own error shape, carrying a fixed reason and the
// host. The message is built from the same masked, topology-redacted pieces as
// httpError's body, never a raw error.

// The fixed reasons a refusal carries. Other lanes and the docs name them, so
// they are not renamed.
const (
	reasonEgressDenied          = "egress_denied"
	reasonTLSUntrusted          = "tls_untrusted"
	reasonUpstreamUnreachable   = "upstream_unreachable"
	reasonCredentialUnavailable = "credential_unavailable"
	reasonUpstreamError         = "upstream_error"
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

// shapeOfHost is the shape for a request to host: a gateway or mirror of an
// LLM vendor answers in that vendor's shape.
func (p *Proxy) shapeOfHost(host string) errShape { return shapeOfChannel(p.channelForHost(host)) }

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

// reasonStatus is the HTTP status a reason is answered with.
func reasonStatus(reason string) int {
	if reason == reasonEgressDenied {
		return http.StatusForbidden
	}
	return http.StatusBadGateway
}

// reasonHint is the fix an operator or developer reads after the host.
func reasonHint(reason, ruleSource string) string {
	switch reason {
	case reasonEgressDenied:
		if ruleSource == "builtin:private-ip" {
			return "declare it in internal_hosts"
		}
		return "add it to the egress allowlist"
	case reasonTLSUntrusted:
		return "the runner or daemon needs WARDYN_TRUSTED_CA_FILE"
	case reasonUpstreamUnreachable:
		return "the sandbox's proxy could not connect to it"
	case reasonCredentialUnavailable:
		return "the model credential could not be refreshed"
	}
	return "the upstream answered with an error"
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
// {"type":"error","error":{"type","message"}} or OpenAI's
// {"error":{"message","type","code"}}. extra fields are merged at the top level
// (the {"wardyn":...} fields older clients read).
func writeProviderError(w http.ResponseWriter, shape errShape, status int, reason, message string, extra map[string]any) {
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
	w.Header().Set(egressHeaderWardynReason, reason)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	out, _ := json.Marshal(body)
	_, _ = w.Write(out)
}

// writeBrokeredFault answers a failed leg on an LLM lane in the shape's body.
// err is masked and topology-redacted before it reaches the message.
func (p *Proxy) writeBrokeredFault(w http.ResponseWriter, shape errShape, host string, f brokeredFault, err error) {
	reason := f.reason
	if reason == "" {
		reason = classifyUpstreamErr(err)
	}
	status := reasonStatus(reason)
	cause := ""
	if reason != reasonEgressDenied {
		cause = p.dialFailureCause(err)
	}
	writeProviderError(w, shape, status, reason,
		brokeredMessage(reason, p.hostPortLabel(host, f.port), f.ruleSource, reasonHint(reason, f.ruleSource), cause), nil)
}

// vetFault is the fault for a failed egressTarget: a guard refusal is the
// range rule (egress_denied), a resolver fault or any other failure is not.
func vetFault(port int, ruleSource string, err error) brokeredFault {
	f := brokeredFault{port: port, ruleSource: ruleSource}
	var blocked *hostBlockedError
	switch {
	case errors.As(err, &blocked):
		f.reason, f.ruleSource = reasonEgressDenied, "builtin:private-ip"
	case errors.Is(err, errHostUnresolved):
		f.reason, f.ruleSource = reasonUpstreamUnreachable, "builtin:resolve-failed"
	default:
		f.reason = reasonUpstreamError
	}
	return f
}

// httpErrorShaped is httpError for a site that never serves an AWS endpoint: an
// LLM vendor's host gets its provider's error body, any other host the plain
// text.
func (p *Proxy) httpErrorShaped(w http.ResponseWriter, host, plainMsg string, err error, status int, f brokeredFault) {
	if shape := p.shapeOfHost(host); shape != shapePlain {
		p.writeBrokeredFault(w, shape, host, f, err)
		return
	}
	p.httpError(w, plainMsg, err, status)
}
