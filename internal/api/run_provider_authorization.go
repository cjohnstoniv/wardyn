// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (s *Server) authorizeRunModelProvider(r *http.Request, req createRunRequest, wsRefs []types.Workspace, incomplete bool) (runProviderChoice, *runRefusal) {
	ctx := r.Context()
	if req.ModelProvider != "" && !modelProviderIDPattern.MatchString(req.ModelProvider) {
		return runProviderChoice{}, runError(http.StatusBadRequest, reasonModelProviderIDInvalid, fmt.Sprintf(mpRunBadID, req.ModelProvider))
	}
	// createDoorIsModelRun (runs_dispatch_llm_mechanism.go) is the one
	// predicate for which create requests are model runs (#767 step 2), so no
	// two doors ask a different question of the same request.
	_, needsModel := agentLLMProvider(req.Agent)
	if !needsModel || !createDoorIsModelRun(req) {
		if req.ModelProvider != "" {
			return runProviderChoice{}, runError(http.StatusBadRequest, reasonModelProviderNotApplicable, mpRunNoModel)
		}
		return runProviderChoice{}, nil
	}
	var sc types.SiteConfig
	if s.cfg.Store != nil {
		var err error
		if sc, err = s.cfg.Store.GetSiteConfig(ctx); err != nil {
			// The same refusal dispatch makes on the identical read
			// (resolveProviderLane's mpRunUnreadable, runs_dispatch_provider.go):
			// both doors refuse an unreadable provider block (#532) rather than
			// have one 500 with driver text while the other names the cause.
			slog.ErrorContext(ctx, "api: get site config for model-provider choice", slog.Any("err", err))
			// Deliberately bare (#656 slice 3): the sentence alone, matching
			// resolveProviderLane's identical arm — a transient store failure
			// is no door, and TestProviderJoin_DoorsEveryKind pins both
			// "provider-unreadable" and "block-unreadable" reason-less.
			return runProviderChoice{}, runError(http.StatusServiceUnavailable, "", mpRunUnreadable)
		}
	}
	if sc.ModelProviders == nil && req.ModelProvider != "" {
		return runProviderChoice{}, runError(http.StatusUnprocessableEntity, reasonModelProviderNoBlockConfigured, fmt.Sprintf(mpRunNoBlock, req.ModelProvider))
	}
	var pin string
	if len(wsRefs) > 0 && wsRefs[0].LLMCred != nil {
		pin = wsRefs[0].LLMCred.ProviderRef
	}
	choice, err := chooseModelProvider(sc, req.Agent, req.ModelProvider, pin, func(id string) (bool, error) {
		return s.capSeamAllowed(ctx, capModelProvider, id)
	})
	if err != nil {
		return runProviderChoice{}, runServerError("resolve capability", err)
	}
	if incomplete && req.ModelProvider == "" && pin == "" && (choice.notGranted || (choice.refusal != "" && choice.providerID == "")) {
		return runProviderChoice{}, nil
	}
	if choice.asMissing || choice.notGranted || choice.refusal != "" {
		return runProviderChoice{}, providerChoiceRunRefusal(choice)
	}
	return choice, nil
}

func providerChoiceRunRefusal(choice runProviderChoice) *runRefusal {
	if choice.notGranted && !choice.asMissing {
		d := authz.Deny(authz.ReasonCapabilityModelProvider, "runs.model_provider", choice.refusal)
		if choice.providerID != "" {
			d = d.With("provider", choice.providerID)
		}
		return runDenied(d)
	}
	d := authz.Deny(authz.ReasonModelProviderUnavailable, "runs.model_provider", choice.refusal)
	if choice.asMissing {
		d = authz.Deny(authz.ReasonCapabilityModelProvider, "runs.model_provider", choice.refusal)
	}
	if choice.providerID != "" {
		d = d.With("provider", choice.providerID)
	}
	if choice.kind != "" {
		d = d.With("kind", string(choice.kind))
	}
	return &runRefusal{status: http.StatusUnprocessableEntity, decision: &d,
		body: errorBody{Error: choice.refusal, Reason: string(authz.ReasonModelProviderUnavailable), Provider: choice.providerID, Kind: string(choice.kind)}}
}
