// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"

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
// nothing is folded here. Otherwise it is the operator ceiling's subscription
// mount, when the ceiling blesses one; no model credential is minted here.
// hadInjections says whether the requirement fold already minted any, which
// the api-key label counts. Returns the session's llm_mode label.
func (s *Server) recordSessionModelAccess(policy *types.RunPolicySpec, mp runProviderChoice, hadInjections bool) string {
	if mp.governs {
		switch {
		case !mp.chosen:
			return "none"
		case mp.provider.Kind == types.ModelProviderAnthropicSubscription:
			return "subscription"
		case mp.provider.Kind.IsBedrock():
			return "bedrock"
		}
		return "api-key"
	}
	if specHasMountTarget(policy, claudeCredTarget) {
		return "subscription"
	}
	if m, _ := applyLLMCredMount(policy, s.cfg.DefaultPolicy, "claude-code", true, s.anthropicGatewayHostPort()); m {
		return "subscription" // managed subscription is injected proxy-side by dispatch
	}
	if hadInjections {
		return "api-key"
	}
	return "none"
}
