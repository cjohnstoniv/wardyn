// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/authz"
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
	mpRunNoneGranted     = "No model provider that serves %s is granted to you — ask your admin. Wardyn does not substitute a different model provider."
	mpRunChoose          = "Choose a model provider for this run: more than one serves %s, and none is its default."
	mpRunNoBlock         = "model_provider names %q, but this deployment has no model providers — launch without model_provider."
	mpRunNoModel         = "model_provider applies only to a run that calls a model — a task_mode=exec run or an agent that takes no model provider chooses none."
	mpRunBadID           = "model_provider: %q is not a provider id — lowercase letters, digits and ._- , at most 64 characters"
)

// runProviderChoice is chooseModelProvider's answer. chosen=false with no
// refusal is "no provider serves this harness": the run launches with no model
// credential and the advisory that says so.
type runProviderChoice struct {
	provider types.ModelProvider
	chosen   bool
	refusal  string
	// notGranted marks a refusal the caller's capability decided — a 403
	// authz.denied row, not an org-configuration 422.
	notGranted bool
	// asMissing marks a notGranted refusal of a provider the caller NAMED
	// (request or pin): it is answered exactly as a provider that does not
	// exist, since provider ids are guessable (D-6, #1018), and only the
	// authz.denied row records capability_model_provider.
	asMissing bool
	// providerID and kind (#532) name the provider a refusal is ABOUT, even
	// when it was never chosen (providerID is set by providerRefusal for
	// every state refusal; kind is "" when the named provider does not exist,
	// state=mpRunStateMissing). The 422 and the run.create audit row read
	// these, never choice.provider, which is the zero value on every refusal.
	providerID string
	kind       types.ModelProviderKind
	// renewAtLaunch: Review's dry check found the chosen AWS session expired
	// but renewable, so launch will renew it (mpBRRenewAtLaunch).
	renewAtLaunch bool
	// loginHost is the nonsecret destination resolved from the already-read
	// own AWS session. Preview does not read that session and leaves it empty.
	loginHost string
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
		return judgeNamedProvider(sc, agent, named, requested == "", granted)
	}
	if row, ok := agentProviderFor(sc, agent); ok && row.DefaultProvider != "" {
		if d, found := modelProviderByID(sc.ModelProviders, row.DefaultProvider); found {
			// The grant before the state: a disabled default the caller is not
			// granted is passed over too, never refused naming it (D-6, #1018).
			ok, err := granted(d.ID)
			if err != nil {
				return runProviderChoice{}, err
			}
			if ok && d.Disabled {
				return providerRefusal(d.ID, d.Kind, mpRunStateOff), nil
			}
			if ok && isCandidate(d.ID) {
				return runProviderChoice{provider: d, chosen: true}, nil
			}
		}
	}
	switch {
	case len(candidates) == 1:
		return runProviderChoice{provider: candidates[0], chosen: true}, nil
	case len(candidates) > 1:
		return runProviderChoice{refusal: fmt.Sprintf(mpRunChoose, agent)}, nil
	case len(serving) == 0:
		return runProviderChoice{}, nil
	}
	// However many serve the agent, the caller named none, so the refusal names
	// none either: naming the one that serves would tell them a provider they
	// are not granted exists (D-6, #1018). The audit row still names it.
	c := runProviderChoice{refusal: fmt.Sprintf(mpRunNoneGranted, agent), notGranted: true}
	if len(serving) == 1 {
		c.providerID = serving[0].ID
	}
	return c, nil
}

// judgeNamedProvider answers for a provider the request or the workspace pin
// named: it is the choice, or the run is refused naming it. The grant is asked
// before the provider's state, so a provider the caller is not granted reads
// as one that does not exist whatever its state (asMissing) — or, when the
// workspace pin named it (fromPin), is refused naming no provider at all: the
// caller never named it, and a workspace read hides it from them
// (pinStamper), so the refusal must not be where they learn it.
func judgeNamedProvider(sc types.SiteConfig, agent, id string, fromPin bool, granted func(id string) (bool, error)) (runProviderChoice, error) {
	// The grant before existence, so a pin the caller is not granted — even
	// one naming no provider — is refused exactly as pinStamper hides it.
	allowed, err := granted(id)
	if err != nil {
		return runProviderChoice{}, err
	}
	if !allowed && fromPin {
		return runProviderChoice{refusal: fmt.Sprintf(mpRunNoneGranted, agent), notGranted: true, providerID: id}, nil
	}
	p, ok := modelProviderByID(sc.ModelProviders, id)
	switch {
	case !ok:
		return providerRefusal(id, "", mpRunStateMissing), nil
	case !allowed:
		c := providerRefusal(id, "", mpRunStateMissing)
		c.notGranted, c.asMissing = true, true
		return c, nil
	case p.Disabled:
		return providerRefusal(id, p.Kind, mpRunStateOff), nil
	case !p.Serves(agent):
		return providerRefusal(id, p.Kind, fmt.Sprintf(mpRunStateNotServing, agent)), nil
	}
	return runProviderChoice{provider: p, chosen: true}, nil
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
// repair nothing for a provider that is off, not available to the agent,
// unset, or of no person (multi-provider §5.8: no door).
//
// Each one is also an authz.denied row (model_provider_unavailable, #987),
// at both doors as dispatch's refuseProviderDispatch is at its own: the body
// keeps this envelope, which refuse's plain error would drop, so the row is
// written beside it with the same provider, kind and credential class.
func (s *Server) writeProviderRefusal(w http.ResponseWriter, r *http.Request, id string, kind types.ModelProviderKind, msg string, credential bool) {
	s.writeProviderRefusalAs(w, r, authz.Deny(authz.ReasonModelProviderUnavailable, "runs.model_provider", msg), id, kind, msg, credential)
}

// writeProviderRefusalAs is writeProviderRefusal with the audit row's decision
// given: a provider refused as if missing (asMissing) records
// capability_model_provider under the byte-identical 422.
func (s *Server) writeProviderRefusalAs(w http.ResponseWriter, r *http.Request, d authz.Decision, id string, kind types.ModelProviderKind, msg string, credential bool) {
	if id != "" {
		d = d.With("provider", id)
	}
	if kind != "" {
		d = d.With("kind", string(kind))
	}
	body := errorBody{Error: msg, Provider: id, Kind: string(kind)}
	if credential {
		body.Reason = llmRefusalAuditReason
		d = d.With("remedy", llmRefusalAuditReason)
	} else {
		// #656 slice 3: the non-credential states (off, not serving, not
		// granted, no such provider) had no wire reason at all — the SAME
		// audit reason d already carries, so a caller can at least tell
		// "this is a model-provider refusal" even without the specific state.
		body.Reason = string(authz.ReasonModelProviderUnavailable)
	}
	s.recordRefusal(r.Context(), r, d)
	writeJSON(w, http.StatusUnprocessableEntity, body)
}

// writeProviderChoiceRefusal answers a refused provider choice, at the create
// door and the record door alike, so the two cannot disagree about one choice:
//   - asMissing: the 422 of a provider that does not exist, no kind;
//   - notGranted: a 403, the provider named only in the authz.denied row;
//   - any other refusal: the 422 naming the provider the refusal is about;
//   - none of those: a chosen provider whose liveness check d refused.
func (s *Server) writeProviderChoiceRefusal(w http.ResponseWriter, r *http.Request, choice runProviderChoice, d providerDenial) {
	if choice.asMissing || choice.notGranted || choice.refusal != "" {
		providerChoiceRunRefusal(choice).write(s, w, r)
		return
	}
	s.writeProviderRefusal(w, r, choice.provider.ID, choice.provider.Kind, d.msg, d.credential)
}

// enforceRunModelProvider is the model-provider choice at BOTH doors, create
// and Review, so Review answers the refusal launch would. With no provider
// block nothing serves the run, and a request that named a provider is refused
// rather than ignored. wsRefs[0] is the primary workspace, the one whose pin a
// run inherits.
//
// A chosen provider is checked live at both doors, as dispatch will check it
// again (providerLiveness, one check per kind): the caller's own credential for
// it must be there, so a run on a deployment that configured providers never
// reaches dispatch with a choice it cannot honour. Every refusal naming a
// provider is written by writeProviderRefusal. So is an env_secret grant in
// spec (the run's folded policy) that would set a model-credential variable
// (modelEnvSecretGrant): under a governing block a run's model credential
// comes only from its provider. Writes its own refusal and returns ok=false
// once it has.
//
// The returned runProviderChoice is launch's (runs.go) only source for
// AgentRun.ModelProviderID and the run.create audit snapshot (#527); Review
// uses it only for its model-access row. choice.chosen is false when no
// provider serves this agent (no model credential at all), and for a run that
// makes no model call. Neither is a choice, and callers must not treat a zero
// provider.ID as one.
//
// renew is create's alone: it lets the Bedrock check renew an expired AWS
// sign-in whose refresh token is live (withCreateRenewal), so no run is
// created that dispatch could not boot. Review passes false: a dry check never
// spends a one-use refresh token.
func (s *Server) enforceRunModelProvider(w http.ResponseWriter, r *http.Request, req createRunRequest,
	spec types.RunPolicySpec, wsRefs []types.Workspace, renew bool,
) (runProviderChoice, bool) {
	ctx := r.Context()
	choice, refusal := s.authorizeRunModelProvider(r, req, wsRefs, false)
	if refusal.write(s, w, r) {
		return runProviderChoice{}, false
	}
	if choice.chosen {
		// Liveness, the check dispatch repeats: the caller's OWN credential for
		// the provider, never anyone else's (runIdentitySubject is the namespace
		// dispatch reads for this run). Only create renews, and only a Bedrock
		// sign-in: the subscription arm keeps its status read at both doors.
		refresh := renew && choice.provider.Kind.IsBedrock()
		lctx := ctx
		if refresh {
			lctx = withCreateRenewal(ctx)
		}
		blob, d, err := s.providerLiveness(lctx, choice.provider, req.Agent, runIdentitySubject(ctx, principalFromRequest(r)), refresh)
		if err != nil {
			// Deliberately bare (#656 slice 3): the sentence alone — a
			// transient store failure is no door (multi-provider §5.8), and
			// `provider` is what keys one. A renewal that could not be saved
			// brings its own sentence (providerUnavailable).
			slog.ErrorContext(ctx, "api: read model provider credential", slog.String("provider", choice.provider.ID), slog.Any("err", err))
			msg := providerReadFailed(choice.provider)
			var pu providerUnavailable
			if errors.As(err, &pu) {
				msg = pu.msg
			}
			writeError(w, http.StatusServiceUnavailable, msg)
			return runProviderChoice{}, false
		}
		if d.msg != "" {
			s.writeProviderChoiceRefusal(w, r, choice, d)
			return runProviderChoice{}, false
		}
		// A dry check passes an expired AWS session only when it is renewable.
		choice.renewAtLaunch = !refresh && choice.provider.Kind == types.ModelProviderBedrockSSO && blob.expired(s.cfg.Now())
		if choice.provider.Kind == types.ModelProviderBedrockSSO {
			choice.loginHost = ssoPortalHost(blob.Region, s.cfg.AWSSSOEndpointOverride)
		}
	}
	_, needsModel := agentLLMProvider(req.Agent)
	if !needsModel || !createDoorIsModelRun(req) {
		return choice, true
	}
	if name, secretName, found := modelEnvSecretGrant(spec); found {
		s.writeProviderRefusal(w, r, choice.provider.ID, choice.provider.Kind, fmt.Sprintf(mpRunModelEnvSecret, secretName, name), false)
		return runProviderChoice{}, false
	}
	return choice, true
}

// DRAFT (M2 canon pending).
const (
	mpAccessProvisioned = "model access provisioned for agent %q: your own credential for model provider %s is injected proxy-side — it is never resident in the sandbox."
	mpAccessNoProvider  = "no model access for agent %q: no model provider serves it on this deployment — an admin adds one under Settings → Model providers."
	mpAccessAWSSignIn   = "model access provisioned for agent %q: your own AWS sign-in for model provider %s serves it."
)

// providerLLMAccess is a model run's model-access verdict: the chosen
// provider's (its credential was checked when it was chosen), or none.
// An AWS sign-in is the one kind not claimed never-resident: the sandbox's AWS
// SDK exchanges the session for role credentials it then holds.
func providerLLMAccess(agent string, mp runProviderChoice) *composeLLMAccess {
	if mp.chosen {
		note := mpAccessProvisioned
		if mp.provider.Kind == types.ModelProviderBedrockSSO {
			note = mpAccessAWSSignIn
		}
		return &composeLLMAccess{Provisioned: true, Note: fmt.Sprintf(note, agent, mp.provider.ID)}
	}
	return &composeLLMAccess{Note: fmt.Sprintf(mpAccessNoProvider, agent)}
}
