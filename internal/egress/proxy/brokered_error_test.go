// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The SDK error structs the two providers' clients decode, mirrored locally
// (field names are the SDKs' own).
type anthropicSDKError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type openAISDKError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// sdkView is what either SDK reads out of a refusal.
type sdkView struct{ typ, message, code string }

func decodeSDK(t *testing.T, shape errShape, rec *httptest.ResponseRecorder) sdkView {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json (body %q)", ct, rec.Body.String())
	}
	if shape == shapeAWS {
		// The AWS JSON protocol: __type and message, the type repeated in x-amzn-errortype.
		var e struct {
			Type    string `json:"__type"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Type == "" || e.Message == "" ||
			rec.Header().Get("x-amzn-errortype") != e.Type {
			t.Fatalf("body %q (x-amzn-errortype %q) is not an AWS error: %+v err=%v",
				rec.Body.String(), rec.Header().Get("x-amzn-errortype"), e, err)
		}
		return sdkView{typ: e.Type, message: e.Message}
	}
	if shape == shapeAnthropic {
		var e anthropicSDKError
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Type != "error" || e.Error.Type == "" || e.Error.Message == "" {
			t.Fatalf("body %q is not an Anthropic error: %+v err=%v", rec.Body.String(), e, err)
		}
		return sdkView{typ: e.Error.Type, message: e.Error.Message}
	}
	var e openAISDKError
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Error.Message == "" || e.Error.Type == "" || e.Error.Code == "" {
		t.Fatalf("body %q is not an OpenAI error: %+v err=%v", rec.Body.String(), e, err)
	}
	return sdkView{typ: e.Error.Type, message: e.Error.Message, code: e.Error.Code}
}

func TestBrokeredErrorsParseAsTheProviderSDKError(t *testing.T) {
	p := attributedProxy(nil)
	x509Err := func(inner error) error {
		return &url.Error{Op: "Post", URL: "https://x", Err: fmt.Errorf("tls: failed to verify certificate: %w", inner)}
	}
	dialErr := &url.Error{Op: "Post", URL: "https://x", Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}}
	// status and awsStatus: the Anthropic/OpenAI routes and the AWS lane. hint
	// is in the message, notHint must not be (a fix that does not apply).
	cases := []struct {
		name       string
		reason     string
		status     int
		awsStatus  int
		anthType   string
		openAIType string
		hint       string
		notHint    string
		write      func(w http.ResponseWriter, shape errShape, host string)
	}{
		{"egress_denied private-ip", reasonEgressDenied, 403, 403, "permission_error", "permission_error", "declare it in internal_hosts", "allowlist",
			func(w http.ResponseWriter, shape errShape, host string) {
				p.denyEgress(w, host, 443, &egress.DecisionLog{Decision: egress.Deny, RuleSource: "builtin:private-ip"}, false, shape)
			}},
		{"egress_denied allowlist", reasonEgressDenied, 403, 403, "permission_error", "permission_error", "add it to the egress allowlist", "internal_hosts",
			func(w http.ResponseWriter, shape errShape, host string) {
				p.denyEgress(w, host, 443, &egress.DecisionLog{Decision: egress.Deny, RuleSource: "policy:default-deny"}, false, shape)
			}},
		{"egress_denied method", reasonEgressDenied, 403, 403, "permission_error", "permission_error", "(policy:method) — the run's policy does not allow this HTTP method", "allowlist",
			func(w http.ResponseWriter, shape errShape, host string) {
				p.denyEgress(w, host, 443, &egress.DecisionLog{Decision: egress.Deny, RuleSource: "policy:method"}, false, shape)
			}},
		{"egress_denied vet private range", reasonEgressDenied, 403, 403, "permission_error", "permission_error", "declare it in internal_hosts", "allowlist",
			func(w http.ResponseWriter, shape errShape, host string) {
				err := &hostBlockedError{kind: blockPrivate, msg: "host denied: private address"}
				p.writeBrokeredFault(w, shape, host, vetFault(443, ruleSourceLLMMITM, err), err)
			}},
		{"tls_untrusted unknown authority", reasonTLSUntrusted, 403, 403, "permission_error", "permission_error", "trusted CA: the runner or daemon needs WARDYN_TRUSTED_CA_FILE", "not name",
			func(w http.ResponseWriter, shape errShape, host string) {
				p.writeBrokeredFault(w, shape, host, brokeredFault{port: 443, ruleSource: "builtin:dial-failed"}, x509Err(x509.UnknownAuthorityError{}))
			}},
		{"tls_untrusted invalid cert", reasonTLSUntrusted, 403, 403, "permission_error", "permission_error", "certificate is not valid", "trusted CA",
			func(w http.ResponseWriter, shape errShape, host string) {
				p.writeBrokeredFault(w, shape, host, brokeredFault{port: 443, ruleSource: "builtin:dial-failed"}, x509Err(x509.CertificateInvalidError{Reason: x509.Expired}))
			}},
		{"tls_untrusted hostname", reasonTLSUntrusted, 403, 403, "permission_error", "permission_error", "certificate does not name this host", "trusted CA",
			func(w http.ResponseWriter, shape errShape, host string) {
				p.writeBrokeredFault(w, shape, host, brokeredFault{port: 443, ruleSource: "builtin:dial-failed"}, x509Err(x509.HostnameError{Host: host}))
			}},
		{"upstream_unreachable", reasonUpstreamUnreachable, 502, 500, "api_error", "api_error", "could not connect", "answered",
			func(w http.ResponseWriter, shape errShape, host string) {
				p.writeBrokeredFault(w, shape, host, brokeredFault{port: 443, ruleSource: "builtin:dial-failed"}, dialErr)
			}},
		{"credential_unavailable", reasonCredentialUnavailable, 502, 401, "api_error", "api_error", "credential could not be refreshed", "answered",
			func(w http.ResponseWriter, shape errShape, host string) {
				p.writeBrokeredFault(w, shape, host, brokeredFault{port: 443, ruleSource: ruleSourceLLM, reason: reasonCredentialUnavailable}, errors.New("injection status 424"))
			}},
		{"upstream_error", reasonUpstreamError, 502, 500, "api_error", "api_error", "the request to it failed", "answered",
			func(w http.ResponseWriter, shape errShape, host string) {
				p.writeBrokeredFault(w, shape, host, brokeredFault{port: 443, ruleSource: "builtin:dial-failed"}, errors.New("boom"))
			}},
		{"upstream_error gateway vet", reasonUpstreamError, 502, 500, "api_error", "api_error", "configured gateway's address was refused", "answered",
			func(w http.ResponseWriter, shape errShape, host string) {
				p.writeBrokeredFault(w, shape, host, vetFault(443, ruleSourceGatewayVetFailed, errGatewayVet), errGatewayVet)
			}},
	}
	for _, c := range cases {
		for _, route := range []struct {
			name  string
			shape errShape
			host  string
		}{{"anthropic", shapeAnthropic, anthropicHost}, {"openai", shapeOpenAI, openaiHost},
			{"bedrock", shapeAWS, "bedrock-runtime.us-east-1.amazonaws.com"}, {"sso portal", shapeAWS, awsHost}} {
			t.Run(c.name+"/"+route.name, func(t *testing.T) {
				if got := p.shapeOfHost(route.host); got != route.shape {
					t.Fatalf("shapeOfHost(%s) = %v, want %v", route.host, got, route.shape)
				}
				rec := httptest.NewRecorder()
				c.write(rec, route.shape, route.host)
				wantStatus := c.status
				if route.shape == shapeAWS {
					wantStatus = c.awsStatus
				}
				if rec.Code != wantStatus {
					t.Errorf("status = %d, want %d", rec.Code, wantStatus)
				}
				if got := rec.Header().Get("X-Wardyn-Reason"); got != c.reason {
					t.Errorf("X-Wardyn-Reason = %q, want %q", got, c.reason)
				}
				v := decodeSDK(t, route.shape, rec)
				wantType := c.anthType
				if route.shape == shapeAWS {
					wantType = awsErrorType(c.awsStatus)
				}
				if route.shape == shapeOpenAI {
					wantType = c.openAIType
					if v.code != c.reason {
						t.Errorf("code = %q, want %q", v.code, c.reason)
					}
				}
				if v.typ != wantType {
					t.Errorf("type = %q, want %q", v.typ, wantType)
				}
				for _, want := range []string{"wardyn: " + c.reason + ": " + route.host + ":443", c.hint} {
					if !strings.Contains(v.message, want) {
						t.Errorf("message %q lacks %q", v.message, want)
					}
				}
				if strings.Contains(v.message, c.notHint) {
					t.Errorf("message %q carries a fix that does not apply (%q)", v.message, c.notHint)
				}
			})
		}
	}
}

func TestClassifyUpstreamErr(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"unknown authority", fmt.Errorf("x: %w", x509.UnknownAuthorityError{}), reasonTLSUntrusted},
		{"dial", &net.OpError{Op: "dial", Err: errors.New("refused")}, reasonUpstreamUnreachable},
		{"upstream proxy", errors.New("upstream CONNECT refused"), reasonUpstreamUnreachable},
		{"other", errors.New("boom"), reasonUpstreamError},
	} {
		if got := classifyUpstreamErr(tc.err); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// End to end: a method the policy refuses, asked of the brokered route, comes
// back as the provider's error with the rule source in the message.
func TestBrokeredRouteDenyIsAProviderError(t *testing.T) {
	spec := types.RunPolicySpec{AllowedMethods: []string{"GET"}}
	up := newRawUpstream(t, `{}`)
	for _, route := range []struct {
		name, prefix string
		shape        errShape
	}{{"anthropic", llmAnthropicPrefix, shapeAnthropic}, {"openai", llmOpenAIPrefix, shapeOpenAI}} {
		t.Run(route.name, func(t *testing.T) {
			p, _ := llmChecksProxy(t, spec, "", up)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustLocalReq(t, http.MethodDelete, route.prefix+"v1/files/f", nil))
			if rec.Code != http.StatusForbidden || rec.Header().Get("X-Wardyn-Reason") != reasonEgressDenied {
				t.Fatalf("status %d reason %q", rec.Code, rec.Header().Get("X-Wardyn-Reason"))
			}
			v := decodeSDK(t, route.shape, rec)
			if !strings.Contains(v.message, "(policy:method) — the run's policy does not allow this HTTP method") ||
				strings.Contains(v.message, "allowlist") {
				t.Errorf("message %q lacks the rule source and the method fix", v.message)
			}
		})
	}
}

// A forward-lane CONNECT deny: JSON body plus the reason header.
func TestForwardLaneDenyIsStructured(t *testing.T) {
	p := attributedProxy(nil)
	rec := httptest.NewRecorder()
	p.writeEgressDeny(rec, "blocked.test", 443, &egress.DecisionLog{Decision: egress.Deny, RuleSource: "policy:default-deny"}, false)
	b2 := map[string]map[string]string{}
	if err := json.Unmarshal(rec.Body.Bytes(), &b2); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	w := b2["wardyn"]
	if rec.Code != 403 || w["reason"] != "egress_denied" || w["host"] != "blocked.test" || w["rule_source"] != "policy:default-deny" {
		t.Errorf("status %d body %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Wardyn-Reason") != "egress_denied" || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("headers %v", rec.Header())
	}
}

const bedrockHost = "bedrock-runtime.us-east-1.amazonaws.com"

// The adopter's own lane: an AWS SDK reads a refused inner request of the
// terminated Bedrock or SSO portal tunnel, and a brokered route to an AWS
// gateway, as a modelled 403 AccessDeniedException naming the host and rule.
func TestAWSLaneMethodDenyIsAccessDenied(t *testing.T) {
	spec := types.RunPolicySpec{AllowedMethods: []string{"GET"}}
	for _, host := range []string{bedrockHost, awsHost} {
		t.Run("tunnel/"+host, func(t *testing.T) {
			p := newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(spec)})
			rec := httptest.NewRecorder()
			p.serveMITMRequest(rec, httptest.NewRequest(http.MethodPost, "https://"+host+"/model/x/converse", nil), host, 443)
			v := decodeSDK(t, shapeAWS, rec)
			if rec.Code != http.StatusForbidden || v.typ != "AccessDeniedException" ||
				!strings.Contains(v.message, "wardyn: egress_denied: "+host+":443 (policy:method) — the run's policy does not allow this HTTP method") {
				t.Errorf("status %d type %q message %q", rec.Code, v.typ, v.message)
			}
		})
	}
	t.Run("brokered route to an AWS gateway", func(t *testing.T) {
		p, _ := llmChecksProxy(t, spec, "https://"+bedrockHost+"/", newRawUpstream(t, `{}`))
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"v1/messages", nil))
		if v := decodeSDK(t, shapeAWS, rec); rec.Code != http.StatusForbidden || v.typ != "AccessDeniedException" ||
			!strings.Contains(v.message, "(policy:method)") {
			t.Errorf("status %d type %q message %q", rec.Code, v.typ, v.message)
		}
	})
}

// The SSO portal resolving into a private range (CGNAT, internal_hosts not
// declared) is egress_denied naming the portal, the rule source its decision
// row carries, and internal_hosts: not a 500 with only the masked cause.
func TestAWSLanePortalInAPrivateRangeIsEgressDenied(t *testing.T) {
	buf := &bytes.Buffer{}
	p := newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(types.RunPolicySpec{}),
		Sink:     &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 8)},
		Resolver: fakeResolver{m: map[string][]net.IP{awsHost: ips("100.64.1.5")}},
	})
	rec := httptest.NewRecorder()
	p.serveMITMRequest(rec, httptest.NewRequest(http.MethodGet, "https://"+awsHost+"/federation/credentials", nil), awsHost, 443)
	row := lastDecision(t, buf)
	v := decodeSDK(t, shapeAWS, rec)
	want := "wardyn: egress_denied: " + awsHost + ":443 (" + row.RuleSource + ") — declare it in internal_hosts"
	if rec.Code != http.StatusForbidden || v.typ != "AccessDeniedException" || rec.Header().Get("X-Wardyn-Reason") != reasonEgressDenied ||
		!strings.HasPrefix(v.message, want) {
		t.Errorf("status %d type %q message %q, want 403 AccessDeniedException starting %q", rec.Code, v.typ, v.message, want)
	}
}

// A failed refresh on the portal lane names the portal and the rule source
// its decision row carries, in the modelled 401.
func TestAWSLaneRefreshFailureNamesThePortal(t *testing.T) {
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "resolve failed", http.StatusFailedDependency)
	}))
	defer cp.Close()
	inj := &injector{
		byHost: map[string]*injEntry{awsHost: {grantID: uuid.New(), expiresAt: time.Now().Add(-time.Hour).UnixMilli()}},
		base:   cp.URL, token: newTokenSource("tok"), client: cp.Client(),
	}
	p, buf := newLocalRouteProxy(t, cp.URL, "RUNTOK", upstreamAddr(cp), inj, nil)
	rec := httptest.NewRecorder()
	p.serveMITMRequest(rec, httptest.NewRequest(http.MethodGet, "https://"+awsHost+"/federation/credentials", nil), awsHost, 443)
	row := lastDecision(t, buf)
	v := decodeSDK(t, shapeAWS, rec)
	want := "wardyn: credential_unavailable: " + awsHost + ":443 (" + row.RuleSource + ")"
	if rec.Code != http.StatusUnauthorized || v.typ != "UnauthorizedException" || !strings.HasPrefix(v.message, want) {
		t.Errorf("status %d type %q message %q, want 401 UnauthorizedException starting %q", rec.Code, v.typ, v.message, want)
	}
}

// A brokered route whose credential refresh failed AND whose host the guard
// refuses reports the egress refusal (the fault a refreshed credential would
// still not get past), naming the host and the rule source of its row.
func TestBrokeredRefreshFailureBehindAnEgressRefusalIsEgressDenied(t *testing.T) {
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "resolve failed", http.StatusFailedDependency)
	}))
	defer cp.Close()
	buf := &bytes.Buffer{}
	p := newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(types.RunPolicySpec{}),
		Sink:     &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 8)},
		Resolver: fakeResolver{m: map[string][]net.IP{anthropicHost: ips("100.64.1.5")}},
		Injector: &injector{
			byHost: map[string]*injEntry{anthropicHost: {grantID: uuid.New(), expiresAt: time.Now().Add(-time.Hour).UnixMilli()}},
			base:   cp.URL, token: newTokenSource("tok"), client: cp.Client(),
		},
	})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"v1/messages", strings.NewReader("{}")))
	row := lastDecision(t, buf)
	v := decodeSDK(t, shapeAnthropic, rec)
	want := "wardyn: egress_denied: " + anthropicHost + ":443 (" + row.RuleSource + ") — declare it in internal_hosts"
	if rec.Code != http.StatusForbidden || !strings.HasPrefix(v.message, want) {
		t.Errorf("status %d message %q, want 403 starting %q", rec.Code, v.message, want)
	}
}

// The SDK shows the message, never the headers: a policy-decided deny on an
// LLM or AWS lane carries the attribution sentence in it.
func TestShapedDenyCarriesTheAttribution(t *testing.T) {
	const line = `This run is governed by the profile "Team A". To request a change: https://help.example.com/access`
	p := attributedProxy(teamA)
	for _, c := range []struct {
		shape errShape
		host  string
	}{{shapeAnthropic, anthropicHost}, {shapeOpenAI, openaiHost}, {shapeAWS, bedrockHost}} {
		rec := httptest.NewRecorder()
		p.denyEgress(rec, c.host, 443, &egress.DecisionLog{Decision: egress.Deny, RuleSource: "policy:method"}, false, c.shape)
		if v := decodeSDK(t, c.shape, rec); !strings.HasSuffix(v.message, "HTTP method. "+line) {
			t.Errorf("%s: message %q lacks the attribution", c.host, v.message)
		}
	}
}

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("client went away") }

// A body the proxy could not take in to inspect (a failed read, or the scan
// queue or retain budget expiring) is the provider's error on an LLM channel.
func TestScanFaultIsAProviderError(t *testing.T) {
	p := newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{})})
	for _, c := range []struct {
		channel contentscan.Channel
		shape   errShape
	}{{contentscan.ChannelAnthropicMessages, shapeAnthropic}, {contentscan.ChannelOpenAIChat, shapeOpenAI}} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", io.NopCloser(failingBody{}))
		_, _, release, blocked := p.scanBufferedBody(rec, req, c.channel, "read llm body", func(egress.Decision, string, *egress.ScanSummary) {})
		release()
		v := decodeSDK(t, c.shape, rec)
		if !blocked || rec.Code != http.StatusBadRequest || !strings.Contains(v.message, "read llm body: client went away") {
			t.Errorf("read failure: blocked %v status %d message %q", blocked, rec.Code, v.message)
		}
		rec = httptest.NewRecorder()
		p.scanFault(rec, c.channel, "read llm body", context.DeadlineExceeded, http.StatusBadGateway)
		if v := decodeSDK(t, c.shape, rec); rec.Code != http.StatusBadGateway || rec.Header().Get("X-Wardyn-Reason") != reasonNotInspected ||
			!strings.Contains(v.message, "deadline exceeded") {
			t.Errorf("queue expiry: status %d message %q", rec.Code, v.message)
		}
	}
}
