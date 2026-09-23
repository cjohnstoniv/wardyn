// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"fmt"
	"log/slog"
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
// choose. ponytail: MP-8 (subscription) and MP-9 (Bedrock) add their arms; MP-9
// deletes the map.
var providerKindDispatched = map[types.ModelProviderKind]bool{
	types.ModelProviderAnthropicAPIKey: true,
	types.ModelProviderOpenAIAPIKey:    true,
	types.ModelProviderCustomEndpoint:  true,
}

// runProviderChoice is chooseModelProvider's answer. chosen=false with no
// refusal is "no provider serves this harness": the run launches on today's
// path with today's advisory.
type runProviderChoice struct {
	provider types.ModelProvider
	chosen   bool
	// governs: a provider block is set and this is a model run, so the legacy
	// lanes are bypassed — the run is credentialed by its provider or by
	// nothing (resolveRunLLMAccess, dispatch's resolveProviderLane).
	governs bool
	refusal string
	// notGranted marks a refusal the caller's capability decided — a 403
	// authz.denied row, not an org-configuration 422.
	notGranted bool
	// providerID and kind (#532) name the provider a refusal is ABOUT, even
	// when it was never chosen (providerID is set by providerRefusal for
	// every state refusal; kind is "" when the named provider does not exist,
	// state=mpRunStateMissing). The 422 and the run.create audit row read
	// these, never choice.provider, which is the zero value on every refusal.
	providerID string
	kind       types.ModelProviderKind
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
			return providerRefusal(d.ID, d.Kind, mpRunStateOff), nil
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
		c := providerRefusal(serving[0].ID, serving[0].Kind, mpRunStateNotGranted)
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
		return providerRefusal(id, "", mpRunStateMissing)
	case p.Disabled:
		return providerRefusal(id, p.Kind, mpRunStateOff)
	case !p.Serves(agent):
		return providerRefusal(id, p.Kind, fmt.Sprintf(mpRunStateNotServing, agent))
	case !isCandidate(id):
		c := providerRefusal(id, p.Kind, mpRunStateNotGranted)
		c.notGranted = true
		return c
	}
	return runProviderChoice{provider: p, chosen: true}
}

// providerRefusal is a state refusal naming id (multi-provider §2.6's one
// sentence). kind is "" when id does not name a real provider
// (mpRunStateMissing) — the only state refusal without one.
func providerRefusal(id string, kind types.ModelProviderKind, state string) runProviderChoice {
	return runProviderChoice{refusal: fmt.Sprintf(mpRunRefusal, id, state, mpRunRemedy), providerID: id, kind: kind}
}

// writeProviderRefusal is enforceRunModelProvider's 422: `provider` and
// `kind` (#532) name the provider the refusal is about, so the console can
// open THAT provider's door instead of guessing from the roster. id is ""
// only when the refusal names no provider at all (mpRunChoose). `reason`
// (llmRefusalAuditReason, "model_credential") rides only on a credential
// refusal, the one class a sign-in or a stored key repairs: the console
// already answers that reason with a sign-in and a relaunch, which would
// repair nothing for a provider that is off, not available to the agent or
// of a kind with no dispatch arm (multi-provider §5.8: no door).
func writeProviderRefusal(w http.ResponseWriter, id string, kind types.ModelProviderKind, msg string, credential bool) {
	body := errorBody{Error: msg, Provider: id, Kind: string(kind)}
	if credential {
		body.Reason = llmRefusalAuditReason
	}
	writeJSON(w, http.StatusUnprocessableEntity, body)
}

// enforceRunModelProvider is the model-provider choice at BOTH doors, create
// and Review, so Review answers the refusal launch would. With no provider
// block it changes nothing unless the request named a provider, which it
// refuses rather than ignores. wsRefs[0] is the primary workspace, the one
// whose pin a run inherits (foldRunIntegration reads the same one).
//
// A provider of a kind with no dispatch arm yet is refused
// (providerKindDispatched), and so is one whose credential the caller has not
// stored, so a run on a deployment that configured providers never reaches
// dispatch with a choice it cannot honour. Writes its own refusal and returns
// ok=false once it has.
//
// The returned runProviderChoice is launch's (runs.go) only source for
// AgentRun.ModelProviderID and the run.create audit snapshot (#527); Review
// uses it only for its model-access row. choice.chosen is false with no
// provider block (the zero runProviderChoice, today's path) and under a block
// that serves no provider for this agent (governs=true: no model credential
// at all). Neither is a choice, and callers must not treat a zero
// provider.ID as one.
func (s *Server) enforceRunModelProvider(w http.ResponseWriter, r *http.Request, req createRunRequest, wsRefs []types.Workspace) (runProviderChoice, bool) {
	ctx := r.Context()
	if req.ModelProvider != "" && !modelProviderIDPattern.MatchString(req.ModelProvider) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(mpRunBadID, req.ModelProvider))
		return runProviderChoice{}, false
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
			return runProviderChoice{}, false
		}
		return runProviderChoice{}, true
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
			writeError(w, http.StatusServiceUnavailable, mpRunUnreadable)
			return runProviderChoice{}, false
		}
	}
	if sc.ModelProviders == nil {
		if req.ModelProvider != "" {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf(mpRunNoBlock, req.ModelProvider))
			return runProviderChoice{}, false
		}
		return runProviderChoice{}, true
	}
	// An AI integration no longer credentials a run here (foldRunIntegration
	// folds none under a block), so naming one is refused, not ignored.
	if req.IntegrationID != "" {
		writeError(w, http.StatusUnprocessableEntity, mpRunNoIntegration)
		return runProviderChoice{}, false
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
		return runProviderChoice{}, false
	case choice.notGranted:
		s.denyMemberField(w, r, "runs.model_provider", "capability_model_provider", choice.refusal)
		return runProviderChoice{}, false
	case choice.refusal != "":
		writeProviderRefusal(w, choice.providerID, choice.kind, choice.refusal, false)
		return runProviderChoice{}, false
	case choice.chosen && !providerKindDispatched[choice.provider.Kind]:
		writeProviderRefusal(w, choice.provider.ID, choice.provider.Kind, fmt.Sprintf(mpRunNotYet, choice.provider.ID), false)
		return runProviderChoice{}, false
	case choice.chosen:
		// Liveness, the check dispatch repeats: the caller's OWN credential for
		// the provider, never anyone else's (runIdentitySubject is the namespace
		// dispatch reads for this run).
		msg, err := s.providerCredentialRefusal(ctx, runIdentitySubject(ctx, principalFromRequest(r)), choice.provider)
		if err != nil {
			// The sentence alone: a transient store failure is no door
			// (multi-provider §5.8), and `provider` is what keys one.
			writeError(w, http.StatusServiceUnavailable, fmt.Sprintf(mpRunCredUnreadable, choice.provider.ID))
			return runProviderChoice{}, false
		}
		if msg != "" {
			writeProviderRefusal(w, choice.provider.ID, choice.provider.Kind, msg, providerCredentialMissing(msg))
			return runProviderChoice{}, false
		}
	}
	choice.governs = true
	return choice, true
}

// DRAFT (M2 canon pending).
const (
	mpAccessProvisioned = "model access provisioned for agent %q: your own credential for model provider %s is injected proxy-side — it is never resident in the sandbox."
	mpAccessNoProvider  = "no model access for agent %q: no model provider serves it on this deployment — an admin adds one under Settings → Model providers."
)

// providerLLMAccess is the model-access verdict under a provider block: the
// chosen provider's (its credential was checked when it was chosen), or none.
func providerLLMAccess(agent string, mp runProviderChoice) *composeLLMAccess {
	if mp.chosen {
		return &composeLLMAccess{Provisioned: true, Note: fmt.Sprintf(mpAccessProvisioned, agent, mp.provider.ID)}
	}
	return &composeLLMAccess{Note: fmt.Sprintf(mpAccessNoProvider, agent)}
}
