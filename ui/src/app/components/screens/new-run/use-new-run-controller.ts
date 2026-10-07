/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { useLocation, useNavigate } from "react-router-dom";
import {
  CC_ORDER as ORDERED_CLASSES,
  type ConfinementClass,
  type RunPolicySpec,
  type SetupHarnessTool,
  type SetupModelProvider,
  type SetupProviderAccess,
} from "../../../lib/types";
import { policies as policiesApi } from "../../../lib/api/policies";
import { health as healthApi, type PolicyRef } from "../../../lib/api/health";
import { runs as runsApi } from "../../../lib/api/runs";
import { setup as setupApi } from "../../../lib/api/setup";
import type { SCMAccessPAT } from "../../../lib/types/ado-pat";
import { hasLlmPath } from "../../../lib/readiness";
import { useWorkspaceList } from "../../../lib/use-workspace-list";
import { useMyCapabilities } from "../../../lib/capabilities";
import {
  useOperator,
  useOperatorResolved,
  useSecurityOperator,
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
import { useDefaultPolicy } from "./use-default-policy";

export function useNewRunController() {
  const navigate = useNavigate();
  const { workspaces, reload: reloadWorkspaces } = useWorkspaceList();
  // Visibility is not capability: the workspace list is NOT narrowed by the
  // `workspace` grant (a hidden workspace makes the launch gate's refusal
  // unexplainable and the grant undiscoverable). Ungranted rows are annotated
  // instead.
  const operator = useOperator();
  const securityOperator = useSecurityOperator(), operatorResolved = useOperatorResolved(), caps = useMyCapabilities(!operator);
  // B4b — "Start a run like this one". The run cockpit hands the prefill over
  // in navigation state (navigate("/runs/new", { state: { prefill } })) rather
  // than through a query string or a second GET: it is already holding the run
  // row and the run.create audit event, so this screen re-reads nothing and —
  // the part that matters for a member — opens no new read path. Who may see a
  // run stays the server's getRunAuthorized question, answered before the
  // cockpit rendered at all.
  const prefill = (useLocation().state as { prefill?: RunPrefill } | null)?.prefill;
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
  // dirty and break Esc entirely.
  const pristineSpec = React.useRef(specText);
  const pristineCc = React.useRef(state.confinementClass);
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
  const [savedPolicies, setSavedPolicies] = React.useState<{ id: string; name: string; spec: RunPolicySpec }[]>([]);
  const [policiesLoaded, setPoliciesLoaded] = React.useState(false); // F2-F5: has listPolicies() answered?
  // Whether the barrier probe has SETTLED (null availableClasses after settle
  // means the check failed — unknown, never "confirmed absent").
  const [probeSettled, setProbeSettled] = React.useState(false);
  // null = not answered yet. An agent run with no model path launches and then
  // fails its first model call, so the rail must say so BEFORE launch rather
  // than promising credentials that cannot be minted.
  const [llmReady, setLlmReady] = React.useState<boolean | null>(null);
  // SetupStatus.harnesses — absent while unfetched or failed, same "unknown
  // stays unknown" rule as llmReady above (AgentPicker's own fallback).
  const [harnesses, setHarnesses] = React.useState<SetupHarnessTool[] | undefined>(undefined);
  // #542/#922 — this person's own model providers (already filtered to what
  // they may use, #832/#1015) and connection state. Same "unknown stays
  // unknown" rule as harnesses: undefined until the read lands, which also
  // keeps the rail's provider picker from rendering (and forcing a
  // preselection) before there is anything to pick from, and is the one
  // member-safe signal for #922's pinned-provider-availability check. Read off
  // the SAME /setup/status fetch below, never a second one.
  const [modelProviders, setModelProviders] = React.useState<SetupModelProvider[] | undefined>(undefined);
  const [providerAccess, setProviderAccess] = React.useState<SetupProviderAccess[] | undefined>(undefined);
  const [adoCeiling, setAdoCeiling] = React.useState<string[] | undefined>(undefined);
  // The caller's own Azure DevOps answer, for a row that creates a token per run.
  const [adoAccess, setAdoAccess] = React.useState<SCMAccessPAT | undefined>(undefined);
  // Existing run titles, offered as a native <datalist> — grouping is by
  // EXACT string, so a family needs a character-perfect retype without it.
  const [knownTitles, setKnownTitles] = React.useState<string[]>([]);
  // #1197 L2: Title tracks the task's first line (titleFromTask) until the
  // operator edits it themselves — a clone's carried-over title, or clearing
  // the field by hand, both count as an edit and must not be fought.
  const [titleUserEdited, setTitleUserEdited] = React.useState(!!prefill?.state.title);
  // GET /policies/default: the caller's ceiling. The governance profile bounding
  // THIS caller and its floor come off it, undefined for no assignment or a
  // failed read — either way the rail's ceiling section simply does not render,
  // never claiming a ceiling it could not confirm.
  const { read: defaultRead, retry: retryDefault } = useDefaultPolicy();
  const defaultPolicy = defaultRead.status === "ready" ? defaultRead.policy : undefined;
  const governanceProfile = defaultPolicy?.governance_profile_name;
  // GET /me's governance_contact: who to ask about the policy bounding this
  // caller. Undefined until /me answers, and when it answers null or fails.
  const [governanceContact, setGovernanceContact] = React.useState<PolicyRef | undefined>(undefined);
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

  React.useEffect(() => {
    runsApi
      .listRuns()
      .then((rs) =>
        setKnownTitles(
          [...new Set(rs.map((r) => (r.title ?? "").trim()).filter(Boolean))].sort(),
        ),
      )
      .catch(() => {
        /* the datalist simply offers nothing — never blocks a launch */
      });
  }, []);

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

  // ONE /setup/status read for everything this screen needs: model-access
  // readiness, the harness catalog, and which barriers this host can build —
  // runner.confinement_classes, the same field every other surface reads, never
  // a separately-polled mirror. `unreachable` distinguishes "couldn't check"
  // from a real empty list, so there is no retry-on-empty heuristic to
  // reimplement.
  React.useEffect(() => {
    let alive = true;
    setupApi
      .getSetupStatus()
      .then((st) => {
        if (!alive) return;
        setLlmReady(st.unreachable ? null : hasLlmPath(st));
        setHarnesses(st.harnesses);
        // An absent model_providers key means no provider block (nothing to
        // enforce); a present `[]` means a block that grants this caller
        // nothing. resolvedModelProviders keeps that distinction and folds an
        // unreachable read to undefined, off the same bit this effect reads.
        setModelProviders(resolvedModelProviders(st));
        setProviderAccess(st.unreachable ? undefined : st.provider_access);
        setAdoCeiling(st.unreachable ? undefined : st.scm_access?.capability_ceiling);
        setAdoAccess(st.unreachable ? undefined : (st.scm_access as SCMAccessPAT | undefined));
        // Every path that leaves the class list unread still SETTLES the probe:
        // that is what draws the unknown-barrier line (probeSettled with no
        // availableClasses). Setting it only beside a real list made that
        // state unreachable.
        if (st.unreachable) {
          setProbeSettled(true);
          return;
        }
        setVaultReason(vaultRequirementReason(st.runner.driver, st.platform, st.runner.kubernetes));
        const classes = (st.runner.confinement_classes ?? []).filter(Boolean);
        // No runner AT ALL (environment-step.tsx's own noDriver fold — a
        // member's redacted Driver:"" WITH classes is a withheld NAME, not
        // no-driver) is UNKNOWN here, not "nothing installed": the capability
        // gate (runs_create.go) is skipped entirely with no runner configured.
        const noDriver = st.runner.driver === "none" || (st.runner.driver === "" && classes.length === 0);
        if (noDriver) {
          setProbeSettled(true);
          return;
        }
        setAvailableClasses(classes);
        setProbeSettled(true);
        // B4b: a CLONE's barrier is the SOURCE RUN's — kept explicit
        // (ccTouched) as long as this host can build it. A vanished tier
        // falls back like any fresh run and stops counting as explicit.
        const cloned = clonedCc.current;
        const cloneStillAvailable = !!cloned && classes.includes(cloned);
        const resolved = cloneStillAvailable ? cloned : strongestAvailable(classes) ?? "CC1";
        pristineCc.current = resolved;
        setState((s) => ({ ...s, confinementClass: resolved }));
        if (!cloneStillAvailable) setCcTouched(false);
      })
      .catch(() => {
        /* unknown stays unknown — never claim a missing model path, or a
           confirmed-absent barrier, on a blip */
        if (alive) setProbeSettled(true);
      });
    return () => {
      alive = false;
    };
  }, []);

  React.useEffect(() => {
    policiesApi
      .listPolicies()
      .then((ps) => {
        setSavedPolicies(ps.map((p) => ({ id: p.id, name: p.name, spec: p.spec })));
        setPoliciesLoaded(true);
      })
      .catch(() => {
        /* the Saved-policy lane simply offers nothing — never blocks a launch */
      });
  }, []);

  React.useEffect(() => {
    let alive = true;
    void healthApi.whoami().then((me) => {
      if (alive) setGovernanceContact(me?.governance_contact ?? undefined);
    });
    return () => {
      alive = false;
    };
  }, []);

  // useWorkspaceList does NOT fetch on mount — every caller loads it itself.
  // Without this, a workspace onboarded elsewhere (Getting started, the
  // Workspaces screen) could never be attached to a run from this page.
  React.useEffect(() => {
    reloadWorkspaces();
    // eslint-disable-next-line react-hooks/exhaustive-deps -- run once on mount; reload is stable (useCallback([]))
  }, []);

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
    pristineCc,
  });

  // The up-clamp only ever RAISES the pick. While /me is unresolved the
  // governance floor binds (fail closed), so an admin's untouched pick can be
  // raised to a tier this host lacks; once the floor lifts, re-seed it to the
  // strongest tier that now qualifies.
  React.useEffect(() => {
    if (ccTouched || !policy.qualifying?.length || policy.qualifying.includes(cc)) return;
    const next = policy.qualifying[policy.qualifying.length - 1];
    pristineCc.current = next;
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

  // Launch + preflight state and actions — see use-launch.ts's header for why
  // this lane is a hook rather than a pure function like policy-lane.ts's.
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
    preflightRefusal,
  } = useLaunch({
    state,
    workspaces,
    modelProviders,
    policyMode,
    ccTouched,
    merged: policy.merged,
    onLaunchError: policy.adoDoor.notifyLaunchError,
    autoCheck: {
      local: !gates.problem && !gates.workspaceUnavailable && !policy.noBarrierOnHost,
      // No runner configured: Launch is not refused, so the backend row never holds it.
      backendArm: !policy.noBarrierOnHost && !!availableClasses,
      modelArm: isAgent && !isInteractive,
    },
    doorOpen: modelAccessDoor.open,
  });

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

  // Picking a policy REPLACES the body, so the textarea always shows what will
  // actually govern the run even while the reference path is what ships.
  const onPickPolicy = (id: string) => {
    const p = savedPolicies.find((x) => x.id === id);
    if (p) setSpecText(JSON.stringify(p.spec, null, 2));
    patch({ selectedPolicyId: id });
  };

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
  const dirty =
    policyMode !== "custom" ||
    specText !== pristineSpec.current ||
    JSON.stringify(state) !== JSON.stringify(initialWizardState(pristineCc.current, undefined, modelProviders));
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
    governanceProfile, retryDefault, securityOperator, operatorResolved,
    setSpecText, setPolicyMode, onPickPolicy, savedPolicies,
    adoAccess, operator, hasAdditions, added,
    governanceContact, llmReady, pushRules, unattended,
    launch, launchDisabled, launchSpinning, launching,
    policiesLoaded, pin, error, errorSeq,
    errorPolicy, credentialRefused, refusedProvider, currentBody,
    preflightRefusal, preflightIsCurrent, preflightFresh, preflightBlock,
    preflightNotChecked, preflightError, preflightErrorSeq, preflightResult,
    providerCandidates, providerAccess, onModelProviderChange, providerChangeNote,
    providerGateState, addWsOpen, reloadWorkspaces,
  };
}
