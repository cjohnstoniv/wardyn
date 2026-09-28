// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
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

// llmProviderFor is agentLLMProvider with the operator's internal-gateway
// override applied: when s.cfg.LLMGateways configures a gateway for this
// provider's public host, the returned host is the gateway's — everywhere the
// api-key convention's host matters (grant scope, the exact egress-allowlist
// entry, the no-model-access CTA), not just at the proxy's own dial. Every
// caller of agentLLMProvider routes through this method instead; the
// unexported function survives only as this method's implementation.
func (s *Server) llmProviderFor(agent string) (llmProvider, bool) {
	p, ok := agentLLMProvider(agent)
	if !ok {
		return p, false
	}
	// vendorHost is the harness catalog's compile-time public host — the key
	// BOTH override maps use, so it must be read before p.host is possibly
	// rewritten below.
	vendorHost := p.host
	if base, has := s.cfg.LLMGateways[vendorHost]; has {
		if h := gatewayHost(base); h != "" {
			p.host = h
		}
	}
	// The operator's own header/format override (WARDYN_<VENDOR>_GATEWAY_HEADER
	// / _GATEWAY_FORMAT, validated by ValidateLLMGateways) — independent of
	// whether a gateway base URL is also set. Each field applies only if the
	// operator set it; otherwise the harness catalog's vendor convention
	// (already in p.header/p.format) survives untouched.
	if auth, has := s.cfg.LLMGatewayAuth[vendorHost]; has {
		if auth.Header != "" {
			p.header = auth.Header
		}
		if auth.Format != "" {
			p.format = auth.Format
		}
	}
	return p, true
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
func (s *Server) isModelProviderHost(h string) bool {
	hl := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
	if strings.HasSuffix(hl, "anthropic.com") || hl == "api.openai.com" {
		return true
	}
	// An operator-configured internal gateway host counts too — it IS the
	// model-provider host for every run under the api-key lane (6c's
	// filterUserGrants arm and denyAlwaysReject below both need this).
	for _, base := range s.cfg.LLMGateways {
		if gatewayHost(base) == hl {
			return true
		}
	}
	return false
}

// bedrockLaneHosts are the Bedrock hosts that carry proxy-side credential
// injection: the daemon-wide BedrockRegion pair.
//
// The data-plane host goes through s.bedrockDataPlaneHost, so a
// WARDYN_BEDROCK_BASE_URL override (a VPC/PrivateLink endpoint) is picked up
// here for free rather than derived a sixth time; the control host is
// deliberately not overridden, exactly as runs_bedrock.go documents.
func (s *Server) bedrockLaneHosts() []string {
	if r := s.cfg.BedrockRegion; r != "" {
		return []string{s.bedrockDataPlaneHost(r), bedrockControlHost(r)}
	}
	return nil
}

// isModelProviderRejectHost is the REJECT-lane model-provider predicate: every
// host isModelProviderHost names, PLUS the Bedrock lane (bedrockLaneHosts).
//
// It is a SECOND predicate rather than a widening of isModelProviderHost on
// purpose. isModelProviderHost also gates an ACCEPT: inline_policy.go's 6c
// own-key arm admits a member's OWN api_key secret with no operator eligible-grant
// pairing whenever the paired host is a model-provider host. Teaching that
// predicate about bedrock-runtime.<region> would hand the Bedrock lane a new
// unpaired-grant accept as a side effect of fixing a deny guard. The reject
// direction is where the Bedrock lane belongs: resolveBedrockAuth's PREFERRED
// bearer mode TLS-MITMs bedrock-runtime and injects the Authorization header
// proxy-side (runs_bedrock.go), which is exactly the "proxy-side credential
// injection refuses a denied host" failure denyAlwaysReject exists to prevent —
// and promoteSkipHosts (record.go) already had to patch the same predicate gap
// on the promotion lane.
//
// Both REJECT-direction callers of the concept use THIS one, and the sentence is
// meant to be grep-checkable: denyAlwaysReject (approvals_writeback.go) and
// planArtifactRedirect's To-host veto (artifact_redirect.go). The two remaining
// callers of the narrow isModelProviderHost are not reject tests — inline_policy.go's
// 6c own-key ACCEPT arm, and modelProviderEgress, which only filters entries the
// operator already wrote into their own ceiling.
func (s *Server) isModelProviderRejectHost(h string) bool {
	if s.isModelProviderHost(h) {
		return true
	}
	hl := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
	if hl == "" {
		return false
	}
	for _, b := range s.bedrockLaneHosts() {
		if strings.ToLower(strings.TrimSuffix(strings.TrimSpace(b), ".")) == hl {
			return true
		}
	}
	return false
}

// ceilingBlessesClaudeCreds reports whether the operator ceiling blesses a Claude
// credential mount (a WorkspaceMount targeting /home/agent/.claude). Only the
// operator authors ceiling mounts, so this is the control-plane-level half of the
// subscription consent; the run half is the resolved integration (resident_host
// anthropic_subscription).
func ceilingBlessesClaudeCreds(ceiling types.RunPolicySpec) bool {
	return specHasMountTarget(&ceiling, claudeCredTarget)
}

// anthropicReachable reports whether the FINAL spec's egress lets the agent
// reach the host subscription mode will actually dial.
//
// With no gateway configured that is api.anthropic.com: allow-all, an exact
// entry, or a *.anthropic.com wildcard. Subscription mode injects no secret via
// this check, so a wildcard entry is injection-safe here — the injector's
// exact-host rule (AllowedExactHost) is not in play.
//
// With a gateway configured (gatewayHostPort is s.anthropicGatewayHostPort()),
// dispatch points ANTHROPIC_BASE_URL at the gateway (runs_dispatch_llm.go), so
// ONLY the gateway's reachability counts and a vendor-host entry proves
// nothing: judging api.anthropic.com there would mount the resident credential
// into a run whose one model dial the proxy refuses. The question is put to the
// proxy's own evaluator, so an exact entry, a covering wildcard, a :port
// qualifier, allow-all and denied_domains all answer exactly as they will at
// the CONNECT.
func anthropicReachable(spec *types.RunPolicySpec, gatewayHostPort string) bool {
	if gatewayHostPort != "" {
		host, ps, err := net.SplitHostPort(gatewayHostPort)
		port, perr := strconv.Atoi(ps)
		if err != nil || perr != nil {
			return false
		}
		v, err := proxy.NewBuiltinEvaluator(*spec).EvaluateHost(context.Background(),
			egress.Request{Host: strings.ToLower(host), Port: port, Method: http.MethodConnect})
		return err == nil && v == egress.VerdictAllow
	}
	if spec.AllowAllEgress {
		return true
	}
	return slices.ContainsFunc(spec.AllowedDomains, func(d string) bool {
		d = strings.ToLower(strings.TrimSpace(d))
		return d == "api.anthropic.com" || d == "*.anthropic.com"
	})
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
//   - the final egress reaches the host the run will dial: the configured
//     gateway (gatewayHostPort, non-empty only when one is set), else
//     api.anthropic.com — mounting a resident OAuth credential the agent
//     cannot use is pure downside, so refuse.
func applyLLMCredMount(spec *types.RunPolicySpec, ceiling types.RunPolicySpec, agent string, requested bool, gatewayHostPort string) (bool, []string) {
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
	if !anthropicReachable(spec, gatewayHostPort) {
		hostDesc, remedy := "api.anthropic.com", "The operator ceiling must list *.anthropic.com (or api.anthropic.com) verbatim."
		if gatewayHostPort != "" {
			hostDesc = "the configured gateway " + gatewayHostPort
			remedy = fmt.Sprintf("The operator ceiling must allow the gateway %s (its host, or a *. wildcard covering it); "+
				"an api.anthropic.com entry does not count, because the run dials the gateway, not the vendor.", gatewayHostPort)
		}
		return false, []string{fmt.Sprintf(
			"subscription mode requested but the clamped policy does not allow %s egress, so the "+
				"credential mounts were NOT injected (a resident credential the agent cannot use is pure risk). %s",
			hostDesc, remedy)}
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

// subscriptionInjectEnabled reports whether subscription runs will inject the
// operator's LIVE OAuth token proxy-side (the safe default: MITM auto-enabled,
// sandbox holds an inert sentinel) vs. fall back to the resident-copy behavior
// (no token provider wired, or the WARDYN_SUBSCRIPTION_INJECT=off escape hatch).
// SubscriptionPostureOK is ANDed in so this predicate keeps telling the operator
// the truth: off-posture the providers are never constructed, so the token would
// not resolve anyway, and reporting "will inject" would be an overclaim.
func (s *Server) subscriptionInjectEnabled() bool {
	return s.cfg.SubscriptionPostureOK && s.cfg.SubscriptionToken != nil && !s.cfg.DisableSubscriptionInject
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
func (s *Server) reconcileLLMAccess(spec *types.RunPolicySpec, agent string, secretPresent map[string]bool, subscriptionInject, managed bool) (string, bool) {
	p, ok := s.llmProviderFor(agent)
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
	if agent == "claude-code" && specHasMountTarget(spec, claudeCredTarget) && anthropicReachable(spec, s.anthropicGatewayHostPort()) {
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

	// The verdict keys on the GRANT's own secret — a policy's grant may name
	// any secret, not only the provider convention name. With no grant there is
	// no secret to name: nothing authors one from the operator's convention
	// secret any more, so storing it would not give the run model access.
	var secret string
	if has {
		secret = cmp.Or(apiKeyGrantScopeSecret(g.Scope), p.secret)
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

	// Keyed on the PROVIDER (p.secret == the Anthropic convention name), not
	// p.host: under a configured gateway p.host is the gateway's host, not
	// "api.anthropic.com", but the subscription-mount hint still applies to
	// every Anthropic-provider run regardless of which host its api-key lane
	// dials.
	subHint := ""
	if p.secret == "anthropic-api-key" {
		subHint = ", or launch this proposal from the wizard with your Claude subscription mounted (the composer cannot mount host credentials)"
	}
	switch {
	case has && !secretPresent[secret]:
		// Drop a surviving grant whose secret is absent: an auto-mint injection
		// grant with no resolvable secret fails the proxy CLOSED at startup
		// (injection.go), hard-killing the launch — degrade to honest
		// no-model-access instead.
		removeAPIKeyGrantForHost(spec, p.host)
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
