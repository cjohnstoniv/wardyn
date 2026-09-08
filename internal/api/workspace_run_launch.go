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

// referencedWorkspaces resolves the onboarded workspaces a RESOLVED spec uses —
// local-dir mount sources (skipping system credential mounts) + repos, in
// selection order. The first entry is the PRIMARY (its profile drives image
// selection). Best-effort + never errors: a source with no matching onboarded row
// is skipped (the onboarding gate already rejected non-onboarded sources at
// run-create, so in practice every user source resolves here). Deduped by
// workspace id. The lookup is by workspaceSourceIndex (workspace_refs.go),
// built from EVERY workspace's Sources — not the single-source Kind/Source
// mirror, which is empty for a multi-source workspace.
func (s *Server) referencedWorkspaces(ctx context.Context, spec types.RunPolicySpec) []types.Workspace {
	if s.cfg.Store == nil {
		return nil
	}
	all, err := s.cfg.Store.ListWorkspaces(ctx)
	if err != nil {
		return nil
	}
	idx := indexWorkspacesBySource(all)
	var out []types.Workspace
	seen := map[uuid.UUID]bool{}
	add := func(ws types.Workspace, ok bool) {
		if !ok || seen[ws.ID] {
			return
		}
		seen[ws.ID] = true
		out = append(out, ws)
	}
	for _, wm := range spec.WorkspaceMounts {
		if systemMountTargets[wm.Target] {
			continue
		}
		ws, ok := idx.localDir[wm.Source]
		add(ws, ok)
	}
	for _, wr := range spec.WorkspaceRepos {
		ws, ok := idx.repo[wr.Repo]
		add(ws, ok)
	}
	return out
}

// workspaceIDsOf extracts the ids from referencedWorkspaces' result, in the
// same order — the slice handleCreateRun denormalizes onto
// AgentRun.WorkspaceIDs at create time. A nil/empty wsRefs yields a nil
// slice (not an empty-but-non-nil one), so the persisted column is SQL NULL
// exactly when this run references no onboarded workspace.
func workspaceIDsOf(wsRefs []types.Workspace) []uuid.UUID {
	var ids []uuid.UUID
	for _, ws := range wsRefs {
		ids = append(ids, ws.ID)
	}
	return ids
}

// workspaceSourcesOfType filters ws.Sources down to entries of typ, preserving
// order.
func workspaceSourcesOfType(ws types.Workspace, typ types.WorkspaceSourceType) []types.WorkspaceSource {
	var out []types.WorkspaceSource
	for _, src := range ws.Sources {
		if src.Type == typ {
			out = append(out, src)
		}
	}
	return out
}

// claimImportStep atomically claims the workspace's serial import-step slot for
// runID, CAS-ing active_run_id from the value the caller observed (M1/H14): two
// concurrent step launches that both saw the slot free cannot both dispatch —
// the loser gets errImportStepBusy. It returns the CLAIMED workspace row (the
// base any pre-dispatch status write must build on so it preserves the claim)
// and the release compensator EVERY pre-dispatch failure path must return
// through — it fails the persisted-but-undispatched run and frees the slot, so a
// failed launch never bricks the next step (failAndRevoke's CAS is conditional
// on PENDING, so it no-ops pre-CreateRun).
func (s *Server) claimImportStep(ctx context.Context, ws types.Workspace, runID uuid.UUID) (types.Workspace, func(error) error, error) {
	claimed, ok, err := s.cfg.Store.ClaimWorkspaceActiveRun(ctx, ws.ID, runID, ws.ActiveRunID)
	if err != nil {
		return types.Workspace{}, nil, fmt.Errorf("claim import-step slot: %w", err)
	}
	if !ok {
		return types.Workspace{}, nil, errImportStepBusy
	}
	return claimed, func(e error) error {
		hint := "the workspace import step could not start"
		if e != nil {
			hint += ": " + e.Error()
		}
		s.failAndRevoke(ctx, runID, types.RunPending, hint)
		_, _ = s.cfg.Store.ClearWorkspaceActiveRun(ctx, ws.ID, runID)
		return e
	}, nil
}

// newStepRun mints the run identity and builds the run row every
// server-launched step/probe/login run shares: PENDING, State/SPIFFEID/
// RunnerTarget set, agent claude-code unless set overrides it. set customizes
// what differs (the trusted linkage, Task specifics, AutoStopAfterSec, the
// login lane's agent + Interactive) before the row is returned; callers take
// run.CreatedAt as the launch clock.
//
// IT IS ALSO THE LIMITS-AXIS CHOKEPOINT (F153). Every server-launched lane that
// creates a run passes through here, and gov is a REQUIRED argument with no
// usable zero value, so a lane added later cannot compile without saying which
// of the acting principal's governance limits bind the run it is about to
// create. Reading the limits per call site is exactly how POST /runs came to be
// the only lane that read them at all, while a member-reachable scan created
// runs for a walled principal that counted against nothing.
func (s *Server) newStepRun(ctx context.Context, runID uuid.UUID, actor, task string, cc types.ConfinementClass, gov stepRunGovernance, set func(*types.AgentRun)) (types.AgentRun, string, error) {
	if !gov.decided {
		// Fail closed on a zero value: a lane that did not decide must not
		// silently inherit "no limit binds".
		return types.AgentRun{}, "", fmt.Errorf("api: step run %q created with no governance decision (use stepRunGoverned)", task)
	}
	if err := s.stepRunCeilingLimits(ctx, actor, gov); err != nil {
		return types.AgentRun{}, "", err
	}
	id, err := s.cfg.Identity.MintRunIdentity(ctx, runID, actor, actor, internalAudience)
	if err != nil {
		return types.AgentRun{}, "", fmt.Errorf("mint run identity: %w", err)
	}
	now := s.cfg.Now().UTC()
	run := types.AgentRun{
		ID: runID, CreatedAt: now, UpdatedAt: now, CreatedBy: actor,
		Agent: "claude-code", Task: task,
		ConfinementClass: cc, State: types.RunPending, SPIFFEID: id.SPIFFEID,
		RunnerTarget: s.cfg.RunnerTarget,
	}
	if set != nil {
		set(&run)
	}
	return run, id.Token, nil
}

// newWorkspaceStepRun is newStepRun linked to ws through WorkspaceID — the
// TRUSTED linkage each step's upload authorises on, never sandbox input.
func (s *Server) newWorkspaceStepRun(ctx context.Context, runID uuid.UUID, actor, task string, ws types.Workspace, cc types.ConfinementClass, gov stepRunGovernance) (types.AgentRun, string, error) {
	wsID := ws.ID
	return s.newStepRun(ctx, runID, actor, task, cc, gov, func(run *types.AgentRun) {
		run.WorkspaceID = &wsID
	})
}

// ceilingFloorClass is the ACTING PRINCIPAL's confinement floor (CC1 when their
// ceiling declares none) — what a scan inherits, as opposed to record's
// strongest-class.
//
// The principal's ceiling, never Config.DefaultPolicy directly: a governance
// profile REPLACES the deployment default rather than composing with it, so
// reading the deployment field here would run a walled member's sandbox at the
// deployment's floor instead of the floor their profile declares. For a
// principal with no assignment effectiveCeiling answers Config.DefaultPolicy, so
// this is byte-for-byte the old defaultFloorClass on every deployment that has
// authored no profile.
func ceilingFloorClass(ceiling governanceCeiling) types.ConfinementClass {
	if cc := ceiling.Spec.MinConfinementClass; cc != "" {
		return cc
	}
	return types.CC1
}

// dispatchAndSettle is the shared launch tail: dispatch, re-read the run so the
// caller returns the store's freshest row, and settle a launch that already
// reached a terminal state (see settleTerminalLaunch).
func (s *Server) dispatchAndSettle(ctx context.Context, created types.AgentRun, ceiling dispatchCeiling, p dispatchParams) types.AgentRun {
	s.dispatchRun(ctx, created, ceiling, p)
	created = s.refreshRun(ctx, created.ID, created)
	s.settleTerminalLaunch(ctx, created.ID, created)
	return created
}

// launchRecordRun starts one session's interactive sandbox.
//   - run.Task = "workspace record" (the server-side discriminator: uploads and
//     reconciles branch on it, and it keys the trusted run→workspace linkage);
//   - egress depends on `confined`: a LEARNING session (confined=false) is OPEN
//     (AllowAllEgress=true) so every host the task dials is logged egress.allow
//     (complete capture, no per-domain approvals); a CONFINED REPLAY session
//     (confined=true) is default-deny, limited to AllowedDomains, so re-running
//     the same steps proves least privilege and off-policy hosts are denied live.
//     AllowedDomains keeps the confined-egress union anyway — credential
//     injection fires ONLY on exact allowlist entries even under allow-all, and
//     clone needs its git hosts; private/metadata IPs stay denied by the
//     unconditional guard;
//   - confinement = the STRONGEST class the wired runner supports (an open
//     sandbox deserves the best isolation available), never the policy floor.
//     weakCC reports when that best is still CC1 so callers warn loudly —
//     refusing would make record unusable on Docker Desktop boxes.
//
// Sessions are always interactive: the sandbox comes up idle for the attach
// terminal (bounded — an abandoned OPEN-egress sandbox must not live forever);
// the operator's "Done recording" is the normal run kill, and capture happens
// at termination from the audit events.
//
// LAUNCH ORDER (concurrency-load-bearing): (1) CAS-claim active_run_id — the
// atomic serial gate; a concurrent step launch that also saw the slot free
// loses the CAS and never launches a sandbox. (2) Upsert the task's
// `recording` entry — BEFORE dispatch, so even a run that dies instantly has
// the entry its terminal capture keys on. (3) Create + dispatch.
// mintRecordAPIKeyInjections persists each auto-mint api_key grant in grants
// (skipping approval-gated and non-api_key kinds) and returns its proxy
// injection. The record path wires injections this way from two grant sources —
// the workspace's required-integration folds and the LLM fallback — so both go
// through here. A store error is returned for the caller's abort().
func (s *Server) mintRecordAPIKeyInjections(ctx context.Context, runID uuid.UUID, now time.Time, grants []types.GrantSpec) ([]runner.InjectionGrant, error) {
	var injections []runner.InjectionGrant
	for _, g := range grants {
		if g.Kind != types.GrantAPIKey || g.RequiresApproval {
			continue
		}
		grantID := uuid.New()
		if _, gerr := s.cfg.Store.CreateGrant(ctx, types.CredentialGrant{
			ID: grantID, RunID: runID, CreatedAt: now, Spec: g,
		}); gerr != nil {
			return nil, gerr
		}
		if rule, rerr := injectionRuleFromScope(g.Scope); rerr == nil {
			injections = append(injections, runner.InjectionGrant{GrantID: grantID, Rule: rule})
		}
	}
	return injections, nil
}

// errRecordCeilingLimit marks a refusal by the acting principal's governance
// profile rather than a daemon fault. Its own sentinel so the handler answers
// the status the create path answers for the same limit instead of a 500.
var errRecordCeilingLimit = errors.New("governance profile limit")

// stepRunGovernance is ONE step lane's answer to the Limits axis: which of the
// acting principal's governance limits bind a run that lane creates. It has NO
// usable zero value — newStepRun refuses one — so a lane added later cannot
// compile without deciding, which is the anti-forgetting device dispatchCeiling
// already gives the deny axis.
//
// Two questions, because the two limits ask different things of a lane:
//
//   - interactive: does this lane open an attachable session? deny_interactive
//     is about a human getting a terminal, and a lane's answer is structural
//     (a record session always is, a scan never is) rather than request-shaped,
//     which is why it is stated here and not derived from requestIsInteractive.
//   - counted: does the run this lane creates count against
//     max_concurrent_runs? "How many runs you can have going" means every run
//     attributed to that principal, so a lane says no only when the run is not
//     the principal's own work.
//
// deny_task_mode_exec is deliberately absent: no step lane runs `exec`.
type stepRunGovernance struct {
	ceiling     governanceCeiling
	interactive bool
	counted     bool
	// decided is set only by the constructors below, so the zero value is
	// distinguishable from a deliberate "neither limit binds".
	decided bool
}

// stepRunGoverned is the general constructor: state both answers explicitly.
func stepRunGoverned(ceiling governanceCeiling, interactive, counted bool) stepRunGovernance {
	return stepRunGovernance{ceiling: ceiling, interactive: interactive, counted: counted, decided: true}
}

// recordRunGovernance: a record/verify session comes up idle for the attach
// terminal (always interactive) and is a run the principal has going.
func recordRunGovernance(ceiling governanceCeiling) stepRunGovernance {
	return stepRunGoverned(ceiling, true, true)
}

// scanRunGovernance: a scan run is server-authored and unattachable
// (deny_interactive has nothing to say about it) but it IS one of the runs the
// principal has going, so the quota binds it. This is the lane F153's residue
// exposed: POST /workspaces/{id}/scan is member-reachable, it creates a run for
// that member, and until this it read no limit at all — a member at their cap
// could keep spawning scans.
func scanRunGovernance(ceiling governanceCeiling) stepRunGovernance {
	return stepRunGoverned(ceiling, false, true)
}

// operatorStepGovernance: a lane mounted operator-only, whose run is the
// DEPLOYMENT's diagnostic rather than any principal's work (the site-config
// proxy probe, the managed-harness login). Neither limit binds — and the
// resolved ceiling is carried anyway so the value still says which profile the
// decision was made against.
func operatorStepGovernance(ceiling governanceCeiling) stepRunGovernance {
	return stepRunGoverned(ceiling, false, false)
}

// stepRunCeilingLimits applies the Limits axis to one step lane's launch,
// scoped by that lane's own stepRunGovernance answer.
//
// THE SCOPING RULE, stated once here the way ceilingForDispatch states the deny
// axis's, so "absent row => absent behaviour" cannot be re-decided per call
// site: the Limits axis binds POST /runs (runs_create_validate.go) and every
// lane that reaches newStepRun. An unassigned principal has no profile, so
// there is no limit to read and no deployment-wide default to fall back on
// (PF-36 — the `all` assignment IS the opt-in).
//
// The MESSAGES are the frozen member copy from the create path, reused verbatim
// rather than reworded: one limit means one sentence wherever a member meets it,
// and a second wording for the same refusal is how "your profile denies this"
// stops being recognisable. The interactive sentence's tail ("Launch with a
// task, and without `--interactive`") does not fit an always-interactive route
// and a reword is FILED for the canon owner rather than applied here.
//
// A quota COUNT failure is a store error, not a refusal, and is returned as
// itself so the handler keeps answering 500 for it — an outage must not read as
// a policy decision.
func (s *Server) stepRunCeilingLimits(ctx context.Context, actor string, gov stepRunGovernance) error {
	if gov.ceiling.Profile == nil {
		return nil
	}
	name := gov.ceiling.Profile.Name
	ceiling := gov.ceiling
	// gov.interactive, not requestIsInteractive: whether a step lane opens an
	// attachable session is STRUCTURAL (a record session always does, a scan
	// never can), so the lane states it rather than the request shape implying
	// it — there is no request shape here to inspect.
	if ceiling.Limits.DenyInteractive && gov.interactive {
		//lint:ignore ST1005 canon member sentence (docs/design/governance-prompt.md limits table), pinned verbatim by governance_limits_test.go; it ends the way the doc writes it
		return fmt.Errorf("%w: interactive runs are not allowed by your governance profile %q, and a request with no task comes up interactive too. Launch with a task, and without `--interactive`.", errRecordCeilingLimit, name)
	}
	if limit := ceiling.Limits.MaxConcurrentRuns; limit > 0 && gov.counted {
		active, err := s.cfg.Store.CountActiveRunsBy(ctx, actor)
		if err != nil {
			return fmt.Errorf("count active runs: %w", err)
		}
		if active >= limit {
			//lint:ignore ST1005 canon member sentence (docs/design/governance-prompt.md limits table), pinned verbatim by governance_limits_test.go; it ends the way the doc writes it
			return fmt.Errorf("%w: too many runs at once (max %d) — your governance profile %q caps how many runs you can have going, and %d are still active. Stop one first.", errRecordCeilingLimit, limit, name, active)
		}
	}
	return nil
}

func (s *Server) launchRecordRun(ctx context.Context, actor string, ws types.Workspace, sessionKey, sessionLabel string, confined bool) (types.AgentRun, bool, error) {
	if s.cfg.Runner == nil {
		return types.AgentRun{}, false, fmt.Errorf("no runner configured")
	}
	// THE ACTING PRINCIPAL'S CEILING (PF-24), resolved FIRST — before the
	// import-step CAS claim below, so a refusal costs no state and needs no
	// abort().
	//
	// This lane builds its OWN spec and deliberately skips the member clamp:
	// AllowAllEgress = !confined is Record Mode's learning posture (every host
	// the task dials is captured), and its operator-credential injections are
	// what make a confined replay authenticate the way a real run does. Both
	// stay. What must NOT follow from them is that a profile-walled principal
	// gets a server-authored, allow-all, credentialed sandbox they can attach
	// to on demand.
	//
	// POST /workspaces/{id}/record IS OPERATOR-ONLY (routes.go), not a
	// security-tier route — an earlier version of this comment said the
	// opposite, and the guard it justified could therefore never fire.
	// requireOperator gates on s.isOperator and effectiveCeiling short-circuits
	// on that SAME predicate, so every principal who reaches this function
	// arrives with Profile == nil. A security admin gets 403 at the door:
	// mounting the lane on securityOps would hand the tier that is defined never
	// to reach credential material or the host exactly both, which is why the
	// route did not move to make the guard live (F153).
	//
	// recordCeilingLimits below is therefore a FAIL-CLOSED assertion rather than
	// a live gate: any resolved profile refuses the lane outright. It costs
	// nothing today and is what a re-mount, or a second caller, meets.
	//
	// WHICH WALLS RIDE ALONG, named rather than implied — the earlier wording
	// ("the same walls as their ordinary runs") was true of one axis and false
	// of the other, which is how the gap survived review:
	//   - the DENY axis rides into dispatch and the re-assertion phase applies
	//     it there (runs_dispatch_ceiling.go), same phase as an ordinary run;
	//   - the member CLAMP is deliberately skipped, for the reason just given;
	//   - the LIMITS axis is applied BELOW, in this function. It is read nowhere
	//     else that this lane passes through: dispatch never reads it, and
	//     denyMemberGovernance/denyMemberRunQuota sit on POST /runs.
	//     THE SCOPING RULE, stated once here the way ceilingForDispatch states
	//     the deny axis's, so it cannot be re-decided per call site: the Limits
	//     axis binds POST /runs and this lane, and this lane binds it by
	//     refusing every assigned profile outright rather than by reading limit
	//     by limit — because the axis that makes this lane dangerous is the
	//     member CLAMP it skips, which is not a limit and can never be one.
	//     deny_task_mode_exec genuinely does not apply: a record session runs an
	//     agent, never `exec`.
	//
	// Unwalled principals thread nothing: no assignment ⇒ no denies ⇒ Record
	// Mode byte-for-byte unchanged, moat workflow intact.
	// Detach from request cancellation before the durable launch work + image
	// build: a client that walks away mid-build must not cancel it (dispatch's
	// own WithoutCancel lands too late to protect the pre-dispatch work above it).
	// The ceiling resolve below sits UNDER it — values are preserved, so the
	// principal still resolves, and a client disconnect can no longer turn into
	// a spurious ceiling failure.
	ctx = context.WithoutCancel(ctx)
	ceiling, cerr := s.effectiveCeiling(ctx)
	if cerr != nil {
		// FAIL CLOSED, the call effectiveCeiling's own doc makes: carrying on
		// would silently substitute the deployment ceiling for a profile that
		// may be far narrower — a widening caused by a database hiccup, on the
		// one lane that hands out an open-egress sandbox.
		return types.AgentRun{}, false, fmt.Errorf("resolve governance ceiling: %w", cerr)
	}
	// BEFORE the CAS claim below, so a refusal costs no state and needs no
	// abort() — the same reason the ceiling resolve sits where it does.
	if lerr := s.stepRunCeilingLimits(ctx, actor, recordRunGovernance(ceiling)); lerr != nil {
		return types.AgentRun{}, false, lerr
	}
	caps, cerr := s.cfg.Runner.Capabilities(ctx)
	if cerr != nil {
		return types.AgentRun{}, false, fmt.Errorf("runner capabilities unavailable: %w", cerr)
	}
	cc := bestClass(caps.ConfinementClasses)
	if cc == "" {
		return types.AgentRun{}, false, fmt.Errorf("runner declares no confinement class")
	}
	weakCC := cc == types.CC1

	runID := uuid.New()
	_, release, err := s.claimImportStep(ctx, ws, runID)
	if err != nil {
		return types.AgentRun{}, false, err
	}
	startedAt := s.cfg.Now().UTC()
	if _, _, perr := s.putRecordResult(ctx, ws.ID, sessionKey, RecordTaskResult{
		RunID: runID, Label: sessionLabel, Mode: recordModeInteractive, Confined: confined, Status: recordStatusRecording, StartedAt: startedAt,
	}, ""); perr != nil {
		return types.AgentRun{}, false, release(fmt.Errorf("persist record state: %w", perr))
	}
	// A CreateGrant failure AFTER CreateRun (below) would otherwise orphan the
	// persisted RunPending run + leave its minted run token / eligible grants
	// un-revoked, so every failure path from here on returns through abort.
	abort := func(reason error) error {
		now := s.cfg.Now().UTC()
		_, _, _ = s.putRecordResult(ctx, ws.ID, sessionKey, RecordTaskResult{
			RunID: runID, Label: sessionLabel, Mode: recordModeInteractive, Confined: confined, Status: recordStatusFailed, StartedAt: now, FinishedAt: &now,
			FailureHint: "launch failed: " + reason.Error(),
		}, recordStatusRecording)
		return release(reason)
	}

	run, runToken, err := s.newWorkspaceStepRun(ctx, runID, actor, "workspace record", ws, cc, recordRunGovernance(ceiling))
	if err != nil {
		return types.AgentRun{}, false, abort(err)
	}
	now := run.CreatedAt
	run.Interactive = true
	// A confined verify HOLDS an off-policy host at the door (wait_for_review:
	// the connection parks while the approval surfaces in the verify panel's
	// live strip; approve releases it, deny/timeout fails it). The old
	// deny_with_review here leaned on a stale "unattended probe must fail
	// fast" rationale — every session is interactive now (the operator drives
	// the attach terminal), so the operator IS present to decide, and a held
	// request that gets approved both completes in-flight AND lands as an
	// egress: requirement row via the decide() hook. A learning session
	// (allow-all) makes this inert.
	verifyFirstUse := types.FirstUseAlwaysDeny
	if confined {
		verifyFirstUse = types.FirstUseWaitForReview
	}
	policy := types.RunPolicySpec{
		MinConfinementClass: cc,
		// A CONFINED REPLAY session is default-deny, limited to AllowedDomains
		// (baseline clone/registry hosts ∪ the workspace's approved egress) — so
		// re-running the same steps proves they work under least privilege. A
		// learning session (open) allows all egress so the capture is complete.
		// Same interactive attach either way.
		AllowAllEgress: !confined,
		AllowedDomains: s.confinedEgressDomains(ws),
		// The operator's permanent per-workspace denies (Phase 4's `deny · always`)
		// must reach BOTH branches above, not just the confined AllowedDomains set —
		// deny beats allow_all_egress at the proxy (docs/POLICIES.md), so this line
		// is what actually stops the AllowAllEgress:true LEARNING session from
		// reaching (and then durably LEARNING — offering for promotion) a host the
		// operator already permanently blocked, which is exactly the branch that
		// looks least like it needs a deny-list. Raw column, not routed through
		// confinedEgressDomains: that helper's return stays allow-only (mirrors
		// unionWorkspaceEgress's contract) and there is no per-source deny contract
		// to fold the way the required-egress loop inside it folds allows.
		DeniedDomains: ws.DeniedEgress,
		// In a confined replay, an off-policy host ESCALATES to the operator instead
		// of a silent hard-deny — so a "bad curl" surfaces an approve/reject decision
		// in the record panel as it happens. Inert under allow-all, so it's a no-op
		// for a learning session. (Cloud-metadata / private IPs stay unconditionally
		// blocked regardless.)
		FirstUseApproval: verifyFirstUse,
		// Generous but FINITE idle cap — an abandoned open-egress recording
		// self-terminates (and revokes) instead of living forever.
		AutoStopAfterSec: int(recordInteractiveIdleCap.Seconds()),
	}
	run.AutoStopAfterSec = policy.AutoStopAfterSec // reaper reads the run row
	cloneURLs, ephemeralDirs, werr := wireWorkspaceSource(&run, &policy, ws)
	if werr != nil {
		return types.AgentRun{}, false, abort(werr)
	}
	created, err := s.cfg.Store.CreateRun(ctx, run)
	if err != nil {
		return types.AgentRun{}, false, abort(fmt.Errorf("create record run: %w", err))
	}
	// Clone grants only AFTER the run row exists — credential_grants.run_id has an
	// immediate FK to agent_runs(id). Only the FIRST repo source's clone gets an
	// auto-minted credential (see wireWorkspaceSource's doc comment) — matches
	// the pre-composition-model single-source behavior; an additional repo
	// source needs its own pre-existing access until multi-repo grant minting
	// is wired.
	var primaryCloneURL string
	if len(cloneURLs) > 0 {
		primaryCloneURL = cloneURLs[0]
	}
	ghGrantID, sshGrants, gerr := s.workspaceSourceGrants(ctx, runID, now, primaryCloneURL)
	if gerr != nil {
		return types.AgentRun{}, false, abort(fmt.Errorf("create record clone grants: %w", gerr))
	}

	// Record in the built devcontainer image so the task actually runs (its
	// toolchain isn't in the convention agent image) — same lane as verify.
	image := s.workspaceRunImage(ctx, runID, ws)

	// The model provider is part of the HARNESS the operator configured (getting
	// started), not per-workspace app egress they approve — so its host must be
	// reachable in EVERY agent session, confined replay included. A learning session
	// is AllowAllEgress so it's fine; a confined replay's AllowedDomains is
	// baseline+approved and would NOT list api.anthropic.com, which makes
	// applyLLMCredMount refuse the subscription mount (anthropicReachable=false) and
	// silently fall back to a broken api-key path. Union the ceiling's model-provider
	// egress in first so subscription/api-key wiring below attaches in both modes.
	unionAllowedDomains(&policy, s.modelProviderEgress(s.cfg.DefaultPolicy))

	// Model access for the session comes from the WORKSPACE's OWN binding (SPINE-7)
	// — the same resolveRunIntegration precedence (explicit → workspace pin →
	// operator default) a real run of this workspace uses — not just the operator
	// ceiling's convention secret. A confined replay whose job is to PROVE least
	// privilege must authenticate on the SAME credential path a real run will, or
	// its capture (and the promotion candidates derived from it) reflect a different
	// transport. A synthetic claude-code request with this one workspace as wsRefs
	// drives the identical fold launch/preflight run; bedrockRef is threaded into
	// dispatch below so a bedrock-bound workspace records its OWN region, not the
	// global default.
	var injections []runner.InjectionGrant

	// The workspace's REQUIRED contract rows ride a session the SAME way they
	// ride a real run (SEAM-2): a confined replay whose install step needs
	// Artifactory (say) or a declared secret must reach AND authenticate to
	// it, not merely reach it — without this the held-then-approved request
	// the operator approves below goes out credential-less and 401s, and the
	// approve hook (learnVerifyEgress) durably writes a DUPLICATE egress: row
	// for a host a integration: or secret: row already provides. Folded
	// through the SAME chokepoint a real run uses (applyWorkspaceRequirements,
	// runs_create.go) — not a hand-rolled integration:-only loop, which
	// silently dropped required secret: rows even though the Verify carry
	// card promises "N required secrets ride proxy-side" (W8-S1-2). nil
	// selections: only Required rows apply — a confined replay has no per-run
	// optional opt-in surface. Independent of the LLM mode below — a
	// requirement credential is never skipped just because this session
	// happens to be subscription-mounted.
	before := len(policy.EligibleGrants)
	_ = s.applyWorkspaceRequirements(ctx, &policy, "claude-code", []types.Workspace{ws}, nil)
	minted, ierr := s.mintRecordAPIKeyInjections(ctx, runID, now, policy.EligibleGrants[before:])
	if ierr != nil {
		return types.AgentRun{}, false, abort(fmt.Errorf("create requirement grant: %w", ierr))
	}
	injections = append(injections, minted...)
	// llmGrantsBefore fences the fallback mint below to ONLY what IT adds: the
	// fold above already minted (and audited) the requirement grants — reusing
	// the full policy.EligibleGrants slice there would remint and re-inject
	// every one of them a second time.
	llmGrantsBefore := len(policy.EligibleGrants)

	// Unconditional, same as launch/preflight for a real run (W20-W20-llm-transport-matrix-1):
	// foldRunIntegration already resolves the workspace's OWN binding first and only
	// falls through to the operator's site-wide DefaultFor:agent_runs integration when
	// the workspace names nothing — it returns kind=="" when neither resolves, so the
	// ceiling/convention fallback below stays the last resort exactly as before. Gating
	// this call on the workspace carrying its own binding skipped tier 3 (the operator's
	// site-wide default) for every unbound workspace's record/replay session, silently
	// diverging from "Model access resolves" (docs/OPERATIONS.md).
	_, integKind, bedrockRef := s.foldRunIntegration(ctx, "", &policy, createRunRequest{Agent: "claude-code"}, []types.Workspace{ws})
	subMounted := specHasMountTarget(&policy, claudeCredTarget)
	if integKind == "" && !subMounted {
		// No workspace/operator integration bound: fall back to the operator
		// ceiling's convention subscription mount, else a brokered api-key grant
		// (today's behavior for an unbound workspace).
		if m, _ := applyLLMCredMount(&policy, s.cfg.DefaultPolicy, "claude-code", true); m {
			subMounted = true
		} else {
			s.ensureLLMGrant(&policy, "claude-code", s.presentSecretNames(ctx), false)
		}
	}
	llmMode := "none"
	switch {
	case subMounted, integKind == "anthropic_subscription":
		llmMode = "subscription" // managed subscription is injected proxy-side by dispatch
	case integKind == "bedrock" || bedrockRef != nil:
		llmMode = "bedrock" // dispatch's resolveBedrockAuth wires it from bedrockRef below
	}
	if !subMounted {
		// Build the injection from whatever api_key grant the FALLBACK just added
		// (llmGrantsBefore: the fold's own grants above are already minted) —
		// mirrors handleCreateRun's api_key branch (a subscription/bedrock fold
		// adds none: managed is injected proxy-side, Bedrock via resolveBedrockAuth).
		minted, ierr := s.mintRecordAPIKeyInjections(ctx, runID, now, policy.EligibleGrants[llmGrantsBefore:])
		if ierr != nil {
			return types.AgentRun{}, false, abort(fmt.Errorf("create llm grant: %w", ierr))
		}
		injections = append(injections, minted...)
		if len(injections) > 0 && llmMode == "none" {
			llmMode = "api-key"
		}
	}

	// Save the resolved auth mode + model onto the session entry so it's visible and a
	// later confined replay reflects the SAME provider the operator configured (not a
	// guess). Guarded on `recording`: a superseding re-record must not resurrect this
	// entry.
	_, _, _ = s.putRecordResult(ctx, ws.ID, sessionKey, RecordTaskResult{
		RunID: runID, Label: sessionLabel, Mode: recordModeInteractive, Confined: confined,
		Status: recordStatusRecording, StartedAt: startedAt,
		LLMMode: llmMode, Model: s.cfg.AgentAnthropicModel,
		Caveats: repoDevcontainerImageCaveats(ws),
	}, recordStatusRecording)

	// Sessions are interactive (the operator drives the activity in the attach
	// shell); no auto command plan. The `--idle` path clones the repo + attaches.
	var resolvedManaged bool
	result := s.dispatchAndSettle(ctx, created, ceilingForDispatch(ceiling), dispatchParams{
		RunToken:           runToken,
		Image:              image,
		Policy:             policy,
		FirstGitHubGrantID: ghGrantID,
		GitGrants:          gitBrokerGrant(primaryCloneURL, ghGrantID),
		SSHGrants:          sshGrants,
		Injections:         injections,
		// The workspace's own Bedrock region/model (SPINE-7) — nil for a non-bedrock
		// binding, so dispatch keeps the global config exactly as before.
		BedrockRef:  bedrockRef,
		Interactive: true,
		// A record/verify session runs ONE workspace — its scans decide the
		// toolchain env, same rule as an ordinary workspace run.
		Toolchains: runToolchainNeeds([]types.Workspace{ws}),
		// Any ephemeral source's scratch target — wireWorkspaceSource's doc
		// comment — surfaced the same way the ordinary create-run path does.
		EphemeralDirs: ephemeralDirs,
		// A member-owned workspace's local_dir stays inside that member's roots
		// even on an ADMIN-launched record/verify session: the SOURCE was
		// member-authored, so the bind-time gate follows the source rather than
		// the launcher. The zero posture for an operator-owned workspace (today's
		// path) — and even for a member's, it gates that workspace's OWN binds
		// only, never the session's operator-staged credential mounts.
		MemberMounts: s.memberMountPosture([]types.Workspace{ws}),
		// W20-llm-transport-matrix-2: the pre-dispatch llmMode guess above
		// cannot see the Wardyn-managed subscription lane at all — correct it
		// below against what dispatch ACTUALLY resolved.
		ResolvedManaged: &resolvedManaged,
	})
	// The mount/integration-based guess above already covers a host-staged
	// subscription and a bound Bedrock/api-key integration; only the managed
	// lane can flip "none"/"api-key" to "subscription" post-dispatch (the
	// fallback grant it should have preempted was never minted in that case —
	// see resolveLLMTransport's managed precedence comment).
	if resolvedManaged && llmMode != "subscription" {
		_, _, _ = s.putRecordResult(ctx, ws.ID, sessionKey, RecordTaskResult{
			RunID: runID, Label: sessionLabel, Mode: recordModeInteractive, Confined: confined,
			Status: recordStatusRecording, StartedAt: startedAt,
			LLMMode: "subscription", Model: s.cfg.AgentAnthropicModel,
			Caveats: repoDevcontainerImageCaveats(ws),
		}, recordStatusRecording)
	}
	return result, weakCC, nil
}
