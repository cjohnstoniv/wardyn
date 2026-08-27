// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "github.com/cjohnstoniv/wardyn/internal/types"

// harness.go is the single catalog of coding-agent "harnesses" Wardyn knows
// how to run. Before this file, the same knowledge was spread across three
// independent string-matches that had to be kept in sync by hand: agentImage's
// ghcr.io convention (runs_policy.go), agentLLMProvider's api-key switch
// (llmcred.go), and agentHarnessLogin's container-login switch
// (harnesscred.go). Adding a harness — or teaching an ai-integration TYPE
// (internal/api/integrations.go) which harnesses it can/cannot drive — is now
// one row/entry here; those three functions are thin lookups into
// harnessCatalog.
//
// aws-sso is deliberately NOT a row here: it is a login-only auxiliary
// provider (Bedrock credential capture) that names no coding-agent harness a
// run's --agent selects, so agentHarnessLogin keeps it as a direct case
// exactly as before — see harnesscred.go.

// harnessDef is one catalog row: everything Wardyn statically knows about a
// coding agent it can run.
type harnessDef struct {
	ID      string // the --agent / types.AgentRun.Agent value this row matches
	Display string // human label (Integrations UI "Tool compatibility" rows)

	// ImageKey feeds the ghcr.io/cjohnstoniv/agent-<ImageKey>:latest fallback
	// agentImage uses once WARDYN_AGENT_IMAGES has no override for ID. Equal to
	// ID for every shipped row; kept distinct in case an image slug ever
	// diverges from the CLI-facing agent id.
	ImageKey string

	// Gateway is the api-key injection convention a composed (non-subscription,
	// non-Bedrock) run uses to reach this harness's model. nil for a harness
	// with no managed model credential (BYOA).
	Gateway *llmProvider

	// Login is the container-login convention (harnesscred.go) an operator can
	// use to connect a subscription/session credential for this harness. nil
	// when no such flow exists yet.
	Login *harnessLogin

	// ProviderTypes maps an ai-integration TYPE (e.g. "anthropic_api_key",
	// "bedrock" — the Settings cards' vocabulary, see
	// integrations.go) to "" when that type CAN drive this harness, or to the
	// verbatim protocol-fact reason it CANNOT. A type absent from the map
	// reads as "" (possible); every type this harness's family is ever asked
	// about is listed explicitly below so that default is never load-bearing.
	ProviderTypes map[string]string

	// NoManagedAuth marks the BYOA row: Wardyn wires no model credential at
	// all and the image authenticates however it likes.
	NoManagedAuth bool
}

// Tool-impossibility copy canon, verbatim from the Integrations screen mock
// (scratchpad mockup/wardyn-integrations.js, the T object) — this is
// reviewed, user-facing wording; do not paraphrase it here or let it drift
// from there.
const (
	reasonXKeyCodex     = "Codex CLI speaks the OpenAI API only — an Anthropic key can't drive it. Not a setting."
	reasonXSubCodex     = "Codex CLI speaks the OpenAI API only — a Claude login can't drive it. Not a setting."
	reasonXBedrockCodex = "Codex CLI speaks the OpenAI API only — Bedrock can't drive it. Not a setting."
	reasonXOpenAIClaude = "Claude Code speaks the Anthropic API only — an OpenAI key can't drive it. Not a setting."
)

// harnessCatalog is the full set of coding-agent harnesses. Order is cosmetic
// (a later wave's Tools-tab listing); lookup is always by ID (harnessByID).
var harnessCatalog = []harnessDef{
	{
		// ImageKey is "base", NOT "claude-code": 0.6.2 stopped publishing
		// agent-claude-code and publishes agent-base in its place, so the ghcr
		// fallback for this row resolved to an image that does not exist —
		// `--agent claude-code` 404'd on every published install (the one-line
		// installer, Helm, and the desktop tier), leaving codex-cli as the only
		// working agent name. agent-base carries the full image contract minus
		// the vendor CLI, which is also exactly what the four callers passing a
		// literal "claude-code" (source_scan, site_config_probe,
		// workspace_run_image, setup) actually want.
		//
		// An operator who wants the real Claude Code CLI builds it locally and
		// names it in WARDYN_AGENT_IMAGES, which is consulted FIRST and is keyed
		// by agent name — so this changes nothing for them. See loginImageKey
		// below for the one lane that must not follow this re-point.
		ID: "claude-code", Display: "Claude Code", ImageKey: "base",
		Gateway: &llmProvider{host: "api.anthropic.com", header: "x-api-key", format: "%s", secret: "anthropic-api-key"},
		Login: &harnessLogin{
			provider: "anthropic",
			agent:    "claude-code",
			// NOT the catalog's ImageKey ("base"): a login sandbox must carry the
			// vendor CLI it is logging into, and agent-base ships none — the box
			// would come up with `claude` not on PATH and the flow could never
			// complete. NAMED GAP: agent-claude-code is unpublished, so on a
			// published install this ref 404s unless the operator has built it
			// locally and named it in WARDYN_AGENT_IMAGES (which wins over this).
			// That is the same state as before the re-point, not a regression.
			loginImageKey: "claude-code",
			secretName:    harnessCredSecretName("anthropic"),
			sentinel:      types.ManagedOAuthSecret,
			injectHost:    subscriptionInjectionHost, // api.anthropic.com
			tokenPrefix:   "sk-ant-oat",
			// `claude setup-token` OAuth (observed v2.1.x): authorize on claude.com,
			// remote callback on platform.claude.com, token exchange on the Anthropic
			// console/api hosts. Enumerated empirically; prune/extend from the login
			// run's decision log (any extra host surfaces as a deny_with_review).
			egress: []string{"claude.com", "platform.claude.com", "console.anthropic.com", "api.anthropic.com"},
		},
		ProviderTypes: map[string]string{
			"anthropic_api_key":      "",
			"anthropic_subscription": "",
			"bedrock":                "",
			"openai_api_key":         reasonXOpenAIClaude,
		},
	},
	{
		ID: "codex-cli", Display: "Codex CLI", ImageKey: "codex-cli",
		Gateway: &llmProvider{host: "api.openai.com", header: "Authorization", format: "Bearer %s", secret: "openai-api-key"},
		// No container-login convention yet (v2 seam: ~/.codex/auth.json capture
		// + a chatgpt.com sink — see the harnessLogin type doc in harnesscred.go).
		ProviderTypes: map[string]string{
			"anthropic_api_key":      reasonXKeyCodex,
			"anthropic_subscription": reasonXSubCodex,
			"bedrock":                reasonXBedrockCodex,
			"openai_api_key":         "",
		},
	},
	{
		// BYOA: the run's own image authenticates however it likes; Wardyn
		// wires no model credential and offers this row against no
		// ai-integration type (ProviderTypes intentionally nil/empty).
		ID: "none", Display: "Your own tools", ImageKey: "none",
		NoManagedAuth: true,
	},
}

// harnessByID returns the catalog row for agent, or ok=false when agent names
// no known harness (a WARDYN_AGENT_IMAGES-only custom agent, or a typo) — its
// callers each keep their own convention fallback for that case (agentImage's
// ghcr convention; agentLLMProvider/agentHarnessLogin's plain "unsupported").
func harnessByID(agent string) (harnessDef, bool) {
	for _, d := range harnessCatalog {
		if d.ID == agent {
			return d, true
		}
	}
	return harnessDef{}, false
}

// harnessProviderReason reports whether the ai-integration type providerType
// can drive the named harness: "" means yes, a non-empty string is the
// verbatim protocol fact it cannot (harnessDef.ProviderTypes). An unknown
// harness id reads as "" (possible) — capabilitiesFor only asks this for the
// catalog's own Gateway-bearing rows, so that default is never load-bearing.
func harnessProviderReason(harnessID, providerType string) string {
	def, ok := harnessByID(harnessID)
	if !ok {
		return ""
	}
	return def.ProviderTypes[providerType]
}
