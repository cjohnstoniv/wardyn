// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/setup"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// setup_checks.go holds the /setup/status checklist rows extracted from
// handleSetupStatus — one small pure function per row, in the order the wizard
// renders them. "info" is used for permanent / non-fixable or purely-optional
// conditions so the operator is never shown a red they cannot clear. (The
// remaining rows still live beside their state in setup.go.)

// runnerCheck grades the sandbox runner: no runner (or no live class) is the one
// FAIL on the checklist — runs cannot launch at all. CC2+ is ok; a CC1-only host
// is "info", not a warning: runs work, just at the weakest isolation.
func runnerCheck(rnr SetupRunner) SetupCheck {
	if rnr.Driver == "none" || len(rnr.ConfinementClasses) == 0 {
		return SetupCheck{
			ID: "runner", Label: "Sandbox runner", Status: "fail",
			Detail: "No sandbox runner is configured, so runs cannot launch.",
			Fix:    "Start wardynd with -runner docker (built with -tags docker) so runs are confined and executed.",
		}
	}
	labeled := make([]string, len(rnr.ConfinementClasses))
	hasCC2Plus := false
	for i, c := range rnr.ConfinementClasses {
		labeled[i] = tierLabel(types.ConfinementClass(c))
		if c != "CC1" {
			hasCC2Plus = true
		}
	}
	if !hasCC2Plus {
		return SetupCheck{
			ID: "runner", Label: "Sandbox runner", Status: "info",
			Detail: "Only the Fence tier (weakest — a shared-kernel container) is available on this host; runs work but with the lowest isolation.",
			Fix:    "Unlock the Wall or Vault tier: run `wardyn setup wall` (or `wardyn setup vault`) on the host — it detects your OS/Docker setup and prints the exact steps.",
		}
	}
	return SetupCheck{
		ID: "runner", Label: "Sandbox runner", Status: "ok",
		Detail: "Runner live with the Wall tier or stronger (" + strings.Join(labeled, ", ") + ").",
	}
}

// llmProviderCheck reports the WINNING model/harness signal (llmProvenance's
// detail, "" when there is none). INFO, never a warning, when there is none: a
// model provider is OPTIONAL — needed only for agent-harness runs or the AI Run
// Composer, so "no model" is a deliberate non-blocking state, never a gap the
// operator must clear.
func llmProviderCheck(llmDetail string) SetupCheck {
	if llmDetail != "" {
		return SetupCheck{ID: "llm_provider", Label: "LLM access", Status: "ok", Detail: llmDetail}
	}
	return SetupCheck{
		ID: "llm_provider", Label: "LLM access", Status: "info",
		Detail: "No model/harness provider configured (optional): needed only for agent-harness runs or the AI Run Composer. Bring-your-own-container and interactive runs work without one.",
		Fix:    "Optional — connect a Claude subscription/API key or Bedrock (Getting Started → Model), or bind creds to a workspace/container.",
	}
}

// bedrockProviderCheck surfaces a row only once the operator has touched ANY
// Bedrock knob (ok=false otherwise), so the majority who never use AWS aren't
// shown an irrelevant row. warn = partially configured, a real gap worth fixing.
func bedrockProviderCheck(bedrock SetupBedrock) (SetupCheck, bool) {
	if !bedrock.configured() {
		return SetupCheck{}, false
	}
	if bedrock.ready() {
		return SetupCheck{
			ID: "bedrock_provider", Label: "AWS Bedrock", Status: "ok",
			Detail: fmt.Sprintf("Bedrock is configured (region %s, model %s) for Claude runs via %s.", bedrock.Region, bedrock.Model, bedrock.credSourceDesc()),
		}, true
	}
	var missing []string
	if bedrock.Region == "" {
		missing = append(missing, "-bedrock-region")
	}
	if bedrock.Model == "" {
		missing = append(missing, "-bedrock-model")
	}
	if !bedrock.CredsPresent && !bedrock.AWSMount && !bedrock.BearerPresent {
		missing = append(missing, "a credential — a read-only ~/.aws mount (-bedrock-aws-dir), a bedrock-api-key bearer secret, or aws-access-key-id + aws-secret-access-key secrets")
	}
	return SetupCheck{
		ID: "bedrock_provider", Label: "AWS Bedrock", Status: "warn",
		Detail: "Bedrock is partially configured; runs will NOT use it until this is complete.",
		Fix:    "Still needed: " + strings.Join(missing, ", ") + ".",
	}, true
}

// composerCheck reports whether the AI Run Composer is enabled — optional, so
// "not enabled" is info.
func composerCheck(comp SetupComposer) SetupCheck {
	if comp.Enabled {
		return SetupCheck{
			ID: "composer", Label: "AI Run Composer", Status: "ok",
			Detail: "The AI Run Composer is enabled (default backend: " + comp.Default + ").",
		}
	}
	return SetupCheck{
		ID: "composer", Label: "AI Run Composer", Status: "info",
		Detail: "The AI Run Composer is not enabled (optional); runs can still be configured manually.",
		Fix:    "Set -composer-config / WARDYN_COMPOSER_CONFIG to enable natural-language run composition.",
	}
}

// ageKeyCheck warns when the secret store's age key is EPHEMERAL: stored secrets
// become unreadable after a restart.
func ageKeyCheck(durable bool) SetupCheck {
	if durable {
		return SetupCheck{
			ID: "age_key", Label: "Secret store durability", Status: "ok",
			Detail: "The secret store age key is durable; stored secrets survive a restart.",
		}
	}
	return SetupCheck{
		ID: "age_key", Label: "Secret store durability", Status: "warn",
		Detail: "The secret store uses an EPHEMERAL age key generated at boot; stored secrets (API keys, GitHub App credentials) become unreadable after a restart.",
		Fix:    "Generate a durable key with `wardynd -gen-age-key` and set it as WARDYN_AGE_KEY (or -age-key).",
	}
}

// siteConfigCheck reports whether an operator-wide corporate baseline (upstream
// proxy, artifact-registry overrides, default SCM hosts) has been authored yet.
// Always "info" — it is optional and skippable, never a blocking gate.
func siteConfigCheck(sc types.SiteConfig) SetupCheck {
	if sc.UpstreamProxySecretRef != "" || len(sc.ArtifactOverrides) > 0 || len(sc.ScmHosts) > 0 {
		return SetupCheck{
			ID: "site_config", Label: "Site config (corporate baseline)", Status: "info",
			Detail: "An operator-wide site config is set (upstream proxy / artifact-registry overrides / SCM hosts); every run inherits it.",
		}
	}
	return SetupCheck{
		ID: "site_config", Label: "Site config (corporate baseline)", Status: "info",
		Detail: "No operator-wide site config yet (optional): a corporate upstream proxy, artifact-registry redirects, and default SCM hosts that every run would inherit.",
		Fix:    "Set one via PUT /api/v1/site-config (or the Host Proxy / Artifact Redirect setup steps).",
	}
}

// platformChecks are the platform rows — permanent and non-fixable, so always
// "info" (and absent on a platform they do not apply to).
func platformChecks(plat setup.Platform) []SetupCheck {
	var out []SetupCheck
	if plat.WSL {
		out = append(out, SetupCheck{
			ID: "platform_wsl", Label: "WSL networking", Status: "info", Platform: "wsl",
			Detail: "Running under WSL2: host<->sandbox networking is split. Reach the UI from Windows via localhost port-forwarding, and bind wardynd to a WSL-reachable address. With Docker Desktop's default NAT networking, sandbox->wardynd callbacks don't route in host mode — workspace Verify results never report and Record captures land empty.",
			Fix:    "Enable WSL2 mirrored networking ([wsl2] networkingMode=mirrored in %UserProfile%\\.wslconfig, then `wsl --shutdown`), or run the containerized stack (`make compose-up`) where callbacks route in-network.",
		})
	}
	if plat.OS == "darwin" {
		out = append(out, SetupCheck{
			ID: "platform_macos", Label: "macOS virtualization", Status: "info", Platform: "darwin",
			Detail: "macOS has no /dev/kvm; the Vault tier (CC3, hardware-virtualized) is unavailable — runs use container isolation.",
		})
	}
	return out
}
