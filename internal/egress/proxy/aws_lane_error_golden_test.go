// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The AWS-lane error-body family (0.7.8): on the run's own SSO portal or a
// Bedrock endpoint, a dial-shaped/vet-shaped/credential-shaped refusal
// answers with a MODELLED, valid-JSON AWS SDK error instead of the
// plain-text body every other refusal gets — an AWS SDK hands plain text
// straight to a JSON parser and crashes on it (the field report this lane
// answers). This is a SIBLING to denial_body_golden_test.go, not an
// extension of it: that file pins the 403 writeEgressDeny family; this pins
// the four httpErrorAWSAware sites' 401/500 bodies.
//
// Every AWS-lane case below has a NON-AWS-lane twin proving the body is
// BYTE-IDENTICAL to what it always was — Anthropic, OpenAI and an operator's
// corp artifact mirror must see no change at all.

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// awsHost / corpHost are this file's two fixtures: a real AWS-lane hostname
// (the run's own SSO portal) and an ordinary corp-artifact MITM host that is
// NOT AWS-lane, so every table below drives both through the identical code
// path and diverges only on isAWSLane's answer.
const (
	awsHost  = "portal.sso.us-east-1.amazonaws.com"
	corpHost = "mirror.corp"
)

// decodeAWSSDKError asserts body is valid JSON shaped like writeAWSSDKError's
// output and returns it decoded.
func decodeAWSSDKError(t *testing.T, body []byte) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("body is not JSON: %v\nbody: %s", err, body)
	}
	return m
}

// TestAWSLaneErrorGolden_VetFailed pins mitm.go's "llm upstream vet failed"
// site (egressTarget failure, before any credential/body handling).
func TestAWSLaneErrorGolden_VetFailed(t *testing.T) {
	for _, tc := range []struct {
		name, host string
		aws        bool
	}{
		{"AWS lane: the run's own SSO portal", awsHost, true},
		{"non-AWS lane: a corp artifact mirror, unchanged", corpHost, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			p := newProxy(Options{
				RunID:    uuid.New(),
				Policy:   CompilePolicy(types.RunPolicySpec{}),
				Sink:     &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 8)},
				Resolver: fakeResolver{err: errors.New("dns down")},
			})
			rec := httptest.NewRecorder()
			p.serveMITMRequest(rec, httptest.NewRequest(http.MethodPost, "https://"+tc.host+"/", nil), tc.host, 443)

			if tc.aws {
				if rec.Code != http.StatusInternalServerError {
					t.Fatalf("status = %d, want 500 (InternalServerException)", rec.Code)
				}
				if got := rec.Header().Get("Content-Type"); got != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", got)
				}
				if got := rec.Header().Get("x-amzn-errortype"); got != "InternalServerException" {
					t.Errorf("x-amzn-errortype = %q, want InternalServerException", got)
				}
				body := decodeAWSSDKError(t, rec.Body.Bytes())
				if body["__type"] != "InternalServerException" {
					t.Errorf("__type = %q, want InternalServerException", body["__type"])
				}
				if !strings.Contains(body["message"], "dns down") {
					t.Errorf("message = %q, want it to carry the underlying cause", body["message"])
				}
			} else {
				// BYTE-IDENTICAL to today: plain text, 502, no AWS headers.
				if rec.Code != http.StatusBadGateway {
					t.Fatalf("status = %d, want 502 (unchanged plain-text lane)", rec.Code)
				}
				if got := rec.Header().Get("x-amzn-errortype"); got != "" {
					t.Errorf("x-amzn-errortype = %q, want unset on a non-AWS lane", got)
				}
				if got := denyBody(rec); !strings.HasPrefix(got, "llm upstream vet failed: ") {
					t.Errorf("body = %q, want the unchanged plain-text prefix", got)
				}
			}
		})
	}
}

// TestAWSLaneErrorGolden_UpstreamError pins forwardInspectedLLM's RoundTrip
// failure ("llm upstream error"), reached here through serveMITMRequest —
// the field report's own path (portal.sso is MITM'd, not brokered).
func TestAWSLaneErrorGolden_UpstreamError(t *testing.T) {
	for _, tc := range []struct {
		name, host string
		aws        bool
	}{
		{"AWS lane: the run's own SSO portal", awsHost, true},
		{"non-AWS lane: a corp artifact mirror, unchanged", corpHost, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			p := newProxy(Options{
				RunID:    uuid.New(),
				Policy:   CompilePolicy(types.RunPolicySpec{}),
				Sink:     &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 8)},
				Resolver: publicResolver{}, // vet succeeds; the actual round trip fails
				Dial:     redirectDial("127.0.0.1:1"),
			})
			rec := httptest.NewRecorder()
			p.serveMITMRequest(rec, httptest.NewRequest(http.MethodPost, "https://"+tc.host+"/", nil), tc.host, 443)

			if tc.aws {
				if rec.Code != http.StatusInternalServerError {
					t.Fatalf("status = %d, want 500 (InternalServerException)", rec.Code)
				}
				if got := rec.Header().Get("x-amzn-errortype"); got != "InternalServerException" {
					t.Errorf("x-amzn-errortype = %q, want InternalServerException", got)
				}
				body := decodeAWSSDKError(t, rec.Body.Bytes())
				// withStage=true at this one site: the SDK message matches the
				// decision log's Cause field verbatim (same causeSentence call).
				if !strings.HasPrefix(body["message"], "tcp dial: ") {
					t.Errorf("message = %q, want the stage-prefixed cause (parity with the decision log)", body["message"])
				}
				d := findDecision(t, buf, "builtin:dial-failed")
				if d.Cause != body["message"] {
					t.Errorf("sandbox message %q != decision log Cause %q — C1/C3 must agree", body["message"], d.Cause)
				}
			} else {
				if rec.Code != http.StatusBadGateway {
					t.Fatalf("status = %d, want 502 (unchanged plain-text lane)", rec.Code)
				}
				if got := denyBody(rec); !strings.HasPrefix(got, "llm upstream error: ") {
					t.Errorf("body = %q, want the unchanged plain-text prefix", got)
				}
			}
		})
	}
}

// TestAWSLaneErrorGolden_CredentialRefreshFailed pins mitm.go's credential
// resolve failure — writeSSOUnauthorized's own sibling three lines away,
// generalised. Extends TestMITMRefreshFailureMasksSecretInError's exact
// setup (which stays in place, unmodified, as the non-AWS control) with the
// AWS-lane host and the new JSON shape — a registered secret in the control
// plane's own failure text must still never reach the sandbox, JSON or not.
func TestAWSLaneErrorGolden_CredentialRefreshFailed(t *testing.T) {
	const secret = "sk-ant-oat-AWS-LANE-GOLDEN-0123456789"
	procMask([]byte(secret))
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "resolve failed for "+secret, http.StatusInternalServerError)
	}))
	defer cp.Close()

	for _, tc := range []struct {
		name, host string
		aws        bool
	}{
		{"AWS lane: the run's own SSO portal", awsHost, true},
		{"non-AWS lane: a corp artifact mirror, unchanged", corpHost, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inj := &injector{
				byHost: map[string]*injEntry{tc.host: {grantID: uuid.New(), expiresAt: time.Now().Add(-time.Hour).UnixMilli()}},
				base:   cp.URL,
				token:  newTokenSource("tok"),
				client: cp.Client(),
			}
			p, _ := newLocalRouteProxy(t, cp.URL, "RUNTOK", upstreamAddr(cp), inj, nil)

			rec := httptest.NewRecorder()
			p.serveMITMRequest(rec, httptest.NewRequest(http.MethodPost, "https://"+tc.host+"/v1/messages", nil), tc.host, 443)

			if strings.Contains(rec.Body.String(), secret) {
				t.Fatalf("registered secret reached the sandbox: %q", rec.Body.String())
			}

			if tc.aws {
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401 (UnauthorizedException, non-retryable — writeSSOUnauthorized's own precedent)", rec.Code)
				}
				body := decodeAWSSDKError(t, rec.Body.Bytes())
				if body["__type"] != "UnauthorizedException" {
					t.Errorf("__type = %q, want UnauthorizedException", body["__type"])
				}
				// The DECODED field, not the wire bytes: encoding/json HTML-escapes
				// '<'/'>' by default, so the placeholder rides as <secret-hidden>
				// on this JSON lane — still the same masked string once a compliant
				// JSON parser (the AWS SDK's) decodes it.
				if !strings.Contains(body["message"], "<secret-hidden>") {
					t.Fatalf("expected the masked placeholder in the decoded message, got %q", body["message"])
				}
			} else {
				// BYTE-IDENTICAL to TestMITMRefreshFailureMasksSecretInError's own
				// assertion: plain text, 502, fail closed — the placeholder is
				// literal on the wire here, unlike the JSON lane above.
				if rec.Code != http.StatusBadGateway {
					t.Fatalf("status = %d, want 502 (unchanged plain-text lane)", rec.Code)
				}
				if !strings.Contains(rec.Body.String(), "<secret-hidden>") {
					t.Fatalf("expected the masked placeholder in the error, got %q", rec.Body.String())
				}
			}
		})
	}
}

// TestAWSLaneErrorGolden_VetFailed_BrokeredRoute pins llm_routes.go's OWN
// "llm upstream vet failed" call site (proxyLLMRequest, reached through the
// brokered /wardyn/llm/* route) — the fourth of the family's four sites, and
// the one none of the cases above exercise (they all drive serveMITMRequest).
// The non-AWS-lane control for this exact code path already lives in
// llm_gateway_test.go (TestLLMGateway_ResolverError_502GatewayVetFailed_RunUnaffected)
// and is not duplicated here.
func TestAWSLaneErrorGolden_VetFailed_BrokeredRoute(t *testing.T) {
	res := fakeResolver{err: errors.New("dns down")}
	inj := staticInj(map[string]injectedHeader{awsHost: {name: "X-Api-Key", value: "K"}})
	p, _ := gatewayProxy(t, "https://"+awsHost+"/", res, "127.0.0.1:1", inj)

	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"messages", strings.NewReader("{}"))
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (InternalServerException)", rec.Code)
	}
	body := decodeAWSSDKError(t, rec.Body.Bytes())
	if body["__type"] != "InternalServerException" {
		t.Errorf("__type = %q, want InternalServerException", body["__type"])
	}
	// vetTrustedHost drops the raw resolver error and returns the fixed
	// errGatewayVet sentinel (egress_target.go) — a config-vet refusal, not a
	// lost dial, so the underlying "dns down" text does not survive here (see
	// TestAWSLaneErrorGolden_VetFailed for the dial-shaped sibling that DOES
	// carry the raw cause).
	if !strings.Contains(body["message"], "configured gateway host refused") {
		t.Errorf("message = %q, want the gateway-vet sentinel's own text", body["message"])
	}
}

// TestIsAWSLane pins the host-shape predicate directly: the run's own SSO
// portal and Bedrock (public + PrivateLink) are IN; Anthropic, OpenAI, a
// configured gateway and an attacker lookalike are OUT.
func TestIsAWSLane(t *testing.T) {
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"portal.sso.us-east-1.amazonaws.com", true},
		{"portal.sso.eu-west-2.amazonaws.com", true},
		{"PORTAL.SSO.US-EAST-1.AMAZONAWS.COM.", true}, // case + trailing dot
		{"bedrock-runtime.us-east-1.amazonaws.com", true},
		{"bedrock-runtime-fips.us-gov-west-1.amazonaws.com", true},
		// Not AWS-lane: the built-in LLM hosts, a configured gateway, and an
		// attacker-chosen lookalike that must not borrow AWS's error shape.
		{anthropicHost, false},
		{openaiHost, false},
		{"llm-gateway.corp.internal", false},
		{"portal.sso.evil.com", false},
		{"portal.sso.us-east-1.amazonaws.com.evil.com", false},
		{"bedrock-runtime.s3.amazonaws.com", false}, // customer-squattable S3 virtual-host label
	} {
		if got := isAWSLane(tc.host); got != tc.want {
			t.Errorf("isAWSLane(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}
