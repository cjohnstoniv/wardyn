// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
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
// carries. A grant may name a NON-convention secret (a workspace's resolved
// AI-provider Integration does — applyIntegrationCreds grants the
// INTEGRATION's own secret, not necessarily the provider convention name), so
// verdict code must key on the grant's secret, not the provider default.
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

// removeAPIKeyGrantForHost drops the api_key grant whose scope.host == host.
func removeAPIKeyGrantForHost(spec *types.RunPolicySpec, host string) {
	spec.EligibleGrants = slices.DeleteFunc(spec.EligibleGrants, func(g types.GrantSpec) bool {
		return g.Kind == types.GrantAPIKey && strings.EqualFold(apiKeyGrantScopeHost(g.Scope), host)
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
// unions these in so subscription/api-key wiring can attach and the model is
// reachable (see launchRecordRun); without them applyLLMCredMount refuses to inject
// a resident credential the agent could never use.
func modelProviderEgress(ceiling types.RunPolicySpec) []string {
	var out []string
	for _, d := range ceiling.AllowedDomains {
		if isModelProviderHost(d) {
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
// modelProviderEgress returns ceiling entries VERBATIM, and the canonical
// ceiling entry is the wildcard: applyLLMCredMount below accepts
// "api.anthropic.com" or "*.anthropic.com" and its own error text instructs the
// operator to list "*.anthropic.com (or api.anthropic.com) verbatim". A deny
// candidate, by contrast, is always a concrete host — hostrules.ValidApprovedHost
// admits no wildcards. So on a wildcard-only ceiling "api.anthropic.com" is not
// a member of {"*.anthropic.com"}, a membership test would accept the deny, and
// the workspace's credential injection would be permanently bricked. A guard
// whose firing depends on deployment config is worse than one uniformly absent.
//
// The suffix match is loose — it also matches evilanthropic.com. For a REJECT
// guard loose is the fail-CLOSED direction; for modelProviderEgress it only ever
// filters hosts the OPERATOR already put in their own ceiling.
func isModelProviderHost(h string) bool {
	hl := strings.ToLower(strings.TrimSpace(h))
	return strings.HasSuffix(hl, "anthropic.com") || hl == "api.openai.com"
}

// ceilingBlessesClaudeCreds reports whether the operator ceiling blesses a Claude
// credential mount (a WorkspaceMount targeting /home/agent/.claude). Only the
// operator authors ceiling mounts, so this is the control-plane-level half of the
// subscription consent; the run half is the resolved integration (resident_host
// anthropic_subscription).
func ceilingBlessesClaudeCreds(ceiling types.RunPolicySpec) bool {
	return specHasMountTarget(&ceiling, claudeCredTarget)
}

// anthropicReachable reports whether the FINAL spec's egress lets the agent reach
// api.anthropic.com: allow-all, an exact entry, or a *.anthropic.com wildcard
// (label-suffix semantics mirroring the proxy's policy matcher). Subscription
// mode injects no secret, so a wildcard entry is injection-safe here — the
// injector's exact-host rule (AllowedExactHost) is not in play.
func anthropicReachable(spec *types.RunPolicySpec) bool {
	if spec.AllowAllEgress {
		return true
	}
	for _, d := range spec.AllowedDomains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "api.anthropic.com" || d == "*.anthropic.com" {
			return true
		}
	}
	return false
}

// specHasMountTarget reports whether the spec already carries a mount at target.
func specHasMountTarget(spec *types.RunPolicySpec, target string) bool {
	return slices.ContainsFunc(spec.WorkspaceMounts, func(wm types.WorkspaceMount) bool {
		return wm.Target == target
	})
}

// applyLLMCredMount injects the ceiling's operator-blessed Claude credential
// mounts into a composed run's FINAL (post-clamp) spec. Like applyWorkspace, it
// runs AFTER the clamp so composer.Clamp's invariant is untouched: the model can
// never propose a mount; a host path enters a composed run only via deterministic
// server code copying the OPERATOR's own ceiling entries verbatim (Source, Target,
// and the ceiling's resolved ReadOnly).
//
// It injects ONLY when every gate holds, and explains itself when one doesn't:
//   - the run half is the resolved integration (resident_host anthropic_subscription);
//   - the agent is Claude (there is no Codex/OpenAI subscription-mount path);
//   - the ceiling blesses the /home/agent/.claude mount (operator staged creds);
//   - the final egress allows api.anthropic.com — mounting a resident OAuth
//     credential the agent cannot use is pure downside, so refuse.
func applyLLMCredMount(spec *types.RunPolicySpec, ceiling types.RunPolicySpec, agent string, requested bool) (bool, []string) {
	if !requested {
		return false, nil
	}
	if agent != "claude-code" {
		return false, []string{fmt.Sprintf(
			"subscription mode ignored: agent %q has no subscription-mount path (Claude-only); the api-key path applies", agent)}
	}
	if !ceilingBlessesClaudeCreds(ceiling) {
		return false, []string{
			"subscription mode requested but the operator policy does not bless a Claude credential mount " +
				"(a workspace_mount targeting " + claudeCredTarget + "). Stage credentials with scripts/stage-claude-creds.sh " +
				"and point WARDYN_DEFAULT_POLICY at the generated policy, then re-compose."}
	}
	if !anthropicReachable(spec) {
		return false, []string{
			"subscription mode requested but the clamped policy does not allow api.anthropic.com egress, so the " +
				"credential mounts were NOT injected (a resident credential the agent cannot use is pure risk). " +
				"The operator ceiling must list *.anthropic.com (or api.anthropic.com) verbatim."}
	}
	var warns []string
	injected := false
	for _, wm := range ceiling.WorkspaceMounts {
		if wm.Target != claudeCredTarget && wm.Target != claudeCredJSONTarget {
			continue // only the credential mounts; the ceiling may bless others for other purposes
		}
		if specHasMountTarget(spec, wm.Target) {
			continue
		}
		spec.WorkspaceMounts = append(spec.WorkspaceMounts, wm)
		injected = true
	}
	if injected && !specHasMountTarget(spec, claudeCredJSONTarget) {
		// The CLI needs ~/.claude.json (account config) too — without it the
		// proven recipe fails subscription detection inside the sandbox.
		warns = append(warns, "subscription mounts injected WITHOUT a "+claudeCredJSONTarget+
			" companion (the ceiling does not bless one); the Claude CLI may not detect the account")
	}
	return injected, warns
}

// subscriptionLane reads an anthropic_subscription integration's Config.lane
// ("managed" | "resident_host"; "" behaves as "managed" — the same fallback
// capabilitiesFor's subscriptionCaps documents for an unset/unrecognized
// value, integrations.go).
func subscriptionLane(integ types.Integration) string {
	lane, _ := integ.Config["lane"].(string)
	return lane
}

// applyIntegrationCreds folds a resolved AI-provider Integration into the
// run's policy — the Integration-based successor to the pre-Integration
// Mode-switch (git history: `git show ecc1903~1:internal/api/llmcred.go`,
// applyWorkspaceCreds's api_key/managed/bedrock cases). Returns the
// Integration Type actually applied ("" = no-op: a non-LLM agent, an
// unresolvable/absent credential, or a type with no sandbox lane at all —
// per capabilitiesFor) and, for a bedrock
// integration with a region/model override, that override for dispatch to
// resolve against (dispatchParams.BedrockRef).
//
// The switch below is now a base-component fold with TWO exceptions, not a
// per-kind table: the default branch takes any row — an api-key AI kind or a
// generic connection — and turns its proxy-header secret into one api_key grant
// plus its egress into allowlist entries. anthropic_subscription and bedrock
// keep bespoke branches because their credential is genuinely not an HTTP
// header (an OAuth mount/inject lane; SigV4 request signing), and no key
// has no sandbox lane at all.
//
// model_api is deliberately NEVER granted here: resolving an integration for a
// run grants the TOOL's ability to sign in (the harness), never ambient direct
// model access for the sandbox WORKLOAD — that comes only from a workspace's
// secret: requirement (applyRequiredSecretGrant, runs_create.go) or an
// explicit run grant, never from an integration binding.
func (s *Server) applyIntegrationCreds(ctx context.Context, spec *types.RunPolicySpec, integ types.Integration, agent string) (kind string, bedrockRef *types.WorkspaceBedrockRef) {
	// AI kinds only, even though the default branch below is kind-agnostic: this
	// function answers "what credentials the run's MODEL". A generic connection
	// is not a model provider, and resolveRunIntegration already refuses one at
	// every tier of the ladder — a generic row reaches a run through the
	// workspace's own `integration:<id>` requirement instead
	// (applyIntegrationRequirement), which folds the same two halves.
	if !types.AIProviderKind(integ.Kind) {
		return "", nil
	}
	p, ok := agentLLMProvider(agent)
	if !ok {
		return "", nil // non-LLM agent — nothing to bind
	}
	// AGENT×PROVIDER COMPATIBILITY (SPINE-1, security): the harness catalog
	// (harness.go) is the single source of truth for which AI provider KIND can
	// drive which agent, and it is consulted here so a run can never fold an
	// incompatible integration onto the agent's OWN provider host. Without this an
	// openai_api_key integration on a claude-code run injected the operator's
	// OpenAI key as x-api-key on api.anthropic.com (credential disclosed to the
	// wrong vendor), and a subscription/bedrock pin on a codex-cli run silently
	// removeAPIKeyGrantForHost'd its working OpenAI grant. Bail with no grant and
	// no removal; the reason is surfaced to the operator via capabilitiesFor and
	// (for a composed/preflight run) the honest "no model access" verdict.
	if harnessProviderReason(agent, integ.Kind) != "" {
		return "", nil
	}
	switch integ.Kind {
	case types.IntegrationKindAnthropicSubscription:
		// Both lanes (managed / resident_host) displace a competing api-key
		// grant and ensure Anthropic egress — the part common to the old
		// pre-Integration managed-mode case. The resident_host lane ADDITIONALLY
		// needs the ceiling mount; the caller applies that via
		// applyLLMCredMount (THE single subscription gate) since only it
		// knows the ceiling — this function only ever sees the resolved spec.
		removeAPIKeyGrantForHost(spec, p.host)
		for _, d := range []string{"*.anthropic.com", p.host} {
			if !spec.AllowAllEgress && !domainAllowedExact(spec.AllowedDomains, d) {
				spec.AllowedDomains = append(spec.AllowedDomains, d)
			}
		}
		return integ.Kind, nil
	case types.IntegrationKindBedrock:
		removeAPIKeyGrantForHost(spec, p.host)
		region, _ := integ.Config["region"].(string)
		model, _ := integ.Config["model"].(string)
		if region == "" && model == "" {
			return integ.Kind, nil // inherit the global Bedrock config
		}
		// Widen egress only when this row actually names a region: an empty
		// region (row sets model only, region inherits from the global config)
		// would otherwise build a malformed "bedrock-runtime..amazonaws.com"
		// double-dot host here — the real region is resolved later at dispatch.
		if region != "" && !spec.AllowAllEgress {
			unionAllowedDomains(spec, []string{bedrockRuntimeHost(region), bedrockControlHost(region)})
		}
		return integ.Kind, &types.WorkspaceBedrockRef{Region: region, Model: model}
	default:
		// UNIFORM FOLD — the base-component default: an api-key AI kind and a
		// generic connection are the same thing here. The row's proxy-header
		// credential becomes ONE api_key grant on the agent's provider host, and
		// the row's own egress joins the allowlist. The two kinds above keep
		// bespoke transports because they genuinely are not header credentials
		// (an OAuth mount/inject lane; SigV4 signing via WorkspaceBedrockRef).
		secret, header, format := integrationKeyGrant(integ, p)
		if secret == "" || !s.secretPresent(ctx, secret) {
			return "", nil // absent secret would fail the proxy closed — fall back rather than hard-fail
		}
		if _, exists := apiKeyGrantForHost(spec, p.host); exists {
			return integ.Kind, nil // an api_key grant for this host was already proposed; respect it
		}
		scope, _ := json.Marshal(map[string]string{
			"host": p.host, "header": header, "format": format, "secret_name": secret,
		})
		spec.EligibleGrants = append(spec.EligibleGrants, types.GrantSpec{
			Kind: types.GrantAPIKey, Scope: scope, TTLSeconds: 3600, RequiresApproval: false,
		})
		// Couple the exact-host egress entry UNCONDITIONALLY, even under allow-all
		// (SPINE-4): the proxy's credential injector consults the exact allowlist
		// only and deliberately does NOT honor allow-all (Policy.AllowedExactHost),
		// so a grant whose host is missing from AllowedDomains fails buildInjector
		// CLOSED at startup and the sandbox gets zero egress. Same rule the four
		// dispatch/integration-side authors already follow (integrations_run.go).
		if !domainAllowedExact(spec.AllowedDomains, p.host) {
			spec.AllowedDomains = append(spec.AllowedDomains, p.host)
		}
		// The row's OWN egress — where the system lives — comes along, the same
		// half applyIntegrationRequirement folds for a workspace-named row. Under
		// allow-all there is nothing to add (the injector's exact entry above is
		// added regardless, for the reason stated there).
		if !spec.AllowAllEgress {
			unionAllowedDomains(spec, integ.Egress)
		}
		return integ.Kind, nil
	}
}

// integrationKeyGrant is the (secret, header, format) triple a row contributes
// to the run's model-credential grant: the "api_key"-role secret when the row
// names one (the convention every AI provider row uses), else its
// proxy_header-delivered secret whatever role it carries — role-agnostic, per
// the base-component model.
//
// The row's OWN declared delivery wins where it states one; p (the harness
// catalog's Gateway convention — harness.go) is the fallback for a row that
// declares none, which is every legacy-folded and every derived AI row.
func integrationKeyGrant(integ types.Integration, p llmProvider) (secret, header, format string) {
	for _, s := range integ.Secrets {
		if s.Role != "api_key" {
			continue
		}
		if d := s.Delivery; d != nil && d.Mode == types.DeliveryProxyHeader {
			return s.SecretName, d.Header, cmp.Or(d.Format, "%s")
		}
		return s.SecretName, p.header, p.format
	}
	if name, h, f, ok := integ.HeaderSecret(); ok {
		return name, h, f
	}
	return "", "", ""
}

// resolveRunIntegration resolves the FULL run-level integration precedence:
//
//  1. integrationID (run-explicit: createRunRequest.IntegrationID /
//     composeRequest.IntegrationID) when set — must already be known to name
//     an AI-provider integration (validated at request time with a 400; see
//     decodeAndValidateCreateRun / handleComposeRun) or this tier yields
//     nothing rather than guessing.
//  2. workspaceRef — the primary workspace's LLMCred.IntegrationRef, when it
//     names an AI-provider integration. ok=false — today's honest "no
//     binding" outcome — for an empty ref, a ref naming nothing at all, or a
//     ref naming something non-AI-provider: a dangling/miscategorized ref
//     falls back silently rather than cascading to tier 3 or erroring, since
//     the operator who bound THIS workspace explicitly chose a SPECIFIC
//     integration, and a stale binding silently promoting to a different one
//     is a credential surprise, not a convenience.
//  3. the operator's DefaultFor:agent_runs default (any AI-provider type).
//
// ok=false is today's honest no-model-access / global-provider-config
// fallback, carried through every tier unchanged.
func (s *Server) resolveRunIntegration(ctx context.Context, integrationID string, workspaceRef string) (types.Integration, bool) {
	if integrationID != "" {
		in, ok := s.resolveIntegrationRef(ctx, integrationID)
		// bug-integrations-1: a Disabled row must never fold into a run's
		// model access — this is the actual agent-run credential path
		// (applyIntegrationCreds authors EligibleGrants/AllowedDomains from
		// whatever this returns), mirroring applyIntegrationRequirement's
		// existing check on the probe path. Every tier below shares this
		// same refusal.
		if !ok || !types.AIProviderKind(in.Kind) || in.Disabled {
			return types.Integration{}, false
		}
		// SECMODEL-3: a resident_host subscription mounts the OPERATOR'S OWN
		// resident ~/.claude credentials — §5.1a's consent model is "a
		// workspace pin or the operator's DefaultFor:agent_runs default", a
		// durable WORKSPACE property (74d1b16), never a bearer token any run
		// author may claim by naming its id. This tier may carry a
		// resident_host lane ONLY when the run's OWN primary workspace is
		// pinned to that EXACT integration — otherwise it refuses (yields
		// nothing, same as an unresolvable/miscategorized id above) rather
		// than handing a run whose task an attacker authored a live,
		// refreshable copy of the operator's OAuth credentials because it
		// named a workspace the operator never pinned.
		if in.Kind == types.IntegrationKindAnthropicSubscription && subscriptionLane(in) == "resident_host" && workspaceRef != integrationID {
			return types.Integration{}, false
		}
		return in, true
	}
	if workspaceRef != "" {
		// A SET workspace ref that fails to resolve to an AI-provider row
		// (dangling, or naming something else entirely) is the honest "no
		// binding" outcome — return here rather than falling through to tier
		// 3: the operator who bound THIS workspace chose a SPECIFIC
		// integration, and cascading a stale/miscategorized ref to the
		// site-wide default would be the exact credential surprise this
		// tier's doc above says it refuses.
		in, ok := s.resolveIntegrationRef(ctx, workspaceRef)
		if !ok || !types.AIProviderKind(in.Kind) || in.Disabled {
			return types.Integration{}, false
		}
		return in, true
	}
	return s.defaultAgentRunsIntegration(ctx, "")
}

// foldRunIntegration resolves the run's FULL integration precedence and folds
// the winner into spec — the AUDIT-FREE fold (run-explicit integration_id →
// workspace binding → operator default → none): the credential/egress fold
// (applyIntegrationCreds) plus, for a resident_host subscription, the ceiling
// mount via applyLLMCredMount (THE single subscription gate) — returning what
// was applied so the caller can decide whether to audit. The create path
// (runs.go) emits the run.workspace.creds audit itself, once the run id is
// minted, so the fold can run ABOVE the confinement floor (SPINE-2) and
// preflight can call the SAME fold and discard the result.
//
// Both launch and preflight call THIS, so Review cannot predict a different
// model access than launch grants. Preflight used to fold only the workspace
// tier — it had the createRunRequest all along, so an explicit integration_id
// or an operator's site-wide default simply went unseen in the checklist, and
// a run whose model access came from either would preview as having none.
// kind == "" means nothing was bound (no integration resolved, a non-LLM
// agent, or a resolved integration whose fold applied nothing).
func (s *Server) foldRunIntegration(ctx context.Context, spec *types.RunPolicySpec, req createRunRequest, wsRefs []types.Workspace) (types.Integration, string, *types.WorkspaceBedrockRef) {
	// The run's PRIMARY workspace is wsRefs[0] when the spec references any —
	// shared with preflight (which calls this same function) so the two
	// cannot disagree about whose credential binding a run inherits.
	// An exec run (task-mode=exec — a plain governed shell command, "no agent, no
	// LLM credentials" per its `wardyn run --task-mode exec` contract) makes no
	// model call, so it binds NO model integration. Without this, an operator's
	// site-wide default (or a workspace-bound) AI-provider integration would fold
	// an api-key grant AND append the provider host to egress, and persistRunGrants
	// would inject the operator's key proxy-side — the same implicit credential the
	// exec contract forbids, on the api-key transport. resolveLLMTransport already
	// gates the subscription/managed/Bedrock transports on the same no-model-call
	// rule; this closes the grant-folding path. Shared with preflight (calls this),
	// so the checklist's model-access verdict for an exec run matches launch.
	if req.TaskMode == "exec" {
		return types.Integration{}, "", nil
	}
	var workspaceRef string
	if len(wsRefs) > 0 && wsRefs[0].LLMCred != nil {
		workspaceRef = wsRefs[0].LLMCred.IntegrationRef
	}
	if _, ok := agentLLMProvider(req.Agent); !ok {
		return types.Integration{}, "", nil // non-LLM agent — nothing to bind
	}
	integ, ok := s.resolveRunIntegration(ctx, req.IntegrationID, workspaceRef)
	if !ok {
		return types.Integration{}, "", nil
	}
	kind, bedrockRef := s.applyIntegrationCreds(ctx, spec, integ, req.Agent)
	if kind == types.IntegrationKindAnthropicSubscription && subscriptionLane(integ) == "resident_host" {
		applyLLMCredMount(spec, s.cfg.DefaultPolicy, req.Agent, true)
	}
	return integ, kind, bedrockRef
}

// secretPresent reports whether a secret name exists in the store (best-effort;
// a store error reads as "present" so a transient List failure never silently
// drops a legitimately-configured workspace binding — the proxy still fails
// closed at startup if it's truly absent).
func (s *Server) secretPresent(ctx context.Context, name string) bool {
	if s.cfg.Secrets == nil {
		return false
	}
	names, err := s.cfg.Secrets.List(ctx)
	if err != nil {
		return true
	}
	return slices.Contains(names, name)
}

// ensureLLMGrant gives a COMPOSED run for an LLM-backed agent a path to its model.
// A composed run defaults to api-key mode: model calls go through the proxy's
// brokered /wardyn/llm route, which returns 404 "no_llm_credential" unless an
// auto-mint api_key grant injects the provider key. The analyzer reasons about
// the TASK's egress, not the agent's OWN model channel, so it routinely omits
// this (observed: a "no network needed" static-site task proposed zero grants and
// the agent silently produced nothing).
//
// When the run's resolved integration puts it in SUBSCRIPTION mode
// (subscribed=true: the run half is the resolved integration — resident_host
// anthropic_subscription — plus a ceiling-blessed cred mount plus Claude), it
// instead proposes the subscription egress entries (*.anthropic.com + the exact
// host) pre-clamp — the ceiling must list them verbatim to keep them (the clamp's
// allowlist intersection is exact-string) — and adds NO api_key grant: the
// resolved transport choice is respected, not silently doubled up. The cred
// mounts themselves are injected post-clamp by applyLLMCredMount.
//
// It adds BOTH the api_key grant AND its provider host as an EXACT allowlist entry:
// the proxy's injector fails CLOSED at startup unless the injected host is exactly
// allowlisted (buildInjector -> AllowedExactHost), so a grant without its egress
// entry would hard-FAIL the run. The two are a coupled unit.
//
// SECRET-AWARE and non-breaking: an auto-mint api_key grant whose secret is absent
// ALSO fails the proxy at startup (resolveInjection), so the grant is added ONLY
// when the provider secret is stored. It runs BEFORE the clamp (the operator ceiling
// still governs grant AND domain), and never overrides a grant already proposed for
// the same host.
//
// It emits NO warning: whether the run actually ENDS UP with model access is decided
// after the clamp (which may strip the grant or the domain), so reconcileLLMAccess
// reports the truthful FINAL state — never a pre-clamp promise the clamp revokes.
func ensureLLMGrant(spec *types.RunPolicySpec, agent string, secretPresent map[string]bool, subscribed bool) {
	p, ok := agentLLMProvider(agent)
	if !ok {
		return // non-LLM / unknown agent
	}
	if subscribed {
		// Subscription transport: propose the egress entries only (survive iff the
		// ceiling lists them verbatim); no grant, no secret, no injection.
		for _, d := range []string{"*.anthropic.com", p.host} {
			if !spec.AllowAllEgress && !domainAllowedExact(spec.AllowedDomains, d) {
				spec.AllowedDomains = append(spec.AllowedDomains, d)
			}
		}
		return
	}
	if _, exists := apiKeyGrantForHost(spec, p.host); exists {
		return // respect an api_key grant already proposed for this provider host
	}
	if !secretPresent[p.secret] {
		return // adding a grant with no secret would fail the proxy at startup
	}
	scope, _ := json.Marshal(map[string]string{
		"host": p.host, "header": p.header, "format": p.format, "secret_name": p.secret,
	})
	// TTL 3600 mirrors the broker/clamp 1h ceiling (the clamp caps it regardless).
	spec.EligibleGrants = append(spec.EligibleGrants, types.GrantSpec{
		Kind: types.GrantAPIKey, Scope: scope, TTLSeconds: 3600, RequiresApproval: false,
	})
	// Couple the exact-host egress entry (required by the injector) UNCONDITIONALLY,
	// even under allow-all (SPINE-4): AllowedExactHost does not honor allow-all, so
	// a grant without its exact allowlist entry fails buildInjector closed at
	// startup and the sandbox gets zero egress; dedup.
	if !domainAllowedExact(spec.AllowedDomains, p.host) {
		spec.AllowedDomains = append(spec.AllowedDomains, p.host)
	}
}

// subscriptionInjectEnabled reports whether subscription runs will inject the
// operator's LIVE OAuth token proxy-side (the safe default: MITM auto-enabled,
// sandbox holds an inert sentinel) vs. fall back to the resident-copy behavior
// (no token provider wired, or the WARDYN_SUBSCRIPTION_INJECT=off escape hatch).
func (s *Server) subscriptionInjectEnabled() bool {
	return s.cfg.SubscriptionToken != nil && !s.cfg.DisableSubscriptionInject
}

// composeLLMAccess is the structured model-access verdict for a run's resolved
// LLM credential (resolveRunLLMAccess, runs.go) so a review/setup surface need
// never prose-sniff a warning to tell "this run will do nothing" from "tightened
// by policy". Despite the name (a holdover from the deleted AI Run Composer,
// which first introduced this verdict shape), it backs the general create-run
// path — every run's model-access check, not an AI-composed one.
type composeLLMAccess struct {
	Provisioned bool   `json:"provisioned"`
	Note        string `json:"note"`
}

// reconcileLLMAccess inspects the FINAL (post-clamp) spec and returns ONE
// authoritative, deterministic statement of the composed LLM run's model access —
// either a positive "provisioned" note or an honest "no model access" warning
// (empty only for a non-LLM agent). Returning a line in BOTH cases matters: the
// analyzer (an LLM) may emit its own non-deterministic caution about the agent's
// model channel, so this line is the ground-truth that resolves any contradiction.
//
// It runs AFTER the clamp so it never over-promises: model access requires an
// auto-mint api_key grant for the provider host that SURVIVED the clamp, its secret
// stored, AND the host exactly egress-allowed (the injector's hard requirement).
//
// It also PREVENTS a hard failure: if a grant survived but its exact-host egress
// entry did NOT (an incoherent ceiling that brokers api_key but bars the host), the
// proxy would fail closed at startup — so the orphaned grant is DROPPED here, letting
// the run degrade to no-model-access instead of dying, and the warning explains why.
// The subscription-mount remedy is named only for Anthropic (Claude-only path).
//
// SUBSCRIPTION mode is detected from the FINAL spec exactly the way dispatch
// detects it (a mount targeting /home/agent/.claude — internal/api/runs.go), so
// this note can never disagree with what the run will actually do. One launch-time
// interaction is pre-flighted here: dispatch fail-closes a subscription run whose
// policy sets require_inspectable_llm with an active mode and no intercept_tls
// (the subscription tunnel is opaque). That exact predicate — and only that
// predicate; the default require_inspectable_llm=false merely degrades visibly —
// is surfaced as a warning so the human learns at review time, not at launch.
//
// Returns (note, provisioned): note is the human sentence ("" for a non-LLM agent,
// where there is nothing to verify); provisioned is the STRUCTURED verdict — true
// when the run will reach its model, false when it will launch but 404 on the first
// model call. The caller surfaces false as a blocking acknowledgement, not a benign
// clamp notice, so the two can never be conflated by prose-sniffing.
func reconcileLLMAccess(spec *types.RunPolicySpec, agent string, secretPresent map[string]bool, subscriptionInject, managed bool) (string, bool) {
	p, ok := agentLLMProvider(agent)
	if !ok {
		return "", true
	}
	// Managed subscription (compose, no host ~/.claude): the token is injected
	// proxy-side from the store; dispatch adds api.anthropic.com egress
	// unconditionally, so this run WILL reach the model. Drop any api-key grant
	// that rode along (the human chose subscription).
	if agent == "claude-code" && managed {
		removeAPIKeyGrantForHost(spec, p.host)
		return fmt.Sprintf(
			"model access provisioned for agent %q: your Wardyn-managed Claude subscription (setup-token) is injected "+
				"PROXY-SIDE — Wardyn enables TLS-MITM of api.anthropic.com and swaps in the managed token. The sandbox holds "+
				"only an inert sentinel (no host credential is mounted or resident).", agent), true
	}
	if agent == "claude-code" && specHasMountTarget(spec, claudeCredTarget) && anthropicReachable(spec) {
		// Subscription is this run's chosen transport: drop any provider api_key
		// grant that rode along (the model sometimes proposes one). Least
		// privilege — the human chose subscription, not a standing brokered key —
		// and fail-safe: an auto-mint grant whose secret is absent would fail the
		// proxy closed at startup and hard-kill the launch this note promises.
		removeAPIKeyGrantForHost(spec, p.host)
		if subscriptionInject {
			// Safe DEFAULT: Wardyn auto-enables TLS-MITM of api.anthropic.com and
			// injects a live, host-refreshed OAuth token. The staged .credentials.json
			// carries only inert sentinel tokens (access + refresh both replaced), so no
			// usable credential is resident in the sandbox and it never goes stale.
			return fmt.Sprintf(
				"model access provisioned for agent %q: your Claude subscription is injected PROXY-SIDE — Wardyn "+
					"auto-enables TLS-MITM of api.anthropic.com and swaps in a live, host-refreshed OAuth token. The "+
					"sandbox's staged copy holds only inert sentinel tokens, so no usable credential is resident and it never goes stale.",
				agent), true
		}
		// Escape hatch (WARDYN_SUBSCRIPTION_INJECT=off or no token provider): the
		// legacy resident-copy path. The staged credential is mounted and CAN go
		// stale, and the opaque tunnel is uninspectable without intercept_tls.
		if li := spec.LLMInspection; li != nil && li.RequireInspectableLLM &&
			li.Mode != "" && !strings.EqualFold(li.Mode, "off") && !li.InterceptTLS {
			return "this proposal will FAIL at launch: the policy sets require_inspectable_llm with llm_inspection " +
				"active, but subscription transport is an opaque tunnel and intercept_tls is off — dispatch refuses " +
				"such a run. Enable intercept_tls, drop require_inspectable_llm, or use the api-key path.", false
		}
		return fmt.Sprintf(
			"model access provisioned for agent %q: your Claude subscription credentials (operator-staged copies) are "+
				"mounted read-only and the CLI tunnels directly to api.anthropic.com. Note: subscription-inject is OFF, so "+
				"the credential is resident in the sandbox for this run and CAN go stale; the api-key path keeps it proxy-side.",
			agent), true
	}
	g, has := apiKeyGrantForHost(spec, p.host)
	// EXACT-ONLY (SPINE-4): the injector consults the exact allowlist and does not
	// honor allow-all, so a grant is genuinely reachable only when its host is an
	// EXACT allowlist entry. An allow-all policy with an api_key grant whose host
	// is NOT exactly listed still fails the proxy closed — so this must not read
	// allow-all as "host allowed", or the orphan-grant drop below never fires in
	// the one case that hard-fails the run.
	hostAllowed := domainAllowedExact(spec.AllowedDomains, p.host)

	// The verdict keys on the GRANT's own secret when one exists — a workspace
	// credential binding folds in a grant naming the workspace's secret, which
	// need not be the provider convention name. Falling back to p.secret keeps
	// the no-grant CTA pointing at the name a composed run would use.
	secret := p.secret
	if has {
		if s := apiKeyGrantScopeSecret(g.Scope); s != "" {
			secret = s
		}
	}

	if has && !g.RequiresApproval && secretPresent[secret] && hostAllowed {
		// Authoritative positive note (overrides any stale analyzer caution).
		return fmt.Sprintf(
			"model access provisioned for agent %q: an auto-mint api_key grant for %s (via the %q secret) is injected proxy-side — the key is never resident in the sandbox.",
			agent, p.host, secret), true
	}
	// A surviving grant whose host is NOT egress-allowed would fail the proxy at
	// startup — drop it so the run degrades instead of hard-failing.
	if has && !hostAllowed {
		removeAPIKeyGrantForHost(spec, p.host)
	}

	subHint := ""
	if p.host == "api.anthropic.com" {
		subHint = ", or launch this proposal from the wizard with your Claude subscription mounted (the composer cannot mount host credentials)"
	}
	switch {
	case !secretPresent[secret]:
		// Drop a surviving grant whose secret is absent: an auto-mint injection
		// grant with no resolvable secret fails the proxy CLOSED at startup
		// (injection.go), hard-killing the launch — degrade to honest
		// no-model-access instead. (Latent pre-existing hazard: the model can
		// propose an api_key grant the ceiling blesses while no secret is stored.)
		if has {
			removeAPIKeyGrantForHost(spec, p.host)
		}
		return fmt.Sprintf(
			"no model access for agent %q: a composed run brokers its model key from the %q secret, which is not stored. Add it under Secrets and re-compose%s.",
			agent, secret, subHint), false
	case has && g.RequiresApproval:
		return fmt.Sprintf(
			"no model access for agent %q: the operator policy forces approval on the api_key grant, so it is not auto-injected and the model call 404s. Use a policy with an auto-mint api_key grant%s.",
			agent, subHint), false
	default:
		return fmt.Sprintf(
			"no model access for agent %q: the operator policy does not broker an auto-mint api_key grant for %s with matching egress, so the model credential cannot be injected. Use an api_key-capable policy that also allows %s egress (e.g. examples/policies/composer-dev.json)%s.",
			agent, p.host, p.host, subHint), false
	}
}
