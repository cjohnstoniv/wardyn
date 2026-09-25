// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "github.com/cjohnstoniv/wardyn/internal/types"

// DRAFT (M2 canon pending).
const mpRunModelEnvSecret = "This run's policy grants env_secret %s as %s, but on this deployment a run's model credential " +
	"comes only from its model provider — remove that grant from the policy, or ask your admin."

// modelEnvNames is the one table of sandbox variables a model credential rides
// in or a model-provider arm sets. It is filled by the declarations below, the
// names the arms write through, so it cannot drift from them. Under a
// governing provider block an env_secret grant may set none of them: a run's
// model credential comes only from its provider (owner ruling, 2026-09-25).
var modelEnvNames = map[string]bool{}

func modelEnvVar(name string) string {
	modelEnvNames[name] = true
	return name
}

var (
	envAnthropicAPIKey    = modelEnvVar("ANTHROPIC_API_KEY")
	envOpenAIAPIKey       = modelEnvVar("OPENAI_API_KEY")
	envBedrockBearer      = modelEnvVar("AWS_BEARER_TOKEN_BEDROCK")
	envAnthropicBaseURL   = modelEnvVar("ANTHROPIC_BASE_URL")
	envAnthropicModel     = modelEnvVar("ANTHROPIC_MODEL")
	envClaudeConfigDir    = modelEnvVar("CLAUDE_CONFIG_DIR")
	envClaudeManagedCreds = modelEnvVar("WARDYN_CLAUDE_MANAGED_B64")
	envOpenAIBaseURL      = modelEnvVar("OPENAI_BASE_URL")
	envClaudeUseBedrock   = modelEnvVar("CLAUDE_CODE_USE_BEDROCK")
	envAWSRegion          = modelEnvVar("AWS_REGION")
	envAWSDefaultRegion   = modelEnvVar("AWS_DEFAULT_REGION")
	envBedrockBaseURL     = modelEnvVar("ANTHROPIC_BEDROCK_BASE_URL")
	envBedrockRuntimeURL  = modelEnvVar("AWS_ENDPOINT_URL_BEDROCK_RUNTIME")
	envAWSConfigFile      = modelEnvVar("AWS_CONFIG_FILE")
	envAWSSharedCredsFile = modelEnvVar("AWS_SHARED_CREDENTIALS_FILE")
	envAWSProfile         = modelEnvVar("AWS_PROFILE")
	// Named once elsewhere and written by the AWS sign-in arm (bedrockSSOAuth);
	// credentials a harness reads that no arm sets are refused all the same.
	_ = modelEnvVar(awsSSOConfigEnvVar)
	_ = modelEnvVar(awsEndpointURLSSOEnv)
	_ = modelEnvVar(awsEndpointURLSSOOIDCEnv)
	_ = modelEnvVar("ANTHROPIC_AUTH_TOKEN")
	_ = modelEnvVar("CLAUDE_CODE_OAUTH_TOKEN")
	_ = modelEnvVar("AWS_ACCESS_KEY_ID")
	// What else Claude Code reads that carries a model credential or re-points
	// it past the brokered route: extra request headers, and the Foundry,
	// Vertex and Anthropic-on-AWS lanes (code.claude.com/docs/en/env-vars).
	_ = modelEnvVar("ANTHROPIC_CUSTOM_HEADERS")
	_ = modelEnvVar("ANTHROPIC_FOUNDRY_API_KEY")
	_ = modelEnvVar("ANTHROPIC_FOUNDRY_AUTH_TOKEN")
	_ = modelEnvVar("ANTHROPIC_FOUNDRY_BASE_URL")
	_ = modelEnvVar("ANTHROPIC_FOUNDRY_RESOURCE")
	_ = modelEnvVar("CLAUDE_CODE_USE_FOUNDRY")
	_ = modelEnvVar("CLAUDE_CODE_USE_VERTEX")
	_ = modelEnvVar("ANTHROPIC_VERTEX_BASE_URL")
	_ = modelEnvVar("ANTHROPIC_VERTEX_PROJECT_ID")
	_ = modelEnvVar("ANTHROPIC_AWS_API_KEY")
	_ = modelEnvVar("ANTHROPIC_AWS_BASE_URL")
	_ = modelEnvVar("ANTHROPIC_PROFILE")
)

// modelEnvSecretGrant reports the first env_secret grant in spec that would set
// a variable in modelEnvNames: that variable and the secret the grant names.
// A grant whose scope does not parse is not one: resolveEnvSecretGrants never
// places it.
func modelEnvSecretGrant(spec types.RunPolicySpec) (name, secretName string, found bool) {
	for _, g := range spec.EligibleGrants {
		if g.Kind != types.GrantEnvSecret {
			continue
		}
		if n, sn, err := envSecretScopeFields(g.Scope); err == nil && modelEnvNames[n] {
			return n, sn, true
		}
	}
	return "", "", false
}
