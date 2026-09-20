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
	"log/slog"
	"net/http"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// DRAFT (M2 canon pending)
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
	// The third %s is the REMEDY clause (llmMechanismRemedy): the destination is
	// the one part of this sentence that depends on who is reading it.
	llmMechanismDeadSentence = "this run's model access is configured as %s, and that credential %s — %s " +
		"Wardyn does not substitute a different model provider."

	// llmMechanismPinContradictedSentence is the refusal for a stored AWS SSO
	// session whose account/role the roster no longer allows. It is its own
	// sentence rather than a state of the one above because nothing here is
	// missing or expired: the declared lane fired, the credential is live, and
	// it is the wrong IDENTITY — a fact "is not configured" and "is not the lane
	// this run resolved to" both get wrong.
	//
	// It names BOTH pairs for the same reason ssoTokenAccountPinRefusal does
	// (awssso_pin.go): the person reading it is the one who has to sign in
	// again, and a sign-in is genuinely all it takes — a new login run stamps
	// the CURRENT pin and its capture replaces the stored blob. %s = the stored
	// account, role; then the allowed account, role.
	llmMechanismPinContradictedSentence = "this run's stored AWS sign-in is for account %s / role %s, but this agent now pins AWS sign-ins to account %s / role %s — " +
		"nothing was started. To replace it, %s Wardyn does not rewrite a stored sign-in."

	// llmMechanismStateNotConfigured is the state above when NOTHING credentials
	// the run: the declared lane did not fire and no other one did either.
	llmMechanismStateNotConfigured = "is not configured"

	// llmMechanismStateNotTheLane is the state above when a DIFFERENT lane fired.
	// It names that lane, because "is not configured" is false of a row whose
	// credential is sitting right there working — an admin told that their api
	// key is missing, on a deployment where Bedrock quietly took the run, goes
	// looking for the wrong problem. %s is the lane that won. This is the sentence
	// the whole gate exists to be able to say.
	llmMechanismStateNotTheLane = "is not the lane this run resolved to, which is %s"

	// the REMEDY clause the three refusals above end on
	//
	// The destination is the one part of a refusal that depends on WHO is
	// reading it, so it must not be a single fixed destination such as "sign in
	// again under Settings → Model provider". Under a per_user row that page's AWS
	// button is admin-only (connection-cards.tsx's disabled={!operator}), so that
	// sentence would send the member it is talking to — the only person who CAN
	// repair their own captured session — to the one page that will not let them.
	// The
	// member's two real doors are the console's Getting started page
	// and the model-access banner every screen now carries.

	// llmMechanismRemedyPerUser is that member's destination. It names Getting
	// started FIRST because that is the one door true on all three surfaces this
	// sentence reaches: the rail's 422, the failed run's block
	// in focus mode — where the block owns the sign-in and the banner has no
	// button — and the CLI, whose reader has no console banner at all. Sentence
	// case, as the nav item and the page title are.
	llmMechanismRemedyPerUser = "sign in to AWS from Getting started in the console, or from the sign-in banner the console shows on every page."

	// llmMechanismRemedyShared is the ADMIN's destination, unchanged: under a
	// shared row the one credential is theirs and Settings → Model provider is
	// where they replace it.
	llmMechanismRemedyShared = "sign in again under Settings → Model provider."

	// llmMechanismRemedySharedFirst is the same destination without "again":
	// "again" is a claim about the reader's past, and the not-configured arm is
	// the one state that says nothing ever fired here.
	llmMechanismRemedySharedFirst = "sign in under Settings → Model provider."

	// llmDetailBedrockExpired is the brokered-LLM 404's detail for a
	// half-configured Bedrock deployment (see llmUnavailableDetail). %s = the
	// captured session's expiry.
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
// That promise is now kept at the SOURCE as well as here: resolveBedrockAuth
// takes the same per_user scope and reads only the principal's own blob, with no
// fall-through to the bearer/mount/static arms, so a member with no session of
// their own arrives here as "nothing selected" and this predicate refuses them —
// never served the admin's session by a lane that fired underneath it.
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

// llmRefusalAuditReason is the MACHINE-READABLE class on the run.create/failure
// audit row this gate writes: "the run was refused over a model credential".
// Not copy — a wire value the console grades an ending by (lib/api/audit.ts's
// CREDENTIAL_REASON), so it is not in the DRAFT block above and never changes
// with the wording.
//
// Deliberately NOT narrowed to "a sign-in repairs it": the server states the
// CLASS, and the console decides whether to offer a door from the same
// model-access grading every other surface reads — a refusal whose renewal
// merely did not complete ("launch again in a moment") grades live and gets no
// button, correctly, without this key knowing anything about it.
const llmRefusalAuditReason = "model_credential"

// llmMechanismRemedy is the destination clause for one reader: the member's own
// two doors under a per_user row, the admin's Settings page otherwise.
//
// `configured` is whether ANY lane fired — the one state where nothing ever
// did is also the one where "again" would be false.
func llmMechanismRemedy(perUser, configured bool) string {
	switch {
	case perUser:
		return llmMechanismRemedyPerUser
	case !configured:
		return llmMechanismRemedySharedFirst
	default:
		return llmMechanismRemedyShared
	}
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
	return fmt.Sprintf(llmMechanismDeadSentence, llmMechanismWords[row.Mechanism], state,
		llmMechanismRemedy(row.CredentialSource == types.CredentialSourcePerUser, ok))
}

// pinContradictionRefusal is the sentence for a resolved Bedrock auth whose
// STORED AWS identity the roster no longer allows, "" when there is none.
//
// Shared by the dispatch gate and its create/Review twin for the file's own
// reason: a fold that disagreed between them would refuse a run at launch that
// create had just admitted.
func pinContradictionRefusal(sc types.SiteConfig, b bedrockAuth, perUser bool) string {
	stored, pinned, mismatch := bedrockBlobPinMismatch(sc, b)
	if !mismatch {
		return ""
	}
	// A stored session exists by construction here, so the remedy is always the
	// "again" arm of its audience's clause.
	return fmt.Sprintf(llmMechanismPinContradictedSentence,
		stored.AccountID, stored.RoleName, pinned.AccountID, pinned.RoleName,
		llmMechanismRemedy(perUser, true))
}

// enforceConfiguredLLMMechanism fails a run CLOSED when the org declared HOW this
// agent reaches its model and the lane that actually fired is not that one.
//
// The whole lane is this: a credential must never silently change mechanism or
// source. resolveLLMTransport picks by a FIXED precedence that knows nothing
// about an admin's declaration, so a deployment whose row says "Anthropic API
// key" dispatches BEDROCK the moment a region, a model and a bearer secret exist
// — and a customer reads a Bedrock bill for runs they configured as api-key. A
// readiness check would not catch that: the declared lane IS ready, it just is
// not the one that won. So the gate compares what was SELECTED.
//
// What it does not do, deliberately:
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
	// The stored identity, before the lane itself is judged. The declared lane
	// IS the one that fired here, so mechanismSatisfied is about to admit a run
	// carrying an account/role the roster no longer allows — the pin is checked
	// at capture time and nowhere else, so a capture that predates a pin is the
	// one identity nothing compares. Refused rather than rewritten, for the
	// reason awssso_pin.go opens with: the blob is baked verbatim into the
	// sandbox's ~/.aws/config, so rewriting it would record a session nobody saw
	// and merely move the IAM 403 back to run time.
	msg := pinContradictionRefusal(sc, llm.bedrock, row.CredentialSource == types.CredentialSourcePerUser)
	if msg == "" {
		if mechanismSatisfied(row, selected, ok) {
			return true
		}
		msg = llmMechanismRefusal(row, selected, ok, llm.bedrock.ssoRefreshFailure)
	}
	s.failAndRevoke(ctx, run.ID, types.RunStarting, msg)
	// `reason` is what makes this refusal readable by a machine — the console
	// grades the ending `credential` from it and offers the sign-in instead of
	// directions to it. `mechanism` is the DECLARED lane, so that
	// door binds to THIS run's lane: the reason covers every declared mechanism
	// (an OpenAI row's refusal included), while the console's model_access
	// grades Claude Code alone, and without the lane key a failed Codex run
	// whose owner also lacks an AWS sign-in would be offered one.
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
		run.ID.String(), "failure", mustJSON(map[string]any{
			"error": msg, "reason": llmRefusalAuditReason, "mechanism": string(row.Mechanism),
		})))
	return false
}

// enforceReadableRosterForCredential refuses a dispatch whose roster read
// failed, before the AWS SSO credential scope is resolved from a zero site
// config.
//
// That scope decides WHOSE captured session credentials the run
// (awsSSOScopeFor, resolveLLMInjections) and whether the operator-wide Bedrock
// bearer key is reachable at all (resolveBedrockAuth's !sso.perUser guard). A
// failed read yields perUser=false, owner="" — the OPERATOR namespace — so a
// store blip credentialed a per_user MEMBER's run with the deployment-wide
// session, unaudited. That is a fail-open on the SERVING door, which the
// write-door rules ("written or deleted") do not reach.
//
// Refused, not degraded to the caller's own namespace: a credential must never
// silently change source — the law enforceConfiguredLLMMechanism above exists
// for — and a run that fails to start carrying its reason is the smaller outage
// than one served somebody else's credential. The next dispatch after the store
// recovers is byte-identical to today's.
//
// Scoped to the runs that would actually select one, through the credential
// code's own predicate (bedrockLaneSelectable, runs_bedrock.go): a deployment
// with no Bedrock region/model, a non-model run, a login box, a subscription
// run and every non-claude-code agent dispatch exactly as before, blip or no.
func (s *Server) enforceReadableRosterForCredential(ctx context.Context, run types.AgentRun,
	p dispatchParams, policy *types.RunPolicySpec, siteCfgOK bool,
) bool {
	if siteCfgOK || run.Task == harnessLoginTask {
		return true
	}
	region, model := s.bedrockRegionModel(p.BedrockRef)
	if !bedrockLaneSelectable(run.Agent,
		isModelRun(p.TaskMode, run.WorkspaceID, run.SourceID, p.Interactive),
		specHasMountTarget(policy, claudeCredTarget), s.cfg.Secrets != nil, region, model) {
		return true
	}
	s.failAndRevoke(ctx, run.ID, types.RunStarting, dispatchRosterUnreadableRefusal)
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
		run.ID.String(), "failure", mustJSON(map[string]any{"error": dispatchRosterUnreadableRefusal})))
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
	// bedrock is the operator Bedrock posture; resolved WITH refresh only for the
	// real launch (a dry run — preflight, the advisory — never spends the token).
	bedrock bedrockAuth
}

// resolveRunLLMLanes resolves those lanes. It is the create-side twin of
// resolveLLMTransport's own lane block; the fold from lanes to a mechanism is
// selectedMechanism, shared with dispatch.
func (s *Server) resolveRunLLMLanes(ctx context.Context, req createRunRequest, spec *types.RunPolicySpec,
	bedrockRef *types.WorkspaceBedrockRef, sso awsSSOScope, refresh bool,
) llmLanes {
	var l llmLanes
	llmProv, _ := s.llmProviderFor(req.Agent)
	_, l.apiKey = apiKeyGrantForHost(spec, llmProv.host)
	l.subscription = specHasMountTarget(spec, claudeCredTarget)
	// Bedrock first, because managed is the fallback BELOW it: dispatch computes
	// managed with !bedrockReady, so resolving managed before Bedrock here would
	// make create fold a Bedrock run onto the subscription lane (selectedMechanism
	// tests subscription/managed before Bedrock) and refuse a run dispatch would
	// have credentialed perfectly well.
	//
	// refresh: the REAL launch passes true and redeems an expired-but-renewable
	// captured SSO session right here, so the click is the check — a renewal
	// AWS refuses is refused at create, before any run exists, instead of
	// failing the run at dispatch after the person was told it launched (the
	// field report). Review's preflight and the create-path advisory pass
	// false: dry runs over a ONE-USE rotating token, where an expired-but-
	// renewable session still reads READY (dispatch renews it).
	//
	// modelRun is THIS RUN's own answer, hoisted so the Bedrock probe and the
	// managed lane below cannot disagree. Hard-coding it true here would make a
	// scan run (workspace_id + non-interactive) or a task_mode=exec
	// run — the two shapes isModelRun exists to exclude — read as a ready Bedrock
	// lane at create and at Review, with the 201 saying "Amazon Bedrock … this run
	// uses it automatically" about a run dispatch hands no model credential at
	// all. Source id is nil by construction on this door (a source-bound run is
	// launched by newStepRun, never decoded from a create body) — the same term
	// llmMechanismGateApplies passes.
	modelRun := isModelRun(req.TaskMode, req.WorkspaceID, nil, req.Interactive)
	l.bedrock = s.resolveBedrockAuth(ctx, req.Agent, l.subscription, modelRun, refresh, bedrockRef, sso)
	// The SAME predicate dispatch applies, with the same terms — including the
	// posture term, whose absence here made every SSO deployment's managed run
	// read as "subscription" at create and dispatch as something else.
	// modelRun/harnessLogin are this request's own: a run that makes no model call
	// has no lane at all, which is what dispatch decides for it too.
	l.managed = s.managedSubscriptionLane(req.Agent, modelRun, req.Task == harnessLoginTask,
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
// lane resolution at all — unless a caller asked for the residency grade (out
// non-nil), which legacy mode must answer too and which has no other source for
// it. That is Review's cost alone; create passes nil and pays nothing.
//
// out, when non-nil, receives where this run's model credential will land
// (gradeModelCredential) from the SAME resolved lanes the refusal is judged on.
// It is filled here rather than resolved again one frame up because a third
// resolution would mean a third secret-store read per Review, and because a
// residency graded from different lanes than the gate's could tell the operator
// "never written into the sandbox" about a run the gate is refusing for being on
// the resident lane. Left untouched (residency stays "") wherever nothing was
// resolved, so the caller omits the field rather than publishing a guess.
//
// Returns ok=false when it has already written the 422.
func (s *Server) enforceCreateLLMMechanism(ctx context.Context, w http.ResponseWriter, req createRunRequest,
	spec types.RunPolicySpec, bedrockRef *types.WorkspaceBedrockRef, subject string, out *modelCredentialFacts, refresh bool,
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
	if !declared && out == nil {
		return true
	}
	// The roster read above is also what says WHOSE credential this run may use,
	// so the scope costs nothing extra here. The subject is the CALLER's run
	// identity — never secretOwnerFromRequest, which answers "" for every
	// operator and would refuse an admin their own per_user capture at create
	// while dispatch resolved it fine.
	lanes := s.resolveRunLLMLanes(ctx, req, &spec, bedrockRef, awsSSOScopeFor(sc, req.Agent, subject), refresh)
	selected, ok := s.selectedMechanism(req.Agent, lanes.subscription, lanes.bedrock, lanes.managed, lanes.apiKey)
	if out != nil {
		*out = gradeModelCredential(row, declared, lanes, selected, ok, s.subscriptionInjectEnabled())
	}
	if !declared {
		return true
	}
	// The stored-identity refusal first, exactly as dispatch orders it: this door
	// exists so a run dispatch would refuse never boots at all, and a run whose
	// stored AWS sign-in the roster no longer allows is one of them.
	if msg := pinContradictionRefusal(sc, lanes.bedrock, row.CredentialSource == types.CredentialSourcePerUser); msg != "" {
		writeLLMRefusal(w, msg)
		return false
	}
	if mechanismSatisfied(row, selected, ok) {
		return true
	}
	// The captured-SSO lane's own renewal verdict names the refusal when it has
	// one (the real launch redeems here; a dry run never has one). A renewal AWS
	// did not ANSWER is transient — the sign-in is still good — so that refusal
	// carries no class: the console's launch door must not open over "launch
	// again in a moment".
	msg := llmMechanismRefusal(row, selected, ok, lanes.bedrock.ssoRefreshFailure)
	if lanes.bedrock.ssoRefreshFailure == awsSSORefreshUnavailableSentence && row.Mechanism == types.AgentMechanismBedrockSSO {
		writeError(w, http.StatusUnprocessableEntity, msg)
		return false
	}
	writeLLMRefusal(w, msg)
	return false
}

// writeLLMRefusal is the create-time model-credential refusal: the same 422 and
// sentence as before, plus the class the console acts on — the New Run rail
// opens the AWS sign-in on it and launches again once the capture lands, so a
// lapsed session costs one dialog rather than a trip to Getting started by hand.
// The class is the failure audit row's own word (llmRefusalAuditReason), not a
// second vocabulary; it never names WHICH lane — the console reads the current
// roster row for that, exactly as the failure block does.
func writeLLMRefusal(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusUnprocessableEntity, errorBody{Error: msg, Reason: llmRefusalAuditReason})
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
// sso is the RUN'S OWN scope, so the expiry named is the expiry that matters:
// under a per_user row the operator's blob says nothing about why THIS
// principal's run has no credential.
func (s *Server) llmUnavailableDetail(ctx context.Context, run types.AgentRun, llm llmTransport,
	injections []runner.InjectionGrant, sso awsSSOScope,
) string {
	if llm.bedrockReady || !llm.modelRun || llm.harnessLogin ||
		run.Agent != "claude-code" || s.hasAnthropicAPIKeyInjection(run.Agent, injections) ||
		s.cfg.BedrockRegion == "" || s.cfg.BedrockModel == "" {
		return ""
	}
	// The run's OWN scope: under per_user the expiry worth naming is this
	// principal's, and the operator's says nothing about why their run has no
	// credential.
	blob, found, err := s.readAWSSSOBlob(ctx, sso)
	if err != nil || !found || blob.ExpiresAt.IsZero() {
		return ""
	}
	return fmt.Sprintf(llmDetailBedrockExpired, blob.ExpiresAt.UTC().Format(time.RFC3339))
}

// siteConfigForDispatch reads the operator-wide site config for ONE dispatch,
// retrying a failed read exactly once.
//
// Why a retry belongs here and nowhere else. This one
// read decides three things at once: which artifact redirects apply, which
// upstream proxy the run gets, and — the one that matters — WHOSE model
// credential the run may use (awsSSOScopeFor over the roster). A failed read
// yields a zero SiteConfig, which reads as perUser=false, owner="" — the
// OPERATOR namespace — so enforceReadableRosterForCredential refuses the
// dispatch outright rather than serve a per_user member the deployment-wide
// session. That is the right answer for a genuinely unreadable roster and the
// wrong one for a single dropped connection, which is what a pgx pool blip
// usually is.
//
// ONE retry, immediately, with no delay: a dead pool answers instantly so the
// cost of the second attempt is nil, and a transient failure is very often gone
// by the next call. A loop would turn one Postgres hiccup into a create request
// that hangs; the sweep and the caller's own retry cover everything past that.
//
// It deliberately does NOT widen what is served on failure. The refusal
// downstream is unchanged, so a deployment whose roster really cannot be read
// still fails its Bedrock-credentialled runs closed.
func (s *Server) siteConfigForDispatch(ctx context.Context) (types.SiteConfig, error) {
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err == nil {
		return sc, nil
	}
	slog.WarnContext(ctx, "wardynd: the site config read failed at dispatch; retrying once before the credential scope is decided",
		slog.Any("err", err))
	return s.cfg.Store.GetSiteConfig(ctx)
}
