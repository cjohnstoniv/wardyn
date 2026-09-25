// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// errModelProviderRefused marks a record launch refused by its model-provider
// choice, so the handler answers the create door's 422 with the sentence alone.
var errModelProviderRefused = errors.New("model provider refused")

// recordProviderChoice is a record session's model-provider choice — the same
// resolution order a run makes at create (chooseModelProvider), with the
// workspace's pin and no request, and the same liveness check on the
// launcher's own credential. The zero choice (governs=false) with no provider
// block. A site config that cannot be read refuses rather than falls back to
// the legacy lanes.
func (s *Server) recordProviderChoice(ctx context.Context, actor string, ws types.Workspace) (runProviderChoice, error) {
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return runProviderChoice{}, fmt.Errorf("read model providers: %w", err)
	}
	if sc.ModelProviders == nil {
		return runProviderChoice{}, nil
	}
	var pin string
	if ws.LLMCred != nil {
		pin = ws.LLMCred.ProviderRef
	}
	choice, err := chooseModelProvider(sc, stepRunAgent, "", pin, func(id string) (bool, error) {
		return s.capSeamAllowed(ctx, capModelProvider, id)
	})
	if err != nil {
		return runProviderChoice{}, fmt.Errorf("resolve capability: %w", err)
	}
	refuse := func(msg string) (runProviderChoice, error) {
		return runProviderChoice{}, fmt.Errorf("%w: %s", errModelProviderRefused, msg)
	}
	switch {
	case choice.refusal != "":
		return refuse(choice.refusal)
	case choice.chosen:
		_, d, cerr := s.providerLiveness(ctx, choice.provider, stepRunAgent, runIdentitySubject(ctx, actor), false)
		if cerr != nil {
			return runProviderChoice{}, fmt.Errorf("read model provider credential: %w", cerr)
		}
		if d.msg != "" {
			return refuse(d.msg)
		}
	}
	choice.governs = true
	return choice, nil
}

// recordSessionModelAccess is a record session's model access. Under a
// provider block it is the chosen provider's, which dispatch authors, and
// nothing is folded here. Otherwise it is the legacy fold: the workspace's own
// binding, then the operator default, then the ceiling's subscription mount or
// a brokered api-key grant — minted here so dispatch hands it to the proxy.
// hadInjections says whether the requirement fold already minted any, which
// the api-key label counts too. Returns the session's llm_mode label.
func (s *Server) recordSessionModelAccess(ctx context.Context, runID uuid.UUID, now time.Time, policy *types.RunPolicySpec,
	ws types.Workspace, mp runProviderChoice, hadInjections bool,
) (string, *types.WorkspaceBedrockRef, []runner.InjectionGrant, error) {
	if mp.governs {
		switch {
		case !mp.chosen:
			return "none", nil, nil, nil
		case mp.provider.Kind == types.ModelProviderAnthropicSubscription:
			return "subscription", nil, nil, nil
		case mp.provider.Kind.IsBedrock():
			return "bedrock", nil, nil, nil
		}
		return "api-key", nil, nil, nil
	}
	// llmGrantsBefore fences the fallback mint below to ONLY what IT adds: the
	// fold above already minted and audited the requirement grants — reusing
	// the full policy.EligibleGrants slice there would remint and re-inject
	// every one of them a second time.
	llmGrantsBefore := len(policy.EligibleGrants)

	// Unconditional, same as launch/preflight for a real run:
	// foldRunIntegration already resolves the workspace's OWN binding first and only
	// falls through to the operator's site-wide DefaultFor:agent_runs integration when
	// the workspace names nothing — it returns kind=="" when neither resolves, so the
	// ceiling/convention fallback below stays the last resort exactly as before. Gating
	// this call on the workspace carrying its own binding skipped tier 3 (the operator's
	// site-wide default) for every unbound workspace's record/replay session, silently
	// diverging from "Model access resolves" (docs/OPERATIONS.md).
	_, integKind, bedrockRef := s.foldRunIntegration(ctx, "", policy, createRunRequest{Agent: "claude-code"}, []types.Workspace{ws})
	subMounted := specHasMountTarget(policy, claudeCredTarget)
	if integKind == "" && !subMounted {
		// No workspace/operator integration bound: fall back to the operator
		// ceiling's convention subscription mount, else a brokered api-key grant
		// (today's behavior for an unbound workspace).
		if m, _ := applyLLMCredMount(policy, s.cfg.DefaultPolicy, "claude-code", true, s.anthropicGatewayHostPort()); m {
			subMounted = true
		} else {
			s.ensureLLMGrant(policy, "claude-code", s.presentSecretNames(ctx), false)
		}
	}
	llmMode := "none"
	switch {
	case subMounted, integKind == "anthropic_subscription":
		llmMode = "subscription" // managed subscription is injected proxy-side by dispatch
	case integKind == "bedrock" || bedrockRef != nil:
		llmMode = "bedrock" // dispatch's resolveBedrockAuth wires it from bedrockRef below
	}
	if subMounted {
		return llmMode, bedrockRef, nil, nil
	}
	// Build the injection from whatever api_key grant the FALLBACK just added
	// (llmGrantsBefore: the fold's own grants above are already minted) —
	// mirrors handleCreateRun's api_key branch (a subscription/bedrock fold
	// adds none: managed is injected proxy-side, Bedrock via resolveBedrockAuth).
	minted, err := s.mintRecordAPIKeyInjections(ctx, runID, now, policy.EligibleGrants[llmGrantsBefore:])
	if err != nil {
		return "", nil, nil, fmt.Errorf("create llm grant: %w", err)
	}
	if (hadInjections || len(minted) > 0) && llmMode == "none" {
		llmMode = "api-key"
	}
	return llmMode, bedrockRef, minted, nil
}
