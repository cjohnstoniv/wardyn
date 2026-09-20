// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/api"
)

// validateModelEndpoints resolves and fail-closed-validates every operator knob
// that moves where a model call — or the credential exchange behind one —
// actually goes: the two brokered api-key gateways, the Bedrock data-plane
// override, and the gated AWS SSO endpoint test hatch.
//
// Bedrock is deliberately NOT a member of ValidateLLMGateways' map. That map
// means "broker this vendor's api-key lane through a reverse proxy" — its
// consumers mint an api_key grant and serve the host over /wardyn/llm/*. Bedrock
// is a CONNECT tunnel with SigV4, or MITM plus a bearer, so a key in that map
// would have the proxy try to serve it over a route it does not speak. They are
// validated together here because they answer one question, not because they
// share a mechanism.
func validateModelEndpoints(f *bootFlags) (map[string]string, string, string, error) {
	llmGateways, err := api.ValidateLLMGateways(*f.anthropicBaseURL, *f.openaiBaseURL)
	if err != nil {
		return nil, "", "", err
	}
	// *f.bedrockRegion is already resolved (parseBootFlags folds in AWS_REGION).
	bedrockBaseURL, err := api.ValidateBedrockBaseURL(*f.bedrockBaseURL, *f.bedrockRegion, *f.allowTestEndpoints)
	if err != nil {
		return nil, "", "", err
	}
	// The OTHER relaxation WARDYN_ALLOW_TEST_ENDPOINTS unlocks, made audible.
	// The AWS SSO override WARNs on every boot that carries it; this one
	// — which re-points the bearer-mode credential-INJECTION target — logged
	// nothing at all, so a deployment that inherited it served a real Bedrock API
	// key over cleartext with only docs/ENV.md to say so. Read off the VALIDATED
	// value, so it fires exactly when the relaxation was actually taken.
	if strings.HasPrefix(strings.ToLower(bedrockBaseURL), "http://") {
		slog.Warn(api.BedrockPlainHTTPWarn, slog.String("bedrock_base_url", bedrockBaseURL))
	}
	// The gated AWS SSO endpoint hatch (resolveAWSSSOEndpointOverride,
	// boot_flags.go). Resolved here rather than in run() because it answers the
	// same class of question — "where does this deployment's traffic actually
	// go, and does the value parse" — and because run()'s cyclomatic budget is
	// full. It is NOT a model endpoint: it re-points a CREDENTIAL exchange, and
	// it is a TEST hatch rather than a supported posture, which is why it
	// refuses boot without WARDYN_ALLOW_TEST_ENDPOINTS.
	awsSSOEndpointOverride, err := resolveAWSSSOEndpointOverride(f)
	if err != nil {
		return nil, "", "", err
	}
	// A WARNING, never a refusal: the model is passed to the agent verbatim and
	// Wardyn deliberately does not police its shape. But the AWS SSO account
	// check (roster save and capture, see internal/api/awssso_pin.go) reads the
	// account out of this ARN, and a malformed one turns that check off
	// silently — so say it once, at boot, rather than never.
	if api.BedrockModelARNNamesNoAccount(*f.bedrockModel) {
		slog.Warn("wardynd: the configured Bedrock model looks like an ARN but names no 12-digit account, so the account check is off — an AWS SSO sign-in's account will not be compared against the model's. Check the account field of WARDYN_BEDROCK_MODEL.",
			slog.String("bedrock_model", *f.bedrockModel),
		)
	}
	return llmGateways, bedrockBaseURL, awsSSOEndpointOverride, nil
}
