// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
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
	// A request the run's egress audit could not record is not sent: its target
	// is too long for one record, or the audit spool cannot take another.
	reasonAuditRecordTooLarge = "audit_record_too_large"
	reasonAuditUnavailable    = "audit_spool_unavailable"
	// The upstream answered HTTP/2 on a connection that negotiated HTTP/1.1.
	reasonProtocolMismatch = "upstream_protocol_mismatch"
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

// shapeOfHost is the shape for a request to host when no route names the
// vendor: an AWS endpoint answers in the AWS SDKs' shape, and a gateway or
// mirror of one LLM vendor in that vendor's. A gateway both vendors share has
// no shape of its own.
func (p *Proxy) shapeOfHost(host string) errShape {
	if isAWSLane(host) {
		return shapeAWS
	}
	if vendor, ok := p.gatewayVendorOf(host); ok {
		return shapeOfVendor(vendor)
	}
	return shapeOfChannel(p.channelForHost(host))
}

// routeShapeKey carries the shape a brokered /wardyn/llm/* route answers in.
type routeShapeKey struct{}

// withRouteShape marks r as a brokered route's request: its route names the
// SDK reading the answer, whichever host it is forwarded to.
func withRouteShape(r *http.Request, s errShape) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), routeShapeKey{}, s))
}

// shapeOf is the shape for r to host: the brokered route's when r came in on
// one (Claude Code and Codex may share one gateway host), else the host's.
func (p *Proxy) shapeOf(r *http.Request, host string) errShape {
	if s, ok := r.Context().Value(routeShapeKey{}).(errShape); ok {
		return routeShape(s, host)
	}
	return p.shapeOfHost(host)
}

// routeShape is the shape of a brokered route's answer when it forwards to
// host: the route's own, except that an AWS host keeps the AWS shape.
func routeShape(route errShape, host string) errShape {
	if isAWSLane(host) {
		return shapeAWS
	}
	return route
}

func shapeOfVendor(vendor string) errShape {
	switch vendor {
	case anthropicHost:
		return shapeAnthropic
	case openaiHost:
		return shapeOpenAI
	}
	return shapePlain
}

// gatewayVendorOf reports whether host is a configured gateway's, and the
// vendor host it fronts: "" when both vendors share it, since then only the
// request's route can say which.
func (p *Proxy) gatewayVendorOf(host string) (vendor string, ok bool) {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	for v, u := range p.llmUpstreams {
		if !sameHost(u.host, h) {
			continue
		}
		if ok && vendor != v {
			return "", true
		}
		vendor, ok = v, true
	}
	return vendor, ok
}

// sameHost compares two host names, as parsed addresses when both are IP
// literals so that every spelling of one address (zero-compressed, mixed-case,
// IPv4-mapped, bracketed) is the same host.
func sameHost(a, b string) bool {
	if a == b {
		return true
	}
	x, xerr := netip.ParseAddr(strings.Trim(a, "[]"))
	y, yerr := netip.ParseAddr(strings.Trim(b, "[]"))
	return xerr == nil && yerr == nil && x.Unmap() == y.Unmap()
}

// gatewayFront is the vendor host a configured gateway stands for in an answer
// to the sandbox, which reaches it only through a brokered route and never
// learns its address: the vendor it fronts, else the one shape names, else
// redactedEndpoint. A host that is no gateway's is itself.
func (p *Proxy) gatewayFront(shape errShape, host string) (front string, gateway bool) {
	vendor, ok := p.gatewayVendorOf(host)
	if !ok {
		return host, false
	}
	if vendor == "" {
		switch shape {
		case shapeAnthropic:
			vendor = anthropicHost
		case shapeOpenAI:
			vendor = openaiHost
		default:
			vendor = redactedEndpoint
		}
	}
	return vendor, true
}

// brokeredFault names a refusal for the SDK-facing body: Reason "" means
// classify the dial error.
type brokeredFault struct {
	port       int
	ruleSource string
	reason     string
}

// classifyUpstreamErr maps a failed upstream leg to a reason: a certificate the
// runner does not trust, a leg that never connected, or anything else (a
// connection that broke after it opened included).
func classifyUpstreamErr(err error) string {
	var unknownAuth x509.UnknownAuthorityError
	var certInvalid x509.CertificateInvalidError
	var hostname x509.HostnameError
	if errors.As(err, &unknownAuth) || errors.As(err, &certInvalid) || errors.As(err, &hostname) {
		return reasonTLSUntrusted
	}
	// A failure on an open connection, before the answer's headers (a reset
	// while it was awaited), did connect: not "could not connect". The reason
	// follows dialStage, so it and the Cause's stage never disagree. An answer
	// that broke after its headers never reaches here.
	if dialStage(err) == "after connect" {
		return reasonUpstreamError
	}
	return reasonUpstreamUnreachable
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
		case ruleSource == ruleSourcePort:
			return portRuleDetail
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
	case http.StatusBadRequest, http.StatusRequestURITooLong:
		return "ValidationException"
	case http.StatusUnauthorized:
		return "UnauthorizedException"
	case http.StatusForbidden:
		return "AccessDeniedException"
	case http.StatusNotFound:
		return "ResourceNotFoundException"
	case http.StatusServiceUnavailable:
		return "ServiceUnavailableException"
	}
	return "InternalServerException"
}

// hostPortLabel is the destination as the SDK's user reads it. A configured
// gateway is named by the vendor it fronts (gatewayFront), never by its own
// address; the masked cause after it has the gateway's host redacted too
// (gatewayPatterns), and so does X-Wardyn-Host on a shaped deny.
func (p *Proxy) hostPortLabel(shape errShape, host string, port int) string {
	if front, ok := p.gatewayFront(shape, host); ok {
		return front + " (via the configured gateway)"
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
	case http.StatusBadRequest, http.StatusRequestURITooLong:
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
		brokeredMessage(reason, p.hostPortLabel(shape, host, f.port), f.ruleSource, faultHint(host, reason, f.ruleSource, err), cause), nil)
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

// httpErrorShaped is httpError for a brokered site: shape (shapeOf) is the
// provider's error body, shapePlain the plain text.
func (p *Proxy) httpErrorShaped(w http.ResponseWriter, shape errShape, host, plainMsg string, err error, status int, f brokeredFault) {
	if shape != shapePlain {
		p.writeBrokeredFault(w, shape, host, f, err)
		return
	}
	p.httpError(w, plainMsg, err, status)
}
