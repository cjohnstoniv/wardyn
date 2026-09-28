/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New run — ONE page, two columns, with a live rail that answers "what can this
// run actually do?" while you build it.
//
// Derived from the deleted Figma Make design snapshot's NewRunScreen. It
// replaces a five-step modal wizard whose Review screen
// was the first place the consequences of your choices appeared — by which
// point you had made all of them blind.
//
// It is a RE-LAYOUT, not a re-model. WizardState already carries every field
// this screen edits, and buildSpec()/impliedEgressHosts() are reused verbatim,
// so the launch payload and the policy it produces are exactly what the wizard
// produced. That is deliberate: the governance contract is the tested part, and
// this change is about when the operator SEES it, not what it is.
//
// The Confinement and Network cards are GONE. `inline_policy` on POST /runs is
// the identical Go struct a saved policy stores, validated by the same
// validator — so this screen and /policies now author it through ONE component
// (wardyn/policy-panel.tsx) instead of a bespoke form that could only ever
// assemble a subset of what the server already accepts. What the JSON cannot
// know — the Workspace card's mounts/repos, the grant lanes' grants and the
// egress hosts those grants require — is unioned back in by mergeRunSelections
// and NAMED on screen, never merged behind the operator's back.
import * as React from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { ArrowLeft } from "lucide-react";
import { toast } from "sonner";
import {
  CC_ORDER as ORDERED_CLASSES,
  type ConfinementClass,
  type RunPolicySpec,
  type SetupHarnessTool,
  type SetupModelProvider,
  type SetupProviderAccess,
  type Workspace,
} from "../../../lib/types";
import { Link } from "react-router-dom";
import { ccRank as rank, SectionCard } from "./new-run-primitives";
import { RunRail, useAdoLaunchDoor } from "./new-run-rail";
import { policies as policiesApi } from "../../../lib/api/policies";
import { runs as runsApi } from "../../../lib/api/runs";
import { setup as setupApi } from "../../../lib/api/setup";
import { hasLlmPath } from "../../../lib/readiness";
import { useWorkspaceList } from "../../../lib/use-workspace-list";
import { useMyCapabilities } from "../../../lib/capabilities";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Textarea } from "../../ui/textarea";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import { Field } from "../../wardyn/form-primitives";
import { Mono } from "../../wardyn/code-block";
import { Chip } from "../../wardyn/primitives";
import { useOperator, useOperatorResolved, useSecurityOperator, useUserDrive } from "../../wardyn/operator-context";
import { CC_META } from "../../wardyn/cc-meta";
import { RUN } from "../../wardyn/copy";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { strongestAvailable } from "../../wardyn/default-confinement";
import { PolicyPanel, parseSpec, toolRulesSummary, unparseableFloorClass } from "../../wardyn/policy-panel";
import { TierPicker, allowedFromFloor } from "../../wardyn/tier-picker";
import { vaultRequirementReason } from "../setup/environment-step";
import { TIER_PICKER } from "../../../lib/tier-picker-copy";
import { AddWorkspaceDialog } from "../add-workspace-dialog";
import { WorkspaceCard } from "./workspace-card";
import {
  barrierReasons,
  barrierRequirementReason,
  clearedSpecOnCustomSwitch,
  combineFloors,
  defaultSpecText,
  governanceRemovedTier,
  savedPolicyGone,
} from "./policy-lane";
import { mergeRunSelections } from "./wizard-spec";
import {
  agentLabel,
  initialWizardState,
  primaryWorkspaceId,
  resolvedModelProviders,
  titleFromTask,
  workspaceUnavailableToCaller,
  type RunPrefill,
  type WizardState,
} from "./wizard-types";
import { useLaunch } from "./use-launch";
import { providerCandidates as candidatesForAgent, providerGate } from "./model-provider-lane";
import { useModelProviderPick } from "./use-model-provider-pick";
import { RAIL_PROVIDER } from "../../wardyn/copy";
import { WhatToRunStep } from "./step-bodies";

export function NewRunScreen() {
  const navigate = useNavigate();
  const { workspaces, reload: reloadWorkspaces } = useWorkspaceList();
  // Visibility is not capability: the workspace list is NOT narrowed by the
  // `workspace` grant (the launch gate refuses, and a hidden workspace makes
  // that refusal unexplainable and the grant undiscoverable). Ungranted rows
  // are annotated instead.
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
  // replaces within a tick with the server's own default (the strongest
  // installed class), the moment it resolves. There is no per-browser
  // default left to seed this from (see default-confinement.ts).
  const [state, setState] = React.useState<WizardState>(() =>
    initialWizardState("CC1", prefill?.state),
  );
  // The policy this run ships, as the operator wrote it. `useSaved` is the mode
  // row: reuse a stored policy by REFERENCE (policy_id) or author one here.
  // A clone of a run that launched by reference opens in that mode — otherwise
  // the picker would hold the id while the panel showed an authored document
  // nobody wrote.
  const [useSaved, setUseSaved] = React.useState(!!prefill?.state.selectedPolicyId);
  // The DEFAULT body floors at CC1 — NOT Minimal's authored CC2. The
  // pre-panel screen was launchable by construction (its composed floor was
  // the selected tier); a hardcoded CC2 default would open every fresh
  // /runs/new on a Fence-only host fail-closed, all tiers dead, before the
  // operator authored anything. Clicking the Minimal CHIP afterwards is an
  // authored act and still floors CC2 — that corner stays, with its reason
  // line and preflight naming it.
  const [specText, setSpecText] = React.useState(() => defaultSpecText());
  // The floor the LAST SUCCESSFUL parse authored — deliberately sticky across a
  // broken edit: a half-typed document must not momentarily drop the floor and
  // re-open a barrier tier the operator's own policy forbids.
  const [parsedFloor, setParsedFloor] = React.useState<ConfinementClass | undefined>("CC1");
  // The form as the MACHINE left it — what `dirty` below compares against.
  // specText's baseline is fixed, but the barrier is the one field the machine
  // writes on its own (the health probe re-resolves it, the policy floor
  // up-clamps it), so its baseline moves with those writes. Comparing it to a
  // constant would call an untouched form dirty and break Esc entirely.
  const pristineSpec = React.useRef(specText);
  const pristineCc = React.useRef(state.confinementClass);
  // Whether the Barrier control carries an EXPLICIT pick (a clone's
  // carried-over class counts, B4b). Untouched, buildRunInput omits
  // confinement_class so the server's own default decides, and its audit
  // trail reads `defaulted` rather than `requested` (confinement_source).
  const [ccTouched, setCcTouched] = React.useState(!!prefill?.state.confinementClass);
  // The clone's barrier as this form was seeded with it, read once like the
  // useState seeds above: the top bar's New run navigates to /runs/new with no
  // state, which keeps this screen mounted and clears `prefill`; that must not
  // re-run the /setup/status effect below and half-reset the form.
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
  // #542/#922 — this person's own model providers (/setup/status, already
  // filtered to what they may use — capVisible(capModelProvider), #832/#1015)
  // and their connection state. Same "unknown stays unknown" rule as
  // harnesses above: undefined until the read lands, which is also what keeps
  // the rail's provider picker from rendering (and forcing a preselection)
  // before there is anything to pick from. It is also the one member-safe
  // signal for "is the chosen workspace's pinned model provider available to
  // me" (#922). Read off the SAME /setup/status fetch above, never a second one.
  const [modelProviders, setModelProviders] = React.useState<SetupModelProvider[] | undefined>(undefined);
  const [providerAccess, setProviderAccess] = React.useState<SetupProviderAccess[] | undefined>(undefined);
  // Existing run titles, offered as a native <datalist> under the Title input.
  // Grouping is by EXACT string, so without this the operator has to retype a
  // title character-perfect for a run to ever join its family — the feature
  // would look broken while working precisely as designed.
  const [knownTitles, setKnownTitles] = React.useState<string[]>([]);
  // #1197 L2: Title is optional now, and tracks the task's first line
  // (titleFromTask, wizard-types.ts) until the operator edits it themselves —
  // a clone's carried-over title counts as an edit too, so a non-empty
  // prefill is never silently overwritten by whatever the task says.
  // Clearing the field by hand also counts: re-arming would fight the
  // operator's own delete.
  const [titleUserEdited, setTitleUserEdited] = React.useState(!!prefill?.state.title);
  // The governance profile bounding THIS caller, named by GET
  // /policies/default. undefined for a caller with no assignment (the key is
  // omitted on the wire) and for a read that failed — in both cases the rail's
  // ceiling section simply does not render, which is the honest answer: never
  // claim a ceiling that could not be read.
  const [governanceProfile, setGovernanceProfile] = React.useState<string | undefined>(undefined);
  // #1200 — the SAME read's min_confinement_class: the governance ceiling's
  // own floor, which composer.Clamp raises the run to regardless of what the
  // authored policy sets (internal/composer/clamp.go). Undefined for the same
  // two reasons governanceProfile is: no assignment, or a read that failed —
  // in both cases the Barrier control falls back to the authored floor alone,
  // never a floor it could not confirm.
  const [govFloor, setGovFloor] = React.useState<ConfinementClass | undefined>(undefined);
  // #1200 review P2-6/R2-4 — Vault's driver-aware reason (the /dev/kvm probe
  // on docker, a Kata RuntimeClass on k8s), read off the same /setup/status
  // call, so the T-9 requirement card names the SAME honest reason
  // environment-step.tsx computes instead of a generic "isn't installed".
  const [vaultReason, setVaultReason] = React.useState<string | undefined>(undefined);
  // The Workspace card's drive block: this caller's allocation (nil-means-none)
  // and the door beside it ("" means open), read off the shell's ONE GET /me
  // rather than a second one of this screen's own — app-shell's useMeta already
  // holds that body and hands it down (operator-context.tsx's
  // MeIdentity.userDrive, the same seam member_local_dir_root rides). With no
  // provider above, on an older daemon, or after a failed read it is null/""
  // — which renders as today's card, the same honest answer the server's own
  // resolver gives.
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
  // operator writes their own. Keyed on state.task alone — patch() below is a
  // stable useCallback, so this never fires on an unrelated field's change.
  React.useEffect(() => {
    if (titleUserEdited) return;
    setState((s) => ({ ...s, title: titleFromTask(s.task) }));
  }, [state.task, titleUserEdited]);

  // ONE /setup/status read for everything this screen needs: model-access
  // readiness, the harness catalog, and which barriers this host can
  // build — runner.confinement_classes, the same field every other surface
  // reads, never a separately-polled mirror. `unreachable` already
  // distinguishes "couldn't check" from a real empty list, so there is no
  // retry-on-empty heuristic to reimplement.
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
        if (st.unreachable) return;
        setVaultReason(vaultRequirementReason(st.runner.driver, st.platform));
        const classes = (st.runner.confinement_classes ?? []).filter(Boolean);
        // No runner AT ALL (environment-step.tsx's own noDriver fold — a
        // member's redacted Driver:"" WITH classes is a withheld NAME, not
        // no-driver) is UNKNOWN here, not "nothing installed": the capability
        // gate (runs_create.go) is skipped entirely with no runner configured.
        const noDriver = st.runner.driver === "none" || (st.runner.driver === "" && classes.length === 0);
        if (noDriver) return;
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
    policiesApi
      .getDefaultPolicy()
      .then((p) => {
        setGovernanceProfile(p.governance_profile_name);
        const f = p.min_confinement_class;
        setGovFloor(f && (ORDERED_CLASSES as string[]).includes(f) ? (f as ConfinementClass) : undefined);
      })
      .catch(() => {
        /* unknown stays unknown — the rail names no ceiling it could not read */
      });
  }, []);

  // useWorkspaceList does NOT fetch on mount — every caller loads it itself
  // (setup-screen.tsx does the same in its own mount effect). Without this the
  // Workspace select only ever offers "Ephemeral scratch", so a workspace
  // onboarded anywhere else — Getting started, the Workspaces screen — could
  // not be attached to a run at all; the only way into the list was to add one
  // from this page in this session.
  React.useEffect(() => {
    reloadWorkspaces();
    // eslint-disable-next-line react-hooks/exhaustive-deps -- run once on mount; reload is stable (useCallback([]))
  }, []);

  // The envelope-field detach funnel retired with the controls it guarded:
  // four of its five fields were the Network card's, and the fifth —
  // confinementClass — is now guarded by the floor-DISABLE below instead, which
  // is strictly stronger. A one-time up-clamp alone would re-open the
  // below-floor 422 (runs_create.go's floor check, on both the policy_id and
  // inline paths) the moment the operator lowered the Seg afterwards. Detach
  // now has exactly one trigger: editing the spec text (see onSpecChange).
  const patch = React.useCallback((p: Partial<WizardState>) => setState((s) => ({ ...s, ...p })), []);

  const isAgent = state.runType === "agent";
  const cc = state.confinementClass;
  const parsed = parseSpec(specText);
  // A shell command is unattended by definition — buildSpec forces batch for
  // one, so the Run mode segment is hidden rather than offering a combination
  // that would silently drop the command.
  const isInteractive = isAgent && state.mode === "interactive";
  // Shared display name (wizard-types.agentLabel) — a local re-hardcode here
  // is exactly the drift that helper's doc says it exists to prevent.
  const agentName = agentLabel(state.agent);

  // #542 — this agent's own model-provider candidates (already access-filtered
  // per person by the server — #1015; the harness/disabled narrowing is the
  // console's own). `undefined` modelProviders (unfetched, or an unreachable
  // /setup/status) means "unknown" — no picker, not a false "no provider
  // serves this agent" (R9's shape, which is for a REAL empty answer).
  const providerCandidates = isAgent && modelProviders ? candidatesForAgent(modelProviders, state.agent) : [];

  // F2 (#612) — the primary workspace's own pinned provider: its
  // llm_cred.provider_ref (internal/types.WorkspaceLLMCred), the SAME "pin"
  // the server falls back to when nothing was explicitly requested
  // (cmp.Or(requested, pin), internal/api/run_model_provider.go). Read the
  // primary the same way the server does — wizard-types.ts's
  // primaryWorkspaceId — so this rail can never pin a different workspace's
  // credential than the run actually inherits.
  const primaryWsId = primaryWorkspaceId(state.workspaces, workspaces);
  const pin = workspaces.find((w) => w.id === primaryWsId)?.llm_cred?.provider_ref;

  // #542 rail-gap packet (owner-approved 2026-09-25) — R5b/R5c: undefined for
  // the ordinary R1-R4/R6-R8 shapes and for R9 (model-provider-lane.ts's
  // providerGate).
  const providerGateState = isAgent && modelProviders ? providerGate(modelProviders, state.agent) : undefined;

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

  const selectedPolicy =
    useSaved && state.selectedPolicyId
      ? savedPolicies.find((p) => p.id === state.selectedPolicyId)
      : undefined;
  // The screen's ONE validation rule. Deliberately a local derivation rather
  // than a shared validator: it answers "can this button be pressed", which is
  // this screen's question, and a second general-purpose answer living
  // elsewhere is what drifts out of sync with the form it describes.
  const needsTask = !isAgent || state.mode === "batch";
  // #922: the CHOSEN workspace, resolved the same way workspace-card.tsx's own
  // per-reason advisory lines resolve it (state.workspaces[0] is the primary
  // selection) — folded into ONE generic reason via workspaceUnavailableToCaller,
  // never the picker's own more specific copy (that stays put, unchanged).
  //
  // review F2: the model-provider arm is gated on `isAgent` — a Shell/exec run
  // sends no `agent`, and the server's own model-provider door only ever asks
  // for a model run (run_model_provider.go's `needsModel`/`createDoorIsModelRun`,
  // runs_dispatch_llm.go's `taskMode != "exec"`); applying it to every run type
  // was a false-disable for a command the server would happily admit. The
  // WORKSPACE capability arm is NOT gated — a plain ungranted workspace refuses
  // regardless of run type.
  const pickedWorkspace = workspaces.find((w) => w.id === state.workspaces[0]?.workspaceId);
  const workspaceUnavailable =
    !!pickedWorkspace &&
    workspaceUnavailableToCaller(pickedWorkspace, caps, isAgent ? modelProviders : undefined);
  // #1197 L2: Title dropped out of this chain — the server never required
  // one (runs_create_validate.go's own doc comment), only the console did,
  // and the console default now derives one from the task instead of asking.
  //
  // review F5: `workspaceUnavailable` is NOT a clause here — it disables
  // Launch through the rail's own `workspaceUnavailable` prop instead (below),
  // so the sentence renders exactly once, on the workspace picker's own
  // advisory line (workspace-card.tsx), never a second time in the rail's
  // problem slot.
  const problem = needsTask && !state.task.trim()
    ? isAgent
      ? "An autonomous run needs a task to perform."
      : "Enter a command to run."
    : // A Custom policy that doesn't parse has nothing to send. The saved
      // lane launches by reference, so its body is never on the wire.
      !useSaved && !parsed.ok
      ? "The policy spec isn't valid JSON."
      : savedPolicyGone(useSaved, state.selectedPolicyId, selectedPolicy, policiesLoaded) // F2-F5
        ? RUN.POLICY_GONE
        : useSaved && !state.selectedPolicyId
          ? "Pick a saved policy, or write a custom one."
          : // R5c (#542 rail-gap packet) — the admin's own default is
            // disabled. Named regardless of how many other candidates
            // remain, until an explicit pick lands (same "silent once
            // chosen" rule as R7's changeNote). R5b ("granted none") is not
            // drawn — see providerGate's own doc comment (Opus review
            // round 2): the console has no signal for it.
            providerGateState?.kind === "default_off" && !state.modelProviderId
            ? providerCandidates.length > 0
              ? RAIL_PROVIDER.DEFAULT_OFF(
                  providerGateState.provider.name ?? providerGateState.provider.id,
                  agentName,
                )
              : RAIL_PROVIDER.DEFAULT_OFF_ONLY(
                  providerGateState.provider.name ?? providerGateState.provider.id,
                  agentName,
                )
            : // R6 (QC-4): several candidates, none granted as this agent's
              // default (or the default isn't one of them) — Wardyn never
              // silently substitutes, so Launch waits for an explicit pick.
              // Rule (3): a workspace pin already answers "why wait" its own
              // way (the server's own named refusal on launch), so this
              // generic hint stays silent whenever one is set.
              providerCandidates.length > 1 && !state.modelProviderId && !pin
              ? RAIL_PROVIDER.LAUNCH_HINT
              : null;

  // Every successful parse re-reads the floor the document authors; a FAILED
  // parse changes nothing (parsedFloor stays whatever last parsed).
  React.useEffect(() => {
    const p = parseSpec(specText);
    if (!p.ok) return;
    const f = p.spec.min_confinement_class as ConfinementClass;
    setParsedFloor(ORDERED_CLASSES.includes(f) ? f : undefined);
  }, [specText]);

  // C5's one real trap (policy-panel.tsx's own doc) — the field is present and
  // this build can't spell it.
  const unparseableFloor = unparseableFloorClass(parsed);

  // The ACTIVE floor: a picked saved policy's stored floor, else the last
  // successful parse's. Both paths refuse to launch below it server-side.
  const floor = useSaved ? (selectedPolicy?.spec.min_confinement_class as ConfinementClass | undefined) : parsedFloor;

  // #1200 review P2-2 — govFloor binds ONLY where the server would actually
  // clamp to it, mirrored exactly from the two doors that decide that:
  //   - an OPERATOR is never clamped at all (effectiveCeiling's own
  //     short-circuit, internal/api/governance.go; an admin's inline policy
  //     specifically, inline_policy.go's "admin ⇒ no clamp");
  //   - the INLINE (custom) lane clamps every non-operator unconditionally —
  //     even unassigned, since the deployment default IS the ceiling then;
  //   - the SAVED-POLICY lane clamps only when a NAMED profile is assigned
  //     (governanceProfile present) — an unassigned member's saved policy is
  //     not raised to the deployment default at all (inline_policy.go:310's
  //     `ceiling.Profile != nil`).
  // Folding it unconditionally (the pre-review build) hid tiers the server
  // would have let an admin, or an unassigned member's saved policy, use.
  const govFloorApplies = !operator && (!useSaved || !!governanceProfile);
  const boundGovFloor = govFloorApplies ? govFloor : undefined;

  // #1200 — the floor that actually binds: whichever of the authored
  // policy's own floor and the (now correctly gated) governance ceiling's
  // ranks HIGHER (combineFloors — composer.Clamp raises a weaker authored
  // floor to the ceiling's, never the other way). A Barrier control that
  // only read `floor` could offer a tier the server then 422s at launch.
  const effectiveFloor = combineFloors(floor, boundGovFloor);

  // #1200 review P2-1/R2-1 — whether the GOVERNANCE ceiling actually removed
  // an installed tier, as opposed to this host simply having one tier, or the
  // run's OWN authored floor doing the narrowing (governanceRemovedTier).
  // Only this case gets TierPicker's "set by your admin" line and the
  // governance-sourced requirement wording.
  const governanceBinding = governanceRemovedTier(availableClasses, floor, boundGovFloor);

  // The Barrier control's per-tier state — see barrierReasons.
  const { qualifying, unavailable, belowFloor } = barrierReasons(availableClasses, effectiveFloor);

  // UP-CLAMP the Barrier Seg to the active floor. `cc` is in the deps on
  // purpose: the /setup/status read resolves ASYNCHRONOUSLY and re-seeds
  // confinementClass from the server's own default, which can land BELOW a
  // floor this already clamped to. Watching the value, not just the floor,
  // makes "never below the floor" an invariant instead of a one-shot.
  React.useEffect(() => {
    if (!effectiveFloor || !ORDERED_CLASSES.includes(effectiveFloor)) return;
    if (rank(effectiveFloor) > rank(cc)) {
      pristineCc.current = effectiveFloor;
      patch({ confinementClass: effectiveFloor });
    }
  }, [effectiveFloor, cc, patch]);

  // The post-parse union, computed ONCE: the same value renders the "Added for
  // this run's selections" line and goes on the wire, so the screen cannot show
  // one policy and launch another.
  const merged = React.useMemo(
    () => (parsed.ok ? mergeRunSelections(parsed.spec, state, workspaces) : null),
    // eslint-disable-next-line react-hooks/exhaustive-deps -- parsed is rebuilt every render; specText is what actually changes
    [specText, state, workspaces],
  );
  const added = merged?.added;

  // Launch + preflight state and actions — see use-launch.ts's header for why
  // this lane is a hook rather than a pure function like policy-lane.ts's.
  const adoDoor = useAdoLaunchDoor(); // #386's launch door — F8: never relaunches
  const {
    launching,
    launchDisabled,
    launchSpinning,
    error,
    errorSeq,
    credentialRefused,
    refusedProvider,
    launch,
    preflighting,
    preflightResult,
    preflightError,
    preflightErrorSeq,
    preflightIsCurrent,
    preflight,
  } = useLaunch({ state, workspaces, useSaved, ccTouched, merged, onLaunchError: adoDoor.notifyLaunchError });

  // What happens the moment this launches, in one sentence. Derived HERE and
  // handed to the rail, so the rail cannot describe one run while Launch sends
  // another.
  const startupLine = isInteractive
    ? state.task.trim()
      ? state.interactiveStart === "agent"
        ? `Starts ${agentName} on your prompt at boot — attach to watch and take over.`
        : "Runs your startup command at boot, then a terminal is ready."
      : state.interactiveStart === "agent"
        ? `Comes up idle with the workspace ready. Attaching starts ${agentName} in it.`
        : "Comes up idle with the workspace ready. Attaching drops you into a terminal."
    : isAgent
      ? `${agentName} runs the task unattended, then the run stops.`
      : "The command runs unattended in the sandbox, then the run stops.";

  // What this run's tool_rules actually say, from the SAME spec that ships:
  // the merged document on the custom lane, the stored one on the saved lane.
  // Null when there are no rules, so a policy written before the field existed
  // grows no empty rail section.
  const specForRules = useSaved ? selectedPolicy?.spec : merged?.spec;
  const toolRules = React.useMemo(() => (specForRules ? toolRulesSummary(specForRules) : null), [specForRules]);
  const hasAdditions = !!added && (added.hosts.length > 0 || added.grants.length > 0 || added.mounts.length > 0 || added.repos.length > 0);
  // #181 — same source as toolRules above; a shell command has no
  // specForRules at all (RunRail's pushRulesIsSet reads undefined as "no
  // section"). `unattended` mirrors isInteractive's own doc: a shell command
  // is unattended by definition, and so is an agent run left on batch mode.
  const pushRules = specForRules?.push_rules;
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
    useSaved ||
    specText !== pristineSpec.current ||
    JSON.stringify(state) !== JSON.stringify(initialWizardState(pristineCc.current));
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

  return (
    <div className="mx-auto w-full max-w-[1200px] px-6 py-6">
      <div className="mb-6 flex items-center gap-3">
        <Button variant="ghost" size="sm" onClick={() => navigate("/runs")}>
          <ArrowLeft className="size-4" /> Runs
        </Button>
        <h1 className="text-foreground">New run</h1>
      </div>

      {/* B4b — a clone says what it carried and, in the same breath, what it
          could not. A prefilled form that looks hand-typed is the failure mode:
          the operator would have no way to know the tool-approval posture came
          across but the credentials deliberately did not. */}
      {/* Review F4: a composer launch (runs/runs-composer.tsx) has no source
          run to be "prefilled from" — its own RunPrefill carries
          source:"composer", which skips this banner entirely. */}
      {prefill && prefill.source !== "composer" && (
        <div
          role="status"
          className="mb-6 space-y-1 rounded-lg border border-border bg-muted/40 px-3 py-2.5 text-xs leading-relaxed text-muted-foreground"
        >
          <p>{RUN.CLONE_NOTE}</p>
          {/* …and that carrying a setting over is not the same as being allowed
              it. Create re-clamps against the caller's governance ceiling, so a
              member cloning an admin's run is narrowed at launch — said here,
              before Launch, rather than as a warning after it. */}
          <p>{RUN.CLONE_CEILING_NOTE}</p>
          {/* The one named ceiling: an inline policy is never stored, so there
              is nothing to prefill and the barrier below is this wizard's
              default rather than the original run's document. */}
          {prefill.inlinePolicy && <p className="text-warning">{RUN.CLONE_INLINE_POLICY_CEILING}</p>}
        </div>
      )}

      <div className="grid gap-4 lg:grid-cols-[minmax(0,1fr)_20rem]">
        {/* Left: the form */}
        <div className="min-w-0 space-y-4">
          {/* Identity first: the one thing that makes this run findable a week
              from now. Title is optional (#1197 L2) — it defaults from the
              task's own first line and stays editable. */}
          <SectionCard title="This run">
            <div className="space-y-4">
              <Field label="Title" htmlFor="nr-title">
                <Input
                  id="nr-title"
                  // Rulebook §8: default focus lands on the primary field, not
                  // on the back-out button or the first select.
                  autoFocus
                  // NO Enter-to-launch here. Rulebook §8 allows it from a
                  // single-line input, but this is the input carrying the
                  // datalist below: choosing a suggestion with Enter dispatches
                  // keydown Enter on the input (Chrome), so picking a known
                  // title off the list would LAUNCH the run. Completion and
                  // commit cannot share a key. Launch is the rail's button.
                  maxLength={200}
                  // Native datalist: existing titles are offered as you type, so
                  // joining a family is a pick rather than an exact retype. No
                  // combobox library, and typing something new still just works.
                  list="nr-known-titles"
                  placeholder="Refactor the payments module"
                  value={state.title}
                  // #1197 L2: any edit — including clearing it — turns off the
                  // task-derived default for the rest of this session.
                  onChange={(e) => {
                    setTitleUserEdited(true);
                    patch({ title: e.target.value });
                  }}
                />
                <datalist id="nr-known-titles">
                  {knownTitles.map((t) => (
                    <option key={t} value={t} />
                  ))}
                </datalist>
              </Field>

              <Field
                label="Description"
                htmlFor="nr-description"
                hint="Optional. Why this run exists — for whoever reads it later."
              >
                <Textarea
                  id="nr-description"
                  rows={2}
                  maxLength={2000}
                  placeholder="Ticket 4412 — the refund path double-charges on retry."
                  value={state.description}
                  onChange={(e) => patch({ description: e.target.value })}
                />
              </Field>
            </div>
          </SectionCard>

          <WhatToRunStep
            state={state}
            patch={patch}
            isAgent={isAgent}
            isInteractive={isInteractive}
            agentName={agentName}
            harnesses={harnesses}
          />

          {/* The Select, the ungranted-selection reason and the member's own
              drive block — see workspace-card.tsx for why the drive lives
              under the Select rather than in the Add-workspace dialog. */}
          <WorkspaceCard
            state={state}
            patch={patch}
            workspaces={workspaces}
            caps={caps}
            modelProviders={modelProviders}
            onAddWorkspace={() => setAddWsOpen(true)}
            drive={userDrive}
            driveDeniedBy={driveDeniedBy}
            driveUnavailable={driveUnavailable}
          />

          <SectionCard title="Policy">
            <div className="space-y-4">
              <PolicyPanel
                instance="run"
                value={specText}
                onChange={onSpecChange}
                onPreflight={preflight}
                preflightBusy={preflighting}
                preflightDisabled={useSaved && !state.selectedPolicyId}
                interactive={isInteractive}
                savedPolicy={{
                  active: useSaved,
                  onActiveChange: (v: boolean) => { 
                    const c = clearedSpecOnCustomSwitch(v, securityOperator && operatorResolved, !!state.selectedPolicyId);
                    if (c) setSpecText(c);
                    setUseSaved(v);
                  },
                  picker: (
                    <div className="space-y-2">
                      <Select value={state.selectedPolicyId ?? ""} onValueChange={onPickPolicy}>
                        <SelectTrigger aria-label="Saved policy">
                          <SelectValue placeholder="Pick a policy" />
                        </SelectTrigger>
                        <SelectContent>
                          {savedPolicies.map((p) => (
                            <SelectItem key={p.id} value={p.id}>
                              {p.name}
                            </SelectItem>
                          ))}
                        </SelectContent>
                      </Select>
                      {/* Rulebook §9: an empty picker carries the action that
                          fills it. With no stored policies this lane was a
                          dropdown with nothing in it and no way out. */}
                      {/* M-1b: Policies is Admin view only (/admin/policies),
                          so the door renders only for the tier that authors
                          them. */}
                      {savedPolicies.length === 0 && (
                        <p className="text-xs text-muted-foreground">
                          No saved policies yet
                          {operator && (
                            <>
                              {" "}·{" "}
                              <Link to="/admin/policies" className="font-medium text-info hover:underline">
                                New policy →
                              </Link>
                            </>
                          )}
                        </p>
                      )}
                    </div>
                  ),
                }}
              />

              {/* C5: named, not left to the barrier above silently winning. */}
              {!useSaved && unparseableFloor && (
                <p className="text-xs text-warning">{AGENTS.FLOOR_UNPARSEABLE(unparseableFloor)}</p>
              )}

              {/* What buildSpec unions in AFTER the parse, named out loud. A
                  policy the operator did not write is one they cannot be held
                  to — and these are exactly the entries the document itself
                  cannot know: the Workspace card's attachments, the grant
                  lanes' grants, and the hosts those grants must reach (an
                  api_key grant whose host is not on the allowlist authenticates
                  nothing, allow_all_egress included). Saved-policy runs launch
                  by REFERENCE, so nothing is merged into a stored spec. */}
              {!useSaved && hasAdditions && added && (
                <div
                  className="rounded-lg border border-border bg-surface-2 p-3"
                  data-testid="run-spec-additions"
                >
                  <p className="text-xs text-muted-foreground">
                    Added for this run&apos;s selections:
                  </p>
                  <div className="mt-1.5 space-y-1">
                    {added.hosts.map((h) => (
                      <div key={h} className="flex flex-wrap items-center gap-1.5">
                        <Mono className="text-xs text-foreground">{h}</Mono>
                        <Chip tone="neutral">allowed_domains</Chip>
                      </div>
                    ))}
                    {added.grants.map((g, i) => (
                      <div key={`${g.kind}-${i}`} className="flex flex-wrap items-center gap-1.5">
                        <Mono className="text-xs text-foreground">{String(g.kind)}</Mono>
                        <Chip tone="neutral">eligible_grants</Chip>
                      </div>
                    ))}
                    {added.mounts.map((m) => (
                      <div key={m.target} className="flex flex-wrap items-center gap-1.5">
                        <Mono className="text-xs text-foreground">{m.target}</Mono>
                        <Chip tone="neutral">workspace_mounts</Chip>
                      </div>
                    ))}
                    {added.repos.map((r) => (
                      <div key={r.repo} className="flex flex-wrap items-center gap-1.5">
                        <Mono className="text-xs text-foreground">{r.repo}</Mono>
                        <Chip tone="neutral">workspace_repos</Chip>
                      </div>
                    ))}
                  </div>
                </div>
              )}

              {/* #1200 — the shared TierPicker: only what THIS run can
                  actually use (installed ∧ at-or-above the active floor,
                  which folds in the governance ceiling ONLY where the server
                  would clamp to it — see govFloorApplies). A tier the floor
                  forbids or the host can't build is DROPPED, never shown
                  disabled (the global rule every user-facing picker now
                  follows); the ONE qualifying case collapses to TierPicker's
                  own decided row, and NONE qualifying shows the T-9
                  requirement card instead of a fully-disabled Seg. Review
                  P2-1/P2-3: decidedLine/pickOneNote override TierPicker's
                  defaults, which both assume a governance floor and a
                  browser-persisted pick — neither true of this screen's
                  non-governance decided case or its per-run choice. */}
              <div className="border-t border-border pt-3">
                <div className="text-sm font-medium text-foreground">Barrier</div>
                <TierPicker
                  className="mt-2"
                  // P2-7: an UNKNOWN probe (qualifying: null) must not offer
                  // a tier the active floor already forbids — it falls back
                  // to the floor's own allowed set, not the unfiltered
                  // ORDERED_CLASSES, and only to that when there is no floor
                  // either.
                  tiers={qualifying ?? allowedFromFloor(effectiveFloor) ?? ORDERED_CLASSES}
                  selected={cc}
                  onSelect={(id) => {
                    setCcTouched(true);
                    patch({ confinementClass: id });
                  }}
                  decidedLine={governanceBinding ? undefined : () => RUN.BARRIER_ONLY_QUALIFIER}
                  pickOneNote={TIER_PICKER.PICK_ONE_PER_RUN}
                  requirementNote={
                    qualifying && qualifying.length === 0 && effectiveFloor
                      ? (governanceBinding ? TIER_PICKER.GOVERNANCE_REQUIREMENT_LINE : TIER_PICKER.REQUIREMENT_LINE)(
                          CC_META[effectiveFloor].label,
                          barrierRequirementReason(
                            effectiveFloor,
                            unavailable,
                            belowFloor,
                            vaultReason,
                          ),
                        )
                      : undefined
                  }
                />
                {/* Unknown never blocks launch: an untouched pick sends no
                    confinement_class (ccTouched), so the server decides. */}
                {probeSettled && !availableClasses && (
                  <p className="mt-2 text-xs text-muted-foreground">{RUN.BARRIER_UNKNOWN}</p>
                )}
              </div>
            </div>
          </SectionCard>
        </div>

        {/* Right: the live rail, a fixed 320px */}
        <RunRail
          governanceProfile={governanceProfile}
          savedPolicy={selectedPolicy}
          cc={cc}
          showModelWarning={isAgent && llmReady === false}
          startup={startupLine}
          // The server derives a hold in the OPPOSITE case from what this used
          // to check: autonomyDerive (runs_autonomy.go) sets tool_approvals=
          // hold when the run is non-interactive, the agent has a hold lane
          // (claude-code, here) and the request did NOT already ask for hold.
          // Picking hold yourself derives nothing to announce.
          showHoldNote={
            !isInteractive && isAgent && state.agent === "claude-code" && state.toolApprovals !== "hold"
          }
          toolRules={toolRules}
          pushRules={pushRules}
          unattended={unattended}
          launch={{
            onLaunch: launch,
            disabled: launchDisabled,
            spinning: launchSpinning,
            inFlight: launching,
            problem,
            // review F5: disables Launch WITHOUT a second rendering of the
            // sentence — workspace-card.tsx's own advisory line is the one
            // place it's shown.
            workspaceUnavailable,
            error,
            errorSeq,
            credentialRefused,
            refusedProvider,
          }}
          preflight={
            preflightIsCurrent
              ? { error: preflightError, errorSeq: preflightErrorSeq, result: preflightResult }
              : { error: null, errorSeq: preflightErrorSeq, result: null }
          }
          agentRow={isAgent ? harnesses?.find((h) => h.id === state.agent) : undefined}
          modelProvider={
            isAgent
              ? {
                  candidates: providerCandidates,
                  access: providerAccess,
                  selectedId: state.modelProviderId,
                  onChange: onModelProviderChange,
                  changeNote: providerChangeNote,
                  gate: providerGateState,
                  harnessLabel: agentName,
                }
              : undefined
          }
          adoDialog={adoDoor.dialog}
        />
      </div>

      {addWsOpen && (
        <AddWorkspaceDialog
          existingNames={workspaces.map((w: Workspace) => w.name)}
          onClose={() => setAddWsOpen(false)}
          onCreated={() => {
            setAddWsOpen(false);
            reloadWorkspaces();
            toast.success("Workspace added");
          }}
        />
      )}
    </div>
  );
}
