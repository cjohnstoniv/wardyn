// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

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
	cases := []struct {
		name       string
		reason     string
		status     int
		anthType   string
		openAIType string
		hint       string
		write      func(w http.ResponseWriter, shape errShape, host string)
	}{
		{"egress_denied private-ip", reasonEgressDenied, 403, "permission_error", "permission_error", "declare it in internal_hosts",
			func(w http.ResponseWriter, shape errShape, host string) {
				p.denyEgress(w, host, 443, &egress.DecisionLog{Decision: egress.Deny, RuleSource: "builtin:private-ip"}, false, shape)
			}},
		{"egress_denied allowlist", reasonEgressDenied, 403, "permission_error", "permission_error", "add it to the egress allowlist",
			func(w http.ResponseWriter, shape errShape, host string) {
				p.denyEgress(w, host, 443, &egress.DecisionLog{Decision: egress.Deny, RuleSource: "policy:default-deny"}, false, shape)
			}},
		{"tls_untrusted unknown authority", reasonTLSUntrusted, 502, "api_error", "api_error", "WARDYN_TRUSTED_CA_FILE",
			func(w http.ResponseWriter, shape errShape, host string) {
				p.writeBrokeredFault(w, shape, host, brokeredFault{port: 443, ruleSource: "builtin:dial-failed"}, x509Err(x509.UnknownAuthorityError{}))
			}},
		{"tls_untrusted invalid cert", reasonTLSUntrusted, 502, "api_error", "api_error", "WARDYN_TRUSTED_CA_FILE",
			func(w http.ResponseWriter, shape errShape, host string) {
				p.writeBrokeredFault(w, shape, host, brokeredFault{port: 443, ruleSource: "builtin:dial-failed"}, x509Err(x509.CertificateInvalidError{Reason: x509.Expired}))
			}},
		{"tls_untrusted hostname", reasonTLSUntrusted, 502, "api_error", "api_error", "WARDYN_TRUSTED_CA_FILE",
			func(w http.ResponseWriter, shape errShape, host string) {
				p.writeBrokeredFault(w, shape, host, brokeredFault{port: 443, ruleSource: "builtin:dial-failed"}, x509Err(x509.HostnameError{Host: host}))
			}},
		{"upstream_unreachable", reasonUpstreamUnreachable, 502, "api_error", "api_error", "could not connect",
			func(w http.ResponseWriter, shape errShape, host string) {
				p.writeBrokeredFault(w, shape, host, brokeredFault{port: 443, ruleSource: "builtin:dial-failed"}, dialErr)
			}},
		{"credential_unavailable", reasonCredentialUnavailable, 502, "api_error", "api_error", "credential could not be refreshed",
			func(w http.ResponseWriter, shape errShape, host string) {
				p.writeBrokeredFault(w, shape, host, brokeredFault{port: 443, ruleSource: ruleSourceLLM, reason: reasonCredentialUnavailable}, errors.New("injection status 424"))
			}},
		{"upstream_error", reasonUpstreamError, 502, "api_error", "api_error", "upstream answered with an error",
			func(w http.ResponseWriter, shape errShape, host string) {
				p.writeBrokeredFault(w, shape, host, brokeredFault{port: 443, ruleSource: "builtin:dial-failed"}, errors.New("boom"))
			}},
	}
	for _, c := range cases {
		for _, route := range []struct {
			name  string
			shape errShape
			host  string
		}{{"anthropic", shapeAnthropic, anthropicHost}, {"openai", shapeOpenAI, openaiHost}} {
			t.Run(c.name+"/"+route.name, func(t *testing.T) {
				if got := p.shapeOfHost(route.host); got != route.shape {
					t.Fatalf("shapeOfHost(%s) = %v, want %v", route.host, got, route.shape)
				}
				rec := httptest.NewRecorder()
				c.write(rec, route.shape, route.host)
				if rec.Code != c.status {
					t.Errorf("status = %d, want %d", rec.Code, c.status)
				}
				if got := rec.Header().Get("X-Wardyn-Reason"); got != c.reason {
					t.Errorf("X-Wardyn-Reason = %q, want %q", got, c.reason)
				}
				v := decodeSDK(t, route.shape, rec)
				wantType := c.anthType
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
			if v := decodeSDK(t, route.shape, rec); !strings.Contains(v.message, "(policy:method)") {
				t.Errorf("message %q lacks the rule source", v.message)
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
