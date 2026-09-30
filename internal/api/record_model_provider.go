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
// launcher's own credential. A site config that cannot be read refuses rather
// than launch a session no provider can be checked for.
func (s *Server) recordProviderChoice(ctx context.Context, actor string, ws types.Workspace) (runProviderChoice, error) {
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return runProviderChoice{}, fmt.Errorf("read model providers: %w", err)
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
	return choice, nil
}

// recordSessionModelAccess is a record session's llm_mode label: the chosen
// provider's kind, which dispatch authors, or none. Nothing is folded here.
func recordSessionModelAccess(mp runProviderChoice) string {
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
