// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// errModelProviderRefused marks a record launch refused by its model-provider
// choice. The error itself is a *modelProviderRefusal, which carries the whole
// choice so the handler answers exactly as the create door does.
var errModelProviderRefused = errors.New("model provider refused")

// errModelProvidersUnreadable marks a site config the provider choice could not
// read: the create door's 503 and bare sentence, never a 500 with driver text.
var errModelProvidersUnreadable = errors.New("model providers unreadable")

// modelProviderRefusal is a record launch's refused provider choice: the
// choice as chooseModelProvider made it, and the liveness verdict when the
// choice was made and its credential was not. Wrapping the choice, rather than
// copying a provider id off it, keeps notGranted and asMissing in force: the
// not-granted arms set providerID for the audit row only, and answering with it
// would name a provider the caller is not granted (#1018).
type modelProviderRefusal struct {
	choice runProviderChoice
	live   providerDenial
}

func (e *modelProviderRefusal) sentence() string { return cmp.Or(e.choice.refusal, e.live.msg) }

func (e *modelProviderRefusal) Error() string {
	return errModelProviderRefused.Error() + ": " + e.sentence()
}

func (e *modelProviderRefusal) Is(target error) bool { return target == errModelProviderRefused }

// recordProviderChoice is a record session's model-provider choice — the same
// resolution order a run makes at create (chooseModelProvider), with the
// workspace's pin and no request, and the same liveness check on the
// launcher's own credential. A site config that cannot be read refuses rather
// than launch a session no provider can be checked for.
func (s *Server) recordProviderChoice(ctx context.Context, actor string, ws types.Workspace) (runProviderChoice, error) {
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return runProviderChoice{}, fmt.Errorf("%w: %w", errModelProvidersUnreadable, err)
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
	switch {
	case choice.refusal != "":
		return runProviderChoice{}, &modelProviderRefusal{choice: choice}
	case choice.chosen:
		_, d, cerr := s.providerLiveness(ctx, choice.provider, stepRunAgent, runIdentitySubject(ctx, actor), false)
		if cerr != nil {
			return runProviderChoice{}, fmt.Errorf("read model provider credential: %w", cerr)
		}
		if d.msg != "" {
			return runProviderChoice{}, &modelProviderRefusal{choice: choice, live: d}
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
