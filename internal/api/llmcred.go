// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// llmProvider is the api-key injection convention an LLM-backed agent needs to
// reach its model in API-KEY mode — the mode EVERY composed run uses, because the
// clamp strips the operator-only ~/.claude subscription mount, so a composed run
// always routes model calls through the proxy's brokered /wardyn/llm route. The
// fields mirror the api_key grant scope the stock LLM policies ship
// (examples/policies/claude-llm.json): Anthropic authenticates with x-api-key
// (bare key), OpenAI with Authorization: Bearer.
type llmProvider struct{ host, header, format, secret string }

// agentLLMProvider maps a coding-agent name to its model provider's api_key
// injection convention, or ok=false for a non-LLM / unknown agent. A thin
// lookup into the harness catalog (harness.go); the convention itself lives
// on each row's Gateway field.
func agentLLMProvider(agent string) (llmProvider, bool) {
	def, ok := harnessByID(agent)
	if !ok || def.Gateway == nil {
		return llmProvider{}, false
	}
	return *def.Gateway, true
}

// llmProviderFor is agentLLMProvider; the boot gateway overrides it applied
// are retired. ponytail: inline agentLLMProvider at its caller and delete this.
func (s *Server) llmProviderFor(agent string) (llmProvider, bool) {
	return agentLLMProvider(agent)
}

// apiKeyGrantScopeHost decodes an api_key grant scope's host field
// (trimmed; "" when absent or undecodable).
func apiKeyGrantScopeHost(scope json.RawMessage) string {
	var sc struct {
		Host string `json:"host"`
	}
	_ = json.Unmarshal(scope, &sc)
	return strings.TrimSpace(sc.Host)
}

// apiKeyGrantScopeSecret returns the secret_name an api_key grant's scope
// carries. A grant may name a NON-convention secret (a policy's own grant or a
// workspace's required secret, applyRequiredSecretGrant), so verdict code must
// key on the grant's secret, not the provider default.
func apiKeyGrantScopeSecret(scope json.RawMessage) string {
	var sc struct {
		SecretName string `json:"secret_name"`
	}
	_ = json.Unmarshal(scope, &sc)
	return strings.TrimSpace(sc.SecretName)
}

// apiKeyGrantForHost returns the api_key grant in spec whose scope.host == host.
func apiKeyGrantForHost(spec *types.RunPolicySpec, host string) (types.GrantSpec, bool) {
	for _, g := range spec.EligibleGrants {
		if g.Kind == types.GrantAPIKey && strings.EqualFold(apiKeyGrantScopeHost(g.Scope), host) {
			return g, true
		}
	}
	return types.GrantSpec{}, false
}

// domainAllowedExact reports whether host is an EXACT entry in domains (case-
// insensitive). Wildcards do NOT count: the proxy's credential injector requires
// an exact-host allowlist entry (buildInjector -> AllowedExactHost) so a brokered
// key can never leak to a wildcard-matched host — so the grant's egress entry must
// be exact too.
func domainAllowedExact(domains []string, host string) bool {
	h := strings.TrimSpace(host)
	return slices.ContainsFunc(domains, func(d string) bool {
		return strings.EqualFold(strings.TrimSpace(d), h)
	})
}

// Claude subscription-mode credential mount targets. Dispatch detects
// subscription mode by the FIRST of these (internal/api/runs.go:
// specHasMountTarget(claudeCredTarget) => ANTHROPIC_BASE_URL=https://api.anthropic.com,
// direct CONNECT tunnel gated by the run's egress allowlist). The .claude.json companion
// carries the CLI's account config — the proven recipe needs BOTH mounted.
const (
	claudeCredTarget     = "/home/agent/.claude"
	claudeCredJSONTarget = "/home/agent/.claude.json"
)

// modelProviderEgress returns the LLM MODEL-PROVIDER hosts the operator ceiling
// blesses (api.anthropic.com / *.anthropic.com / api.openai.com). These are the
// HARNESS's egress — needed by any agent session to reach the model — distinct from
// the workspace's app egress the operator approves per-workspace. A confined session
// unions these in so the model is reachable (see launchRecordRun).
func (s *Server) modelProviderEgress(ceiling types.RunPolicySpec) []string {
	var out []string
	for _, d := range ceiling.AllowedDomains {
		if s.isModelProviderHost(d) {
			out = append(out, d)
		}
	}
	return out
}

// isModelProviderHost reports whether h is an LLM model-provider host by the
// anthropic/openai convention. Extracted from modelProviderEgress's own loop so
// a REJECT guard (denyAlwaysReject, approvals.go) can test the CANDIDATE rather
// than test membership in this function's OUTPUT — the difference between the
// guard working and silently not firing.
//
// modelProviderEgress returns ceiling entries VERBATIM, and a ceiling entry may
// be the wildcard "*.anthropic.com" as readily as "api.anthropic.com". A deny
// candidate, by contrast, is always a concrete host — hostrules.ValidApprovedHost
// admits no wildcards. So on a wildcard-only ceiling "api.anthropic.com" is not
// a member of {"*.anthropic.com"}, a membership test would accept the deny, and
// the workspace's credential injection would be permanently bricked. A guard
// whose firing depends on deployment config is worse than one uniformly absent.
//
// The suffix match is loose — it also matches evilanthropic.com. For a REJECT
// guard loose is the fail-CLOSED direction; for modelProviderEgress it only ever
// filters hosts the OPERATOR already put in their own ceiling.
func (s *Server) isModelProviderHost(h string) bool {
	hl := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
	return strings.HasSuffix(hl, "anthropic.com") || hl == "api.openai.com"
}

// modelServingHosts is the ONE answer to "does this host serve a model on this
// deployment", for every reject and skip decision that needs it: the vendor
// hosts (isModelProviderHost) and EVERY model provider row in sc — its own host
// (providerHost: a custom endpoint or route-through gateway's base URL, else
// the vendor's) and, for a Bedrock row, its runtime and control hosts for its
// region — whether or not the row is on or chosen by the run.
//
// It is a SECOND predicate rather than a widening of isModelProviderHost on
// purpose. isModelProviderHost also gates an ACCEPT: inline_policy.go's 6c
// own-key arm admits a member's OWN api_key secret with no operator eligible-grant
// pairing whenever the paired host is a model-provider host. Widening that
// predicate would hand every host here a new unpaired-grant accept as a side
// effect of fixing a deny guard. The reject direction is where these hosts
// belong: each carries proxy-side credential injection, which is exactly the
// "proxy-side credential injection refuses a denied host" failure
// denyAlwaysReject exists to prevent.
//
// Its callers, grep-checkable: denyAlwaysReject (approvals_writeback.go),
// planArtifactRedirect's To-host veto (artifact_redirect.go), the integration
// requirement's credential skip (applyIntegrationRequirement), the provider
// path's strip (dropLegacyModelInjections) and the integration write's refusal
// (handlePutIntegration).
func (s *Server) modelServingHosts(sc types.SiteConfig) func(string) bool {
	var hosts []string
	for _, p := range modelProviderRows(sc) {
		hosts = append(hosts, providerHost(p))
		if b := providerBedrockSettings(p); p.Kind.IsBedrock() && b.Region != "" {
			hosts = append(hosts, providerBedrockRuntimeHost(p), bedrockControlHost(b.Region))
		}
	}
	return func(h string) bool {
		if strings.TrimSpace(h) == "" {
			return false
		}
		return s.isModelProviderHost(h) || slices.ContainsFunc(hosts, func(x string) bool { return x != "" && hostEqual(x, h) })
	}
}

// isModelProviderRejectHost is modelServingHosts over the stored site config,
// for a caller that holds none. A config that cannot be read contributes no
// provider rows; the static half still answers.
func (s *Server) isModelProviderRejectHost(ctx context.Context, h string) bool {
	var sc types.SiteConfig
	if s.cfg.Store != nil {
		if got, err := s.cfg.Store.GetSiteConfig(ctx); err == nil {
			sc = got
		}
	}
	return s.modelServingHosts(sc)(h)
}

// specHasMountTarget reports whether the spec already carries a mount at target.
func specHasMountTarget(spec *types.RunPolicySpec, target string) bool {
	return slices.ContainsFunc(spec.WorkspaceMounts, func(wm types.WorkspaceMount) bool {
		return wm.Target == target
	})
}

// composeLLMAccess is the structured model-access verdict for a run's resolved
// LLM credential (runLLMAccess, runs.go) so a review/setup surface need
// never prose-sniff a warning to tell "this run will do nothing" from "tightened
// by policy". Despite the name (a holdover from the deleted AI Run Composer,
// which first introduced this verdict shape), it backs the general create-run
// path — every run's model-access check, not an AI-composed one.
type composeLLMAccess struct {
	Provisioned bool   `json:"provisioned"`
	Note        string `json:"note"`
}
