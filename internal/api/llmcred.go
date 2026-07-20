// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

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
// injection convention, or ok=false for a non-LLM / unknown agent.
func agentLLMProvider(agent string) (llmProvider, bool) {
	switch agent {
	case "claude-code":
		return llmProvider{host: "api.anthropic.com", header: "x-api-key", format: "%s", secret: "anthropic-api-key"}, true
	case "codex-cli":
		return llmProvider{host: "api.openai.com", header: "Authorization", format: "Bearer %s", secret: "openai-api-key"}, true
	default:
		return llmProvider{}, false
	}
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

// ceilingBlessesClaudeCreds reports whether the operator ceiling blesses a Claude
// credential mount (a WorkspaceMount targeting /home/agent/.claude). Only the
// operator authors ceiling mounts, so this is the control-plane-level half of the
// subscription consent; the per-run half is composeRequest.UseSubscription.
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
		dl := strings.ToLower(strings.TrimSpace(d))
		if strings.HasSuffix(dl, "anthropic.com") || dl == "api.openai.com" {
			out = append(out, d)
		}
	}
	return out
}

func ceilingBlessesClaudeCreds(ceiling types.RunPolicySpec) bool {
	for _, wm := range ceiling.WorkspaceMounts {
		if wm.Target == claudeCredTarget {
			return true
		}
	}
	return false
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
	for _, wm := range spec.WorkspaceMounts {
		if wm.Target == target {
			return true
		}
	}
	return false
}

// applyLLMCredMount injects the ceiling's operator-blessed Claude credential
// mounts into a composed run's FINAL (post-clamp) spec. Like applyWorkspace, it
// runs AFTER the clamp so composer.Clamp's invariant is untouched: the model can
// never propose a mount; a host path enters a composed run only via deterministic
// server code copying the OPERATOR's own ceiling entries verbatim (Source, Target,
// and the ceiling's resolved ReadOnly).
//
// It injects ONLY when every gate holds, and explains itself when one doesn't:
//   - the human explicitly opted in on THIS request (use_subscription);
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

// ensureLLMGrant gives a COMPOSED run for an LLM-backed agent a path to its model.
// A composed run defaults to api-key mode: model calls go through the proxy's
// brokered /wardyn/llm route, which returns 404 "no_llm_credential" unless an
// auto-mint api_key grant injects the provider key. The analyzer reasons about
// the TASK's egress, not the agent's OWN model channel, so it routinely omits
// this (observed: a "no network needed" static-site task proposed zero grants and
// the agent silently produced nothing).
//
// When the operator explicitly opted into SUBSCRIPTION mode for this request
// (subscribed=true: use_subscription + a ceiling-blessed cred mount + Claude), it
// instead proposes the subscription egress entries (*.anthropic.com + the exact
// host) pre-clamp — the ceiling must list them verbatim to keep them (the clamp's
// allowlist intersection is exact-string) — and adds NO api_key grant: the
// explicit human choice of transport is respected, not silently doubled up. The
// cred mounts themselves are injected post-clamp by applyLLMCredMount.
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
// applyWorkspaceCreds folds the PRIMARY workspace/container's operator-owned
// model/harness cred BINDING (types.WorkspaceLLMCred) into the run's policy at
// create — the credential analogue of unionWorkspaceEgress. A run that picks a
// workspace/container inherits its model access; a workspace that binds nothing
// (nil / Mode="") leaves the run on the global provider config, or a plain
// governed command that needs no model. Refs/names only — the secret is resolved
// and injected at dispatch, never resident. Returns the mode applied (for audit),
// or "" when nothing was bound.
func (s *Server) applyWorkspaceCreds(ctx context.Context, spec *types.RunPolicySpec, primary *types.Workspace, agent string) types.WorkspaceLLMCredMode {
	if primary == nil || primary.LLMCred == nil || primary.LLMCred.Mode == types.WorkspaceLLMCredNone {
		return ""
	}
	p, ok := agentLLMProvider(agent)
	if !ok {
		return "" // non-LLM agent — nothing to bind
	}
	c := primary.LLMCred
	switch c.Mode {
	case types.WorkspaceLLMCredAPIKey:
		// A workspace-specific api_key secret, injected proxy-side (never resident).
		if c.APIKeySecret == "" || !s.secretPresent(ctx, c.APIKeySecret) {
			return "" // absent secret would fail the proxy closed — fall back rather than hard-fail
		}
		if _, exists := apiKeyGrantForHost(spec, p.host); exists {
			return c.Mode // an api_key grant for this host was already proposed; respect it
		}
		scope, _ := json.Marshal(map[string]string{
			"host": p.host, "header": p.header, "format": p.format, "secret_name": c.APIKeySecret,
		})
		spec.EligibleGrants = append(spec.EligibleGrants, types.GrantSpec{
			Kind: types.GrantAPIKey, Scope: scope, TTLSeconds: 3600, RequiresApproval: false,
		})
		if !spec.AllowAllEgress && !domainAllowedExact(spec.AllowedDomains, p.host) {
			spec.AllowedDomains = append(spec.AllowedDomains, p.host)
		}
	case types.WorkspaceLLMCredManaged:
		// The Wardyn-managed subscription injects proxy-side at dispatch when the run
		// has anthropic egress and NO api-key grant for the host (resolveLLMTransport's
		// managed gate). Ensure the egress; drop any competing api-key grant so managed
		// wins. (The managed token itself is a control-plane-wide connected credential.)
		removeAPIKeyGrantForHost(spec, p.host)
		for _, d := range []string{"*.anthropic.com", p.host} {
			if !spec.AllowAllEgress && !domainAllowedExact(spec.AllowedDomains, d) {
				spec.AllowedDomains = append(spec.AllowedDomains, d)
			}
		}
	case types.WorkspaceLLMCredBedrock:
		// Bedrock credentials are resolved at dispatch (resolveBedrockAuth); the
		// binding drops any competing api-key grant so Bedrock is the path taken.
		// A per-workspace REGION overrides the global one there, so allow its
		// regional data+control-plane hosts here — dispatch adds them too, but the
		// egress belongs on the run's policy from create (the composer/preview and
		// any policy echo read this spec, not the dispatch-local copy).
		removeAPIKeyGrantForHost(spec, p.host)
		if b := c.Bedrock; b != nil && b.Region != "" && !spec.AllowAllEgress {
			unionAllowedDomains(spec, []string{bedrockRuntimeHost(b.Region), bedrockControlHost(b.Region)})
		}
	}
	return c.Mode
}

// applyPrimaryWorkspaceCreds resolves the run's PRIMARY workspace/container and
// folds its cred binding into the spec (applyWorkspaceCreds), auditing the mode
// applied. The primary is the first referenced local_dir/repo workspace, or —
// for a bring-your-own-container run — the onboarded CONTAINER workspace whose
// image this run launches. A workspace that binds nothing leaves the run on the
// global provider config (or a plain governed command).
//
// Returns the workspace's Bedrock selection when (and only when) a BEDROCK
// binding was applied, for dispatch to resolve region/model against
// (dispatchParams.BedrockRef); nil means "use the global Bedrock config".
func (s *Server) applyPrimaryWorkspaceCreds(ctx context.Context, runID uuid.UUID, spec *types.RunPolicySpec, req createRunRequest, wsRefs []types.Workspace) *types.WorkspaceBedrockRef {
	var primary *types.Workspace
	switch {
	case len(wsRefs) > 0:
		primary = &wsRefs[0]
	case req.Image != "" && s.cfg.Store != nil:
		if cw, err := s.cfg.Store.GetWorkspaceBySource(ctx, types.WorkspaceKindContainer, req.Image); err == nil {
			primary = &cw
		}
	}
	if primary == nil {
		return nil
	}
	mode := s.applyWorkspaceCreds(ctx, spec, primary, req.Agent)
	if mode == "" {
		return nil
	}
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.workspace.creds",
		runID.String(), "success", mustJSON(map[string]any{"mode": string(mode)})))
	if mode == types.WorkspaceLLMCredBedrock {
		return primary.LLMCred.Bedrock
	}
	return nil
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
	// Couple the exact-host egress entry (required by the injector); dedup.
	if !spec.AllowAllEgress && !domainAllowedExact(spec.AllowedDomains, p.host) {
		spec.AllowedDomains = append(spec.AllowedDomains, p.host)
	}
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
// subscriptionInjectEnabled reports whether subscription runs will inject the
// operator's LIVE OAuth token proxy-side (the safe default: MITM auto-enabled,
// sandbox holds an inert sentinel) vs. fall back to the resident-copy behavior
// (no token provider wired, or the WARDYN_SUBSCRIPTION_INJECT=off escape hatch).
func (s *Server) subscriptionInjectEnabled() bool {
	return s.cfg.SubscriptionToken != nil && !s.cfg.DisableSubscriptionInject
}

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
	hostAllowed := spec.AllowAllEgress || domainAllowedExact(spec.AllowedDomains, p.host)

	if has && !g.RequiresApproval && secretPresent[p.secret] && hostAllowed {
		// Authoritative positive note (overrides any stale analyzer caution).
		return fmt.Sprintf(
			"model access provisioned for agent %q: an auto-mint api_key grant for %s (via the %q secret) is injected proxy-side — the key is never resident in the sandbox.",
			agent, p.host, p.secret), true
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
	case !secretPresent[p.secret]:
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
			agent, p.secret, subHint), false
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
