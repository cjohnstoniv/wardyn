// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/api"
)

// TestBedrockPlainHTTPIsAudibleAtBoot pins the runtime signal for the
// SECOND relaxation WARDYN_ALLOW_TEST_ENDPOINTS unlocks.
//
// Only the AWS SSO override was audible: it WARNs on every boot that carries it
// and the flag's help named it. Plain `http://` for WARDYN_BEDROCK_BASE_URL —
// the bearer-mode credential-INJECTION target, so the Bedrock API key rides
// `Authorization: Bearer` in cleartext on every model call — logged nothing, and
// the flag help did not mention it. 0.7.3 refused that boot outright; 0.7.4
// accepts it, so the disclosure has to be somewhere other than docs/ENV.md.
func TestBedrockPlainHTTPIsAudibleAtBoot(t *testing.T) {
	capture := func(t *testing.T, baseURL string, ack bool) (string, error) {
		t.Helper()
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		t.Cleanup(func() { slog.SetDefault(prev) })

		empty, region, model := "", "us-east-1", ""
		override := ""
		f := &bootFlags{
			anthropicBaseURL:       &empty,
			openaiBaseURL:          &empty,
			anthropicGatewayHeader: &empty,
			anthropicGatewayFormat: &empty,
			openaiGatewayHeader:    &empty,
			openaiGatewayFormat:    &empty,
			bedrockBaseURL:         &baseURL,
			bedrockRegion:          &region,
			bedrockModel:           &model,
			allowTestEndpoints:     &ack,
			awsSSOEndpointOverride: &override,
		}
		_, _, _, _, err := validateModelEndpoints(f)
		return buf.String(), err
	}

	t.Run("http with the acknowledgement boots, loudly", func(t *testing.T) {
		logged, err := capture(t, "http://mirror.corp", true)
		if err != nil {
			t.Fatalf("validateModelEndpoints: %v, want nil (the ack accepts plain http)", err)
		}
		if !strings.Contains(logged, api.BedrockPlainHTTPWarn) {
			t.Errorf("boot said nothing about serving the Bedrock credential over cleartext:\n%s", logged)
		}
		if !strings.Contains(logged, "http://mirror.corp") {
			t.Errorf("the warning does not name the plaintext target:\n%s", logged)
		}
	})

	// NEGATIVE CONTROLS: the refusal without the ack is untouched, and an
	// ordinary https endpoint logs nothing new in either posture.
	t.Run("http without the acknowledgement still refuses boot", func(t *testing.T) {
		logged, err := capture(t, "http://mirror.corp", false)
		if err == nil {
			t.Fatal("plain http:// without WARDYN_ALLOW_TEST_ENDPOINTS no longer refuses boot")
		}
		if !strings.Contains(err.Error(), "WARDYN_ALLOW_TEST_ENDPOINTS") ||
			!strings.Contains(err.Error(), "WARDYN_BEDROCK_BASE_URL") {
			t.Errorf("the refusal no longer names both variables: %v", err)
		}
		if strings.Contains(logged, api.BedrockPlainHTTPWarn) {
			t.Errorf("a REFUSED boot logged the hatch warning as though it had been taken:\n%s", logged)
		}
	})
	for _, ack := range []bool{false, true} {
		t.Run("https logs nothing new", func(t *testing.T) {
			logged, err := capture(t, "https://vpce-abc.bedrock-runtime.us-east-1.vpce.amazonaws.com", ack)
			if err != nil {
				t.Fatalf("validateModelEndpoints(https, ack=%v): %v", ack, err)
			}
			if strings.Contains(logged, api.BedrockPlainHTTPWarn) {
				t.Errorf("an https endpoint logged the plain-http hatch warning (ack=%v):\n%s", ack, logged)
			}
		})
	}
}
