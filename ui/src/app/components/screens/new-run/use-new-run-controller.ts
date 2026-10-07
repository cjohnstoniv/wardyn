/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { useLocation, useNavigate } from "react-router-dom";
import {
  CC_ORDER as ORDERED_CLASSES,
  type ConfinementClass,
} from "../../../lib/types";
import type { SCMAccessPAT } from "../../../lib/types/ado-pat";
import { getAuthGeneration } from "../../../lib/api/core";
import { hasLlmPath } from "../../../lib/readiness";
import { useMyCapabilities } from "../../../lib/capabilities";
import {
  useOperator,
  useOperatorResolved,
  useUserDrive,
} from "../../wardyn/operator-context";
import { strongestAvailable } from "../../wardyn/default-confinement";
import { type PolicyMode } from "../../wardyn/policy-panel";
import { vaultRequirementReason } from "../setup/environment-step";
import { defaultSpecText } from "./policy-lane";
import {
  agentLabel,
  initialWizardState,
  seedAllowedDomains,
  primaryWorkspaceId,
  resolvedModelProviders,
  titleFromTask,
  workspacePin,
  type RunPrefill,
  type WizardState,
} from "./wizard-types";
import { useModelAccessDoor } from "../../wardyn/model-access-context";
import { useLaunch } from "./use-launch";
import { launchGates } from "./new-run-launch-gates";
import { providerCandidates as candidatesForAgent, providerGate } from "./model-provider-lane";
import { useModelProviderPick } from "./use-model-provider-pick";
import { useNewRunPolicy } from "./use-new-run-policy";
import { useNewRunSources } from "./use-new-run-sources";
import { useDraftIdentity } from "./use-run-checks";

export function useNewRunController() {
  const navigate = useNavigate();
  const identity = useDraftIdentity();
  const sources = useNewRunSources(identity);
  const { workspaces, savedPolicies, policiesLoaded, knownTitles, governanceContact, defaultRead } = sources;
  const refreshSources = sources.refresh, retryDefault = refreshSources, reloadWorkspaces = refreshSources;
  // Visibility is not capability: the workspace list is NOT narrowed by the
  // `workspace` grant (a hidden workspace makes the launch gate's refusal
  // unexplainable and the grant undiscoverable). Ungranted rows are annotated
  // instead.
  const operator = useOperator();
  const operatorResolved = useOperatorResolved(), caps = useMyCapabilities(!operator && identity.resolved && identity.authGeneration === getAuthGeneration(), sources.revision);
  // B4b — "Start a run like this one". The run cockpit hands the prefill over
  // in navigation state (navigate("/runs/new", { state: { prefill } })) rather
  // than through a query string or a second GET: it is already holding the run
  // row and the run.create audit event, so this screen re-reads nothing and —
  // the part that matters for a member — opens no new read path. Who may see a
  // run stays the server's getRunAuthorized question, answered before the
  // cockpit rendered at all.
  const prefillRef = React.useRef((useLocation().state as { prefill?: RunPrefill } | null)?.prefill);
  const prefill = prefillRef.current;
  // Seed with CC1 — a harmless placeholder the /setup/status effect below
  // replaces with the server's own strongest-installed-class default. There is
  // no per-browser default left to seed this from (see default-confinement.ts).
  const [state, setState] = React.useState<WizardState>(() =>
    initialWizardState("CC1", prefill?.state),
  );
  // `policyMode` is the mode row: launch under the deployment's default policy
  // (no policy sent), reuse a stored policy by REFERENCE (policy_id), or author
  // one here. A clone of a run that launched by reference opens in that mode —
  // otherwise the picker would hold the id while the panel showed an authored
  // document nobody wrote. Custom stays the preselected mode otherwise.
  const [policyMode, setPolicyMode] = React.useState<PolicyMode>(
    prefill?.state.selectedPolicyId ? "saved" : "custom",
  );
  // The DEFAULT body floors at CC1 — NOT Minimal's authored CC2. A hardcoded
  // CC2 default would open every fresh /runs/new on a Fence-only host
  // fail-closed, all tiers dead, before the operator authored anything.
  // Clicking the Minimal CHIP afterwards is an authored act and still floors
  // CC2 — that corner stays, with its reason line and preflight naming it.
  const [specText, setSpecText] = React.useState(() => defaultSpecText());
  // The floor the LAST SUCCESSFUL parse authored — sticky across a broken
  // edit: a half-typed document must not momentarily drop the floor and
  // re-open a barrier tier the operator's own policy forbids.
  const [parsedFloor, setParsedFloor] = React.useState<ConfinementClass | undefined>("CC1");
  // What `dirty` below compares against. The barrier is the one field the
  // machine writes on its own (probe/floor up-clamp), so its baseline moves
  // with those writes — a constant baseline would call an untouched form
  // dirty and break Esc entirely. The class baseline is state, set beside the
  // class itself: a ref written ahead of that render made the form read dirty
  // for every render in between.
  const pristineSpec = React.useRef(specText);
  const [pristineCc, setPristineCc] = React.useState(state.confinementClass);
  // Whether the Barrier control carries an EXPLICIT pick (a clone's
  // carried-over class counts, B4b). Untouched, the server's own default
  // decides and its audit trail reads `defaulted`, not `requested`.
  const [ccTouched, setCcTouched] = React.useState(!!prefill?.state.confinementClass);
  // The clone's barrier as seeded, read once like the useState seeds above:
  // New run navigates to /runs/new with no state, keeping this screen mounted
  // and clearing `prefill` — that must not re-run the /setup/status effect
  // below and half-reset the form.
  const clonedCc = React.useRef(prefill?.state.confinementClass);
  const [addWsOpen, setAddWsOpen] = React.useState(false);
  const [availableClasses, setAvailableClasses] = React.useState<ConfinementClass[] | null>(null);
  const probeSettled = !sources.pending;
  const setup = sources.setup;
  const llmReady = setup && !setup.unreachable ? hasLlmPath(setup) : null;
  const harnesses = setup?.harnesses;
  const modelProviders = setup ? resolvedModelProviders(setup) : undefined;
  const providerAccess = setup?.unreachable ? undefined : setup?.provider_access;
  const adoCeiling = setup?.unreachable ? undefined : setup?.scm_access?.capability_ceiling;
  const adoAccess = setup?.unreachable ? undefined : setup?.scm_access as SCMAccessPAT | undefined;
  // #1197 L2: Title tracks the task's first line (titleFromTask) until the
  // operator edits it themselves — a clone's carried-over title, or clearing
  // the field by hand, both count as an edit and must not be fought.
  const [titleUserEdited, setTitleUserEdited] = React.useState(!!prefill?.state.title);
  // GET /policies/default: the caller's ceiling. The governance profile bounding
  // THIS caller and its floor come off it, undefined for no assignment or a
  // failed read — either way the rail's ceiling section simply does not render,
  // never claiming a ceiling it could not confirm.
  const defaultPolicy = defaultRead.status === "ready" ? defaultRead.policy : undefined;
  const governanceProfile = defaultPolicy?.governance_profile_name;
  // GET /me's governance_contact: who to ask about the policy bounding this
  // caller. Undefined until /me answers, and when it answers null or fails.
  // #1200 — the SAME read's min_confinement_class, the governance ceiling's
  // own floor (composer.Clamp raises the run to it, internal/composer/clamp.go).
  // Undefined for the same two reasons governanceProfile is; the Barrier
  // control then falls back to the authored floor alone.
  const govFloor = (ORDERED_CLASSES as string[]).includes(defaultPolicy?.min_confinement_class ?? "")
    ? (defaultPolicy?.min_confinement_class as ConfinementClass)
    : undefined;
  // #1200 review P2-6/R2-4 — Vault's driver-aware reason (the /dev/kvm probe
  // on docker, a Kata RuntimeClass on k8s), so T-9 names the SAME honest
  // reason environment-step.tsx computes instead of a generic "not installed".
  const [vaultReason, setVaultReason] = React.useState<string | undefined>(undefined);
  // The Workspace card's drive block: this caller's allocation (nil means none)
  // and door ("" means open), read off the shell's ONE GET /me
  // (operator-context.tsx's useUserDrive) rather than a second read of this
  // screen's own. With no provider above, on an older daemon, or after a
  // failed read it is null/"" — which renders as today's card, the same
  // answer the server's own resolver gives.
  const { drive: userDrive, deniedByProfile: driveDeniedBy, unavailable: driveUnavailable } = useUserDrive();

  // #1197 L2: the title default tracks the task's first line until the
  // operator writes their own. Keyed on state.task (and the edited flag) alone,
  // so it never fires on an unrelated field's change.
  React.useEffect(() => {
    if (titleUserEdited) return;
    setState((s) => ({ ...s, title: titleFromTask(s.task) }));
  }, [state.task, titleUserEdited]);

  // An untouched form opens on the starters that follow the model providers,
  // once /setup/status has named them: the policy body and the Network seed.
  React.useEffect(() => {
    if (!modelProviders) return;
    const seeded = defaultSpecText(modelProviders);
    const was = pristineSpec.current;
    setSpecText((prev) => (prev === was ? seeded : prev));
    pristineSpec.current = seeded;
    if (!prefill?.state.allowedDomains) {
      setState((s) => (JSON.stringify(s.allowedDomains) === JSON.stringify(seedAllowedDomains()) ? { ...s, allowedDomains: seedAllowedDomains(modelProviders) } : s));
    }
    // prefill is read once, like the useState seeds above.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [modelProviders]);

  const touchedRef = React.useRef(ccTouched);
  touchedRef.current = ccTouched;
  React.useEffect(() => {
    setAvailableClasses(null);
    setVaultReason(undefined);
    if (!setup?.runner || setup.unreachable) return;
    setVaultReason(vaultRequirementReason(setup.runner.driver, setup.platform, setup.runner.kubernetes));
    const classes = (setup.runner.confinement_classes ?? []).filter(Boolean);
    if (setup.runner.driver === "none" || (setup.runner.driver === "" && classes.length === 0)) return;
    setAvailableClasses(classes);
    if (touchedRef.current && !clonedCc.current) return;
    const cloned = clonedCc.current;
    const cloneStillAvailable = !!cloned && classes.includes(cloned);
    const resolved = cloneStillAvailable ? cloned : strongestAvailable(classes) ?? "CC1";
    setPristineCc(resolved);
    setState((old) => ({ ...old, confinementClass: resolved }));
    if (!cloneStillAvailable) setCcTouched(false);
    clonedCc.current = undefined;
  }, [setup]);

  const [draftOwner, setDraftOwner] = React.useState(identity.principal);
  const [submittedDraft, setSubmittedDraft] = React.useState<string | null>(null);
  const draftSnapshot = JSON.stringify([policyMode, specText, state]);
  const ownerCurrent = draftOwner === identity.principal;
  React.useEffect(() => {
    if (!identity.resolved || !identity.principal || ownerCurrent) return;
    prefillRef.current = undefined;
    clonedCc.current = undefined;
    const text = defaultSpecText();
    setSpecText(text);
    pristineSpec.current = text;
    setPristineCc("CC1");
    setParsedFloor("CC1");
    setState(initialWizardState("CC1"));
    setCcTouched(false);
    setPolicyMode("custom");
    setTitleUserEdited(false);
    setSubmittedDraft(null);
    setDraftOwner(identity.principal);
  }, [identity.principal, identity.resolved, ownerCurrent]);

  // The envelope-field detach funnel retired with the controls it guarded;
  // confinementClass is now guarded by the floor-DISABLE below instead, which
  // is strictly stronger: a one-time up-clamp alone would re-open the
  // below-floor 422 (runs_create.go's floor check, on both the policy_id and
  // inline paths) the moment the operator lowered the Seg afterwards. Detach
  // now has exactly one trigger: editing the spec text (see onSpecChange).
  const patch = React.useCallback((p: Partial<WizardState>) => setState((s) => ({ ...s, ...p })), []);

  const isAgent = state.runType === "agent";
  const cc = state.confinementClass;
  // A shell command is unattended by definition — buildSpec forces batch for
  // one, so the Run mode segment is hidden rather than offering a combination
  // that would silently drop the command.
  const isInteractive = isAgent && state.mode === "interactive";
  // Shared display name (wizard-types.agentLabel) — a local re-hardcode here
  // is exactly the drift that helper's doc says it exists to prevent.
  const agentName = agentLabel(state.agent);

  // The policy this run authors or references — floor, the post-parse merge,
  // tool_rules and the ADO launch door — see use-new-run-policy.ts's header
  // for why this lane is a hook rather than a pure function like
  // policy-lane.ts's.
  const policy = useNewRunPolicy({
    state,
    patch,
    policyMode,
    specText,
    parsedFloor,
    setParsedFloor,
    savedPolicies,
    availableClasses,
    probeSettled,
    governanceProfile,
    govFloor,
    // Fail CLOSED to member until /me has answered (or if it never does): the
    // default `operator` is a fail-open TRUE, which would skip the governance
    // floor and offer a tier the server then refuses.
    operator: operator && operatorResolved,
    workspaces,
    modelProviders,
    setPristineCc,
  });

  // The up-clamp only ever RAISES the pick. While /me is unresolved the
  // governance floor binds (fail closed), so an admin's untouched pick can be
  // raised to a tier this host lacks; once the floor lifts, re-seed it to the
  // strongest tier that now qualifies.
  React.useEffect(() => {
    if (ccTouched || !policy.qualifying?.length || policy.qualifying.includes(cc)) return;
    const next = policy.qualifying[policy.qualifying.length - 1];
    setPristineCc(next);
    patch({ confinementClass: next });
  }, [ccTouched, policy.qualifying, cc, patch]);

  // #542 — this agent's own model-provider candidates (access-filtered
  // server-side, #1015). `undefined` modelProviders means "unknown" — no
  // picker, not a false "no provider serves this agent" (R9's shape).
  const providerCandidates = isAgent && modelProviders ? candidatesForAgent(modelProviders, state.agent) : [];

  // F2 (#612) — the primary workspace's own pinned provider (llm_cred.provider_ref),
  // the SAME "pin" the server falls back to (cmp.Or(requested, pin),
  // run_model_provider.go) — read the primary the same way the server does
  // (wizard-types.ts's primaryWorkspaceId), so this rail can never pin a
  // different workspace's credential than the run actually inherits.
  const primaryWsId = primaryWorkspaceId(state.workspaces, workspaces);
  // A pin the server hid (#1018) still pins: workspacePin makes it one no
  // candidate matches, so nothing is preselected and nothing substituted.
  const pin = workspacePin(workspaces.find((w) => w.id === primaryWsId));

  // #1052 — this agent's own providers_ungranted fact, off the same
  // `harnesses` state as agentRow below: serving > 0 && granted == 0.
  const providersUngranted = !!harnesses?.find((h) => h.id === state.agent)?.providers_ungranted;

  // #542 rail-gap packet — R5b/R5c; undefined for R1-R4/R6-R9.
  const providerGateState =
    isAgent && modelProviders ? providerGate(modelProviders, state.agent, providersUngranted) : undefined;

  // Which provider (if any) is preselected, and the person's own pick
  // (use-model-provider-pick.ts).
  const { providerChangeNote, onModelProviderChange } = useModelProviderPick({
    state,
    isAgent,
    modelProviders,
    providerAccess,
    providerCandidates,
    pin,
    providerGateState,
    patch,
  });

  // The same local gates the launch panel renders: an automatic preflight may
  // fire only when Launch would otherwise be pressable.
  const gates = launchGates({
    isAgent,
    mode: state.mode,
    task: state.task,
    policyMode,
    specParsedOk: policy.parsed.ok,
    selectedPolicyId: state.selectedPolicyId,
    savedPolicy: policy.selectedPolicy,
    policiesLoaded,
    pin,
    workspaces,
    selectedWorkspaceId: state.workspaces[0]?.workspaceId,
    attachedWorkspaces: state.workspaces.length,
    caps,
    modelProviders,
    providerGateState,
    providerCandidates,
    selectedModelProviderId: state.modelProviderId,
    agentName,
  });
  const modelAccessDoor = useModelAccessDoor();

  const {
    launching,
    launchDisabled,
    launchSpinning,
    error,
    errorSeq,
    errorPolicy,
    credentialRefused,
    refusedProvider,
    launch,
    preflighting,
    preflightResult,
    preflightError,
    preflightErrorSeq,
    preflightIsCurrent,
    preflightFresh,
    preflightBlock,
    preflightNotChecked,
    preflight,
    currentBody,
    draftRevision,
    preflightRefusal,
    invalidateChecks,
    preview,
  } = useLaunch({
    state,
    workspaces,
    modelProviders,
    policyMode,
    ccTouched,
    merged: policy.merged,
    onLaunchError: policy.adoDoor.notifyLaunchError,
    autoCheck: {
      local: !gates.problem && !gates.referenceWorkspaceBlocked && !gates.workspaceUnavailable && !policy.noBarrierOnHost,
      // No runner configured: Launch is not refused, so the backend row never holds it.
      backendArm: !policy.noBarrierOnHost && !!availableClasses,
      modelArm: isAgent && !isInteractive,
    },
    doorOpen: modelAccessDoor.open,
    adoDoorOpen: policy.adoDoor.dialog.open,
    identity: { ...identity, resolved: identity.resolved && ownerCurrent },
    externalRevision: sources.revision,
    sourceRefreshPending: sources.pending,
    onCreated: () => setSubmittedDraft(draftSnapshot),
  });

  const doors = React.useRef({ model: modelAccessDoor.open, ado: policy.adoDoor.dialog.open });
  React.useEffect(() => {
    if ((doors.current.model && !modelAccessDoor.open) || (doors.current.ado && !policy.adoDoor.dialog.open)) refreshSources();
    doors.current = { model: modelAccessDoor.open, ado: policy.adoDoor.dialog.open };
  }, [modelAccessDoor.open, policy.adoDoor.dialog.open, refreshSources]);

  const added = policy.added;
  const hasAdditions = !!added && (added.hosts.length > 0 || added.grants.length > 0 || added.mounts.length > 0 || added.repos.length > 0);
  // #181 — same source as the rail's toolRules (use-new-run-policy.ts's
  // specForRules); a shell command has no
  // specForRules at all (RunRail's pushRulesIsSet reads undefined as "no
  // section"). `unattended` mirrors isInteractive's own doc: a shell command
  // is unattended by definition, and so is an agent run left on batch mode.
  const pushRules = policy.specForRules?.push_rules;
  const unattended = !isInteractive;

  // Editing the spec text DETACHES a picked saved policy: the body on screen is
  // no longer the stored one, and launching by reference would ship a policy
  // nobody is looking at.
  const onSpecChange = (next: string) => {
    setSpecText(next);
    if (state.selectedPolicyId) patch({ selectedPolicyId: undefined });
  };

  // A reference never becomes authored source, including a redacted stored policy.
  const onPickPolicy = (id: string) => patch({ selectedPolicyId: id });
  const onPolicyModeChange = (mode: PolicyMode) => setPolicyMode(mode);

  // Rulebook §8: Esc backs out quietly, with no prompt for an untouched form.
  //
  // "Untouched" is the WHOLE form, not four scalar fields: an edited policy
  // body, an attached workspace, a changed barrier or run mode are all work
  // this would throw away. One whole-state comparison rather than a field list,
  // so a control added to this screen cannot quietly fall outside it.
  //
  // ponytail: a DIRTY form ignores Esc rather than prompting to discard — the
  // explicit discard prompt the rulebook asks for needs copy M4 does not draw,
  // and the ghost "Runs" button is still one click away. Wire the prompt when
  // the mock carries its words.
  const dirty = submittedDraft !== draftSnapshot && (
    policyMode !== "custom" ||
    specText !== pristineSpec.current ||
    JSON.stringify(state) !== JSON.stringify(initialWizardState(pristineCc, undefined, modelProviders)));
  React.useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      // defaultPrevented is load-bearing: Radix's DismissableLayer preventDefaults
      // Escape on document capture and THEN dismisses, but the event still
      // reaches window — so closing a Select or the Add-workspace dialog was also
      // leaving the screen.
      if (e.key !== "Escape" || e.defaultPrevented || dirty) return;
      void navigate("/runs");
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [dirty, navigate]);

  return {
    navigate, prefill, state, setTitleUserEdited,
    patch, knownTitles, isAgent, isInteractive,
    agentName, harnesses, workspaces, caps,
    modelProviders, setAddWsOpen, userDrive, driveDeniedBy,
    driveUnavailable, policy, cc, setCcTouched,
    probeSettled, availableClasses, vaultReason, specText,
    onSpecChange, preflight, preflighting, policyMode,
    gates, adoCeiling, defaultRead, defaultPolicy,
    governanceProfile, retryDefault, operatorResolved,
    onPolicyModeChange, onPickPolicy, savedPolicies,
    adoAccess, operator, hasAdditions, added,
    governanceContact, llmReady, pushRules, unattended,
    launch, launchDisabled, launchSpinning, launching,
    policiesLoaded, pin, error, errorSeq,
    errorPolicy, credentialRefused, refusedProvider, currentBody, draftRevision,
    preflightRefusal, invalidateChecks, preview, dirty, preflightIsCurrent, preflightFresh, preflightBlock,
    preflightNotChecked, preflightError, preflightErrorSeq, preflightResult,
    providerCandidates, providerAccess, onModelProviderChange, providerChangeNote,
    providerGateState, addWsOpen, reloadWorkspaces,
  };
}
