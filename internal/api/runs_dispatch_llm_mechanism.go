// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The DECLARED-MECHANISM gate: when an org has said HOW an agent reaches its
// model (SiteConfig.AgentProviders), this is the one place that keeps the lane
// which actually fired from being a different one — at dispatch
// (enforceConfiguredLLMMechanism), and at create/Review as the same refusal in
// 422 form (enforceCreateLLMMechanism).
//
// It is a sibling file rather than more of runs_dispatch_llm.go because it is a
// real seam: resolveLLMTransport DECIDES a transport by a fixed precedence, and
// everything here COMPARES that decision against an admin's declaration. The two
// never mix — nothing in this file resolves a credential, and nothing in the
// transport file reads a roster. (Inlined it would also put that file over the
// 1000-line gate, which is how the seam got noticed, not why it exists.)
package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ── DRAFT (M2 canon pending) ────────────────────────────────────────────────
// The refusal copy this gate introduces, held in ONE block so the canon swap is
// a single-file diff, and asserted THROUGH the constants by every test.
const (
	// llmMechanismDeadSentence is the ONE refusal enforceConfiguredLLMMechanism
	// composes: the DECLARED lane in words, its state, and the promise that
	// nothing else is substituted for it. %s = the lane (llmMechanismWords),
	// %s = its state.
	//
	// The {ts} variant of this sentence ("expired at <ts> and could not be
	// renewed") is NOT composed here: the one lane whose expiry Wardyn knows at
	// dispatch is the captured AWS SSO session, and that lane already hands the
	// gate a finished sentence of its own (bedrockAuth.ssoRefreshFailure, the
	// renewal half of this same promise). Re-deriving a timestamp for it here
	// would be a second spelling of a sentence that exists.
	//
	// DRAFT (M2 canon pending)
	llmMechanismDeadSentence = "this run's model access is configured as %s, and that credential %s — " +
		"sign in again under Settings → Model provider. Wardyn does not substitute a different model provider."

	// llmMechanismStateNotConfigured is the state above when NOTHING credentials
	// the run: the declared lane did not fire and no other one did either.
	//
	// DRAFT (M2 canon pending)
	llmMechanismStateNotConfigured = "is not configured"

	// llmMechanismStateNotTheLane is the state above when a DIFFERENT lane fired.
	// It names that lane, because "is not configured" is false of a row whose
	// credential is sitting right there working — an admin told that their api
	// key is missing, on a deployment where Bedrock quietly took the run, goes
	// looking for the wrong problem. %s is the lane that won. This is the sentence
	// the whole gate exists to be able to say.
	//
	// DRAFT (M2 canon pending)
	llmMechanismStateNotTheLane = "is not the lane this run resolved to, which is %s"

	// llmDetailBedrockExpired is the brokered-LLM 404's detail for a
	// half-configured Bedrock deployment (see llmUnavailableDetail). %s = the
	// captured session's expiry.
	//
	// DRAFT (M2 canon pending)
	llmDetailBedrockExpired = "Bedrock is configured but its credential expired at %s; reconnect it"
)

// llmMechanismWords names a declared lane the way a refusal must — what the
// admin chose, in words. "Bedrock is configured" was exactly the sentence that
// told a customer nothing, so every bedrock_* sub-lane names itself.
var llmMechanismWords = map[types.AgentMechanism]string{
	types.AgentMechanismAnthropicSubscription: "a Claude subscription (container login)",
	types.AgentMechanismAnthropicAPIKey:       "an Anthropic API key",
	types.AgentMechanismOpenAIAPIKey:          "an OpenAI API key",
	types.AgentMechanismBedrockBearer:         "Amazon Bedrock (bearer key)",
	types.AgentMechanismBedrockSSO:            "Amazon Bedrock (captured AWS SSO session)",
	types.AgentMechanismBedrockEnv:            "Amazon Bedrock (AWS credentials in the daemon's own environment)",
	types.AgentMechanismBedrockAWSDir:         "Amazon Bedrock (the host ~/.aws mount)",
	types.AgentMechanismNone:                  "no Wardyn-wired credential — the image brings its own",
}

// selectedMechanism folds the lanes that credential a run onto the ONE mechanism
// actually SELECTED, in resolveLLMTransport's own precedence order (host-staged
// mount / managed subscription > Bedrock > api-key). ok=false means NOTHING
// selected — the api-key arm is resolveLLMTransport's unconditional final else,
// which checks nothing, so its readiness is the caller's api-key term
// (hasAnthropicAPIKeyInjection at dispatch, the resolved spec's api_key grant at
// create), never the placeholder env it writes.
//
// It is deliberately the ONE spelling of "which lane is this?", shared by the
// dispatch gate and its create/Review half: a fold that disagreed between them
// would refuse a run at launch that create had just admitted.
func (s *Server) selectedMechanism(agent string, subscription bool, b bedrockAuth, managed, apiKey bool) (types.AgentMechanism, bool) {
	switch {
	case subscription || managed:
		// Both subscription paths are ONE mechanism to an admin: the resident
		// mount and the Wardyn-managed setup-token are the same Claude account,
		// differing only in where the copy lives.
		return types.AgentMechanismAnthropicSubscription, true
	case b.ready:
		switch {
		case b.bearer:
			return types.AgentMechanismBedrockBearer, true
		case b.ssoInject:
			return types.AgentMechanismBedrockSSO, true
		case b.awsMount:
			return types.AgentMechanismBedrockAWSDir, true
		default:
			// The resident SigV4 fallback: keys the OPERATOR set on the daemon.
			return types.AgentMechanismBedrockEnv, true
		}
	case apiKey:
		// Vendor from the agent's own catalog convention, so a codex-cli run
		// reads as openai_api_key without a second table of who speaks what.
		p, ok := s.llmProviderFor(agent)
		if !ok {
			return "", false
		}
		if p.secret == "openai-api-key" {
			return types.AgentMechanismOpenAIAPIKey, true
		}
		return types.AgentMechanismAnthropicAPIKey, true
	}
	return "", false
}

// mechanismSatisfied reports whether the lane that actually fired is the one the
// admin declared for this agent.
//
// Under `shared` the comparison is at the COARSE provider level (AgentMechanism.
// ProviderType): within Bedrock, today's credential chain (bearer > captured SSO
// > ~/.aws mount > resident keys) still runs exactly as it does today — it is one
// mechanism with several sources, and narrowing an admin's "Bedrock" to one
// sub-lane would refuse runs that work.
//
// Under `per_user` the ONLY admissible lane is the principal's own captured AWS
// SSO session: every other Bedrock arm is an operator-namespace read, so folding
// them together would serve the admin's credential to a member — the exact
// failure per_user exists to prevent.
//
// C4: the per-principal half of that promise is C4's — resolveBedrockAuth still
// reads the OPERATOR's blob, so a per_user row whose operator blob resolves is
// admitted here today. When C4 gives resolveBedrockAuth its perUser flag (own
// blob only, no fall-through to the bearer/mount/static arms), that case becomes
// "nothing selected" and this same predicate refuses it with no change.
//
// A declared `none` row (BYOA) matches only when NO lane fired, which is what
// "Wardyn wires no model credential" means. The gate never reaches it in
// practice — an agent with no catalog gateway is not gated at all — but the row
// is what it says, so the predicate says it.
func mechanismSatisfied(row types.AgentProvider, selected types.AgentMechanism, ok bool) bool {
	if row.Mechanism == types.AgentMechanismNone {
		return !ok
	}
	if !ok {
		return false
	}
	if row.CredentialSource == types.CredentialSourcePerUser {
		return selected == types.AgentMechanismBedrockSSO
	}
	return selected.ProviderType() == row.Mechanism.ProviderType()
}

// llmMechanismRefusal is the sentence for a declared lane that is not carrying
// this run. It names BOTH lanes whenever there are two to name — the one the
// admin declared and the one that actually resolved — and says only "is not
// configured" for the case where that is literally true: nothing fired at all.
//
// The captured-SSO lane's own renewal failure wins when it has one: that sentence
// names WHY (a spent refresh token, or AWS not answering) and what to do about
// it, which neither state above can.
func llmMechanismRefusal(row types.AgentProvider, selected types.AgentMechanism, ok bool, ssoRefreshFailure string) string {
	if ssoRefreshFailure != "" && row.Mechanism == types.AgentMechanismBedrockSSO {
		return ssoRefreshFailure
	}
	state := llmMechanismStateNotConfigured
	if ok {
		state = fmt.Sprintf(llmMechanismStateNotTheLane, llmMechanismWords[selected])
	}
	return fmt.Sprintf(llmMechanismDeadSentence, llmMechanismWords[row.Mechanism], state)
}

// enforceConfiguredLLMMechanism fails a run CLOSED when the org declared HOW this
// agent reaches its model and the lane that actually fired is not that one.
//
// THE WHOLE LANE IS THIS: a credential must never silently change mechanism or
// source. resolveLLMTransport picks by a FIXED precedence that knows nothing
// about an admin's declaration, so a deployment whose row says "Anthropic API
// key" dispatches BEDROCK the moment a region, a model and a bearer secret exist
// — and a customer reads a Bedrock bill for runs they configured as api-key. A
// readiness check would not catch that: the declared lane IS ready, it just is
// not the one that won. So the gate compares what was SELECTED.
//
// WHAT IT DOES NOT DO, deliberately:
//   - LEGACY MODE (no AgentProviders block, or no row for this agent) refuses
//     NOTHING. A no-credential run dispatches today carrying only the create-time
//     advisory, and the corpus depends on it (dispatch fixtures seed no model
//     credential; an early FAILED run cascades through concurrency, governance
//     and completion tests). The refusal exists solely where an admin declared a
//     mechanism.
//   - it never gates a run that makes no model call. modelRun is false for
//     task_mode=exec and every workspace/source-bound non-interactive run (scan,
//     record/verify, Build); harnessLogin is the credential-CAPTURE box, which by
//     definition has no credential yet — gating it would deadlock per_user, where
//     signing in is how a member gets one.
//   - it never gates an agent Wardyn wires no model credential for (--agent none,
//     every WARDYN_AGENT_IMAGES custom image): agentLLMProvider is not ok for
//     them, no lane can fire, and without this term every such run would read
//     "dead" and be refused.
//
// An INTERACTIVE model run is refused too: the person at the terminal cannot
// repair a model credential from inside the sandbox, and a shell that boots to a
// model it cannot reach is a worse answer than a named refusal.
//
// Returns false when the run was marked FAILED (CAS from STARTING, so a
// concurrent kill's KILLED is not clobbered) and dispatch must stop.
func (s *Server) enforceConfiguredLLMMechanism(ctx context.Context, run types.AgentRun, sc types.SiteConfig,
	llm llmTransport, injections []runner.InjectionGrant,
) bool {
	if !llm.modelRun || llm.harnessLogin {
		return true
	}
	if _, needsModel := agentLLMProvider(run.Agent); !needsModel {
		return true
	}
	// agentProviderFor ignores Disabled, and so does this gate: a disabled row is
	// refused earlier and more clearly, at run create and at the record launcher
	// (agentRosterRefusal), so a disabled row never reaches dispatch to be read
	// here for its mechanism.
	row, declared := agentProviderFor(sc, run.Agent)
	if !declared {
		return true
	}
	selected, ok := s.selectedMechanism(run.Agent,
		llm.subscription, llm.bedrock, llm.injectManaged,
		s.hasAnthropicAPIKeyInjection(run.Agent, injections))
	if mechanismSatisfied(row, selected, ok) {
		return true
	}
	msg := llmMechanismRefusal(row, selected, ok, llm.bedrock.ssoRefreshFailure)
	s.failAndRevoke(ctx, run.ID, types.RunStarting, msg)
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
		run.ID.String(), "failure", mustJSON(map[string]any{"error": msg})))
	return false
}

// llmMechanismGateApplies is enforceConfiguredLLMMechanism's three-term gate
// asked of a run REQUEST instead of a resolved transport, so create and Review
// refuse exactly the runs dispatch would. See that function for each term.
func llmMechanismGateApplies(req createRunRequest) bool {
	// Source id is nil by construction: a source-bound run (record/verify/build)
	// is launched by newStepRun, never decoded from this door's body.
	if req.Task == harnessLoginTask ||
		!isModelRun(req.TaskMode, req.WorkspaceID, nil, req.Interactive) {
		return false
	}
	_, needsModel := agentLLMProvider(req.Agent)
	return needsModel
}

// llmLanes is which model-credential lanes a run's RESOLVED spec has available
// at CREATE time — the shared half of resolveRunLLMAccess's advisory and the
// create/Review mechanism refusal, so the warning and the 422 cannot disagree
// about what this run would dispatch on.
type llmLanes struct {
	// subscription: the policy bind-mounts the resident ~/.claude.
	subscription bool
	// managed: the Wardyn-managed setup-token would credential this run —
	// managedSubscriptionLane, the SAME predicate dispatch applies, on the same
	// terms and in the same order.
	managed bool
	// apiKey: the resolved spec already brokers an api_key grant for this
	// agent's provider host — the operator's explicit api-key choice.
	apiKey bool
	// bedrock is the operator Bedrock posture resolved WITHOUT refresh: create is
	// a dry run over a one-use rotating token.
	bedrock bedrockAuth
}

// resolveRunLLMLanes resolves those lanes. It is the create-side twin of
// resolveLLMTransport's own lane block; the fold from lanes to a mechanism is
// selectedMechanism, shared with dispatch.
func (s *Server) resolveRunLLMLanes(ctx context.Context, req createRunRequest, spec *types.RunPolicySpec,
	bedrockRef *types.WorkspaceBedrockRef,
) llmLanes {
	var l llmLanes
	llmProv, _ := s.llmProviderFor(req.Agent)
	_, l.apiKey = apiKeyGrantForHost(spec, llmProv.host)
	l.subscription = specHasMountTarget(spec, claudeCredTarget)
	// BEDROCK FIRST, because managed is the fallback BELOW it: dispatch computes
	// managed with !bedrockReady, so resolving managed before Bedrock here would
	// make create fold a Bedrock run onto the subscription lane (selectedMechanism
	// tests subscription/managed before Bedrock) and refuse a run dispatch would
	// have credentialed perfectly well.
	//
	// refresh=false: create is a dry run over a ONE-USE rotating token. An
	// expired-but-renewable captured SSO session still reads READY here (dispatch
	// renews it), so create never warns about — or refuses — a failure that
	// cannot happen.
	l.bedrock = s.resolveBedrockAuth(ctx, req.Agent, l.subscription, true, false, bedrockRef)
	// The SAME predicate dispatch applies, with the same terms — including the
	// posture term, whose absence here made every SSO deployment's managed run
	// read as "subscription" at create and dispatch as something else.
	// modelRun/harnessLogin are this request's own: a run that makes no model call
	// has no lane at all, which is what dispatch decides for it too.
	l.managed = s.managedSubscriptionLane(req.Agent,
		isModelRun(req.TaskMode, req.WorkspaceID, nil, req.Interactive), req.Task == harnessLoginTask,
		l.subscription, l.bedrock.ready, l.apiKey, spec)
	return l
}

// enforceCreateLLMMechanism is enforceConfiguredLLMMechanism at CREATE and
// REVIEW: the same declaration, the same fold, the same sentence — answered as a
// 422 before a run row exists, rather than as a run that boots and dies.
//
// It is the create-time model-access ADVISORY's harder half: with no declared
// mechanism the advisory is unchanged (a warning on the 201), and with one the
// same finding is a refusal, because an org that wrote down how its agents reach
// their model has said that a run on some other lane is not what it wants.
//
// The roster read is the only cost in legacy mode: no row for this agent, no
// lane resolution at all.
//
// Returns ok=false when it has already written the 422.
func (s *Server) enforceCreateLLMMechanism(ctx context.Context, w http.ResponseWriter, req createRunRequest,
	spec types.RunPolicySpec, bedrockRef *types.WorkspaceBedrockRef,
) bool {
	if !llmMechanismGateApplies(req) || s.cfg.Store == nil {
		return true
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		// Admitting on a read failure is the same call dispatch's own
		// site-config consumers make: refusing would blame the caller for an
		// outage, and dispatch reads the roster again on the way to the sandbox.
		return true
	}
	row, declared := agentProviderFor(sc, req.Agent)
	if !declared {
		return true
	}
	lanes := s.resolveRunLLMLanes(ctx, req, &spec, bedrockRef)
	selected, ok := s.selectedMechanism(req.Agent, lanes.subscription, lanes.bedrock, lanes.managed, lanes.apiKey)
	if mechanismSatisfied(row, selected, ok) {
		return true
	}
	// No ssoRefreshFailure at create: nothing here redeems a refresh token.
	writeError(w, http.StatusUnprocessableEntity, llmMechanismRefusal(row, selected, ok, ""))
	return false
}

// llmUnavailableDetail is what the proxy's brokered-LLM 404 says when this run
// reaches that route with no credential behind it — compiled HERE, at dispatch,
// because the proxy sidecar knows only that no injection matched. Empty means
// the route's own generic detail stands.
//
// Only one state is worth a sentence of its own: Bedrock configured (a region
// and a model are set) with no Bedrock credential that resolved, and a captured
// SSO session on file to name an expiry from. That is the half-configured
// deployment the field report arrived from — the run dispatched on the api-key
// placeholder and the 404 said nothing about Bedrock at all.
//
// It must be the WHOLE truth about the route it explains, so two runs are
// excluded even with Bedrock half-configured: an agent Bedrock cannot credential
// (resolveBedrockAuth is claude-code only — a codex-cli run's OpenAI route 404
// has nothing to do with Bedrock), and a run that already carries an api-key
// injection for its provider (whatever 404'd, it was not this).
//
// C4: readAWSSSOBlob is the OPERATOR's blob. Under a per_user row the expiry
// named here is the admin's, not the reader's — when C4 makes that read
// owner-scoped, this call follows it and the sentence becomes the member's own.
func (s *Server) llmUnavailableDetail(ctx context.Context, run types.AgentRun, llm llmTransport,
	injections []runner.InjectionGrant,
) string {
	if llm.bedrockReady || !llm.modelRun || llm.harnessLogin ||
		run.Agent != "claude-code" || s.hasAnthropicAPIKeyInjection(run.Agent, injections) ||
		s.cfg.BedrockRegion == "" || s.cfg.BedrockModel == "" {
		return ""
	}
	blob, found, err := s.readAWSSSOBlob(ctx)
	if err != nil || !found || blob.ExpiresAt.IsZero() {
		return ""
	}
	return fmt.Sprintf(llmDetailBedrockExpired, blob.ExpiresAt.UTC().Format(time.RFC3339))
}
