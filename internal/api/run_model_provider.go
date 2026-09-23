// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"fmt"
	"net/http"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mpRun* — the run-create refusals of a model-provider choice. DRAFT (M2 canon
// pending). mpRunRefusal is the design's one sentence (multi-provider §2.6):
// provider, state, remedy.
const (
	mpRunRefusal         = "This run's model provider is %s, and %s — %s Wardyn does not substitute a different model provider."
	mpRunRemedy          = "choose another model provider, or ask your admin."
	mpRunStateMissing    = "there is no model provider by that name"
	mpRunStateOff        = "it is turned off"
	mpRunStateNotServing = "it is not available to %s"
	mpRunStateNotGranted = "you are not granted it"
	mpRunNoneGranted     = "No model provider that serves %s is granted to you — ask your admin. Wardyn does not substitute a different model provider."
	mpRunChoose          = "Choose a model provider for this run: more than one serves %s, and none is its default."
	mpRunNotYet          = "This run's model provider is %s, and provider dispatch for that kind is not yet available on this build — nothing was started."
	mpRunNoBlock         = "model_provider names %q, but this deployment has no model providers — launch without model_provider."
	mpRunNoModel         = "model_provider applies only to a run that calls a model — a task_mode=exec run or an agent that takes no model provider chooses none."
	mpRunBadID           = "model_provider: %q is not a provider id — lowercase letters, digits and ._- , at most 64 characters"
)

// providerKindDispatched is the kinds whose dispatch arm has landed. A chosen
// provider of any other kind is refused at create rather than dispatched down
// the legacy lane chain, which would serve it from a credential it did not
// choose. ponytail: empty until MP-7 (keys, endpoint), MP-8 (subscription) and
// MP-9 (Bedrock) add their arms; MP-9 deletes the map.
var providerKindDispatched = map[types.ModelProviderKind]bool{}

// runProviderChoice is chooseModelProvider's answer. chosen=false with no
// refusal is "no provider serves this harness": the run launches on today's
// path with today's advisory.
type runProviderChoice struct {
	provider types.ModelProvider
	chosen   bool
	refusal  string
	// notGranted marks a refusal the caller's capability decided — a 403
	// authz.denied row, not an org-configuration 422.
	notGranted bool
}

// chooseModelProvider is the design's resolution order (multi-provider §2.4):
// candidates are the providers that are on, serve agent and pass granted; then
// the requested provider, else the workspace pin, else the harness default,
// else the single candidate. A named provider that is not a candidate is
// refused, never passed over, and so is a disabled default — even with exactly
// one other candidate left. A default the caller is not granted is passed over.
func chooseModelProvider(sc types.SiteConfig, agent, requested, pin string, granted func(id string) (bool, error)) (runProviderChoice, error) {
	var serving, candidates []types.ModelProvider
	for _, p := range modelProviderRows(sc) {
		if p.Disabled || !p.Serves(agent) {
			continue
		}
		serving = append(serving, p)
		ok, err := granted(p.ID)
		if err != nil {
			return runProviderChoice{}, err
		}
		if ok {
			candidates = append(candidates, p)
		}
	}
	isCandidate := func(id string) bool {
		return slices.ContainsFunc(candidates, func(p types.ModelProvider) bool { return p.ID == id })
	}
	if named := cmp.Or(requested, pin); named != "" {
		return judgeNamedProvider(sc, agent, named, isCandidate), nil
	}
	if row, ok := agentProviderFor(sc, agent); ok && row.DefaultProvider != "" {
		d, _ := modelProviderByID(sc.ModelProviders, row.DefaultProvider)
		if d.Disabled {
			return providerRefusal(d.ID, mpRunStateOff), nil
		}
		if isCandidate(d.ID) {
			return runProviderChoice{provider: d, chosen: true}, nil
		}
	}
	switch {
	case len(candidates) == 1:
		return runProviderChoice{provider: candidates[0], chosen: true}, nil
	case len(candidates) > 1:
		return runProviderChoice{refusal: fmt.Sprintf(mpRunChoose, agent)}, nil
	case len(serving) == 0:
		return runProviderChoice{}, nil
	case len(serving) == 1:
		c := providerRefusal(serving[0].ID, mpRunStateNotGranted)
		c.notGranted = true
		return c, nil
	}
	return runProviderChoice{refusal: fmt.Sprintf(mpRunNoneGranted, agent), notGranted: true}, nil
}

// judgeNamedProvider answers for a provider the request or the workspace pin
// named: it is the choice, or the run is refused naming it.
func judgeNamedProvider(sc types.SiteConfig, agent, id string, isCandidate func(string) bool) runProviderChoice {
	p, ok := modelProviderByID(sc.ModelProviders, id)
	switch {
	case !ok:
		return providerRefusal(id, mpRunStateMissing)
	case p.Disabled:
		return providerRefusal(id, mpRunStateOff)
	case !p.Serves(agent):
		return providerRefusal(id, fmt.Sprintf(mpRunStateNotServing, agent))
	case !isCandidate(id):
		c := providerRefusal(id, mpRunStateNotGranted)
		c.notGranted = true
		return c
	}
	return runProviderChoice{provider: p, chosen: true}
}

func providerRefusal(id, state string) runProviderChoice {
	return runProviderChoice{refusal: fmt.Sprintf(mpRunRefusal, id, state, mpRunRemedy)}
}

// enforceRunModelProvider is the model-provider choice at BOTH doors, create
// and Review, so Review answers the refusal launch would. With no provider
// block it changes nothing unless the request named a provider, which it
// refuses rather than ignores. wsRefs[0] is the primary workspace, the one
// whose pin a run inherits (foldRunIntegration reads the same one).
//
// Every provider this chooses is refused for now (providerKindDispatched), so
// a run on a deployment that configured providers never reaches the legacy lane
// chain with a choice it would not honour. Writes its own refusal and returns
// false once it has.
func (s *Server) enforceRunModelProvider(w http.ResponseWriter, r *http.Request, req createRunRequest, wsRefs []types.Workspace) bool {
	ctx := r.Context()
	if req.ModelProvider != "" && !modelProviderIDPattern.MatchString(req.ModelProvider) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(mpRunBadID, req.ModelProvider))
		return false
	}
	// Not llmMechanismGateApplies: that reads workspace_id without interactive
	// as a scan, but this door never sets run.WorkspaceID (seedRequestWorkspace),
	// so dispatch runs such a body as a model run — exactly what the CLI's
	// --workspace and the console send. Both ids are nil for every run this
	// door creates.
	_, needsModel := agentLLMProvider(req.Agent)
	if !needsModel || req.Task == harnessLoginTask || !isModelRun(req.TaskMode, nil, nil, req.Interactive) {
		if req.ModelProvider != "" {
			writeError(w, http.StatusBadRequest, mpRunNoModel)
			return false
		}
		return true
	}
	var sc types.SiteConfig
	if s.cfg.Store != nil {
		var err error
		if sc, err = s.cfg.Store.GetSiteConfig(ctx); err != nil {
			writeServerError(w, r, "get site config", err)
			return false
		}
	}
	if sc.ModelProviders == nil {
		if req.ModelProvider != "" {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf(mpRunNoBlock, req.ModelProvider))
			return false
		}
		return true
	}
	var pin string
	if len(wsRefs) > 0 && wsRefs[0].LLMCred != nil {
		pin = wsRefs[0].LLMCred.ProviderRef
	}
	choice, err := chooseModelProvider(sc, req.Agent, req.ModelProvider, pin, func(id string) (bool, error) {
		return s.capSeamAllowed(ctx, capModelProvider, id)
	})
	switch {
	case err != nil:
		writeServerError(w, r, "resolve capability", err)
		return false
	case choice.notGranted:
		s.denyMemberField(w, r, "runs.model_provider", "capability_model_provider", choice.refusal)
		return false
	case choice.refusal != "":
		writeError(w, http.StatusUnprocessableEntity, choice.refusal)
		return false
	case choice.chosen && !providerKindDispatched[choice.provider.Kind]:
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf(mpRunNotYet, choice.provider.ID))
		return false
	}
	return true
}
