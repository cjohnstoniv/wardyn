/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New run — ONE page, two columns, with a live rail that answers "what can this
// run actually do?" while you build it. Replaces a five-step modal wizard whose
// Review screen was the first place the consequences of your choices appeared.
//
// A RE-LAYOUT, not a re-model: buildSpec()/impliedEgressHosts() are reused
// verbatim, so the launch payload is exactly what the wizard produced — the
// governance contract is the tested part; this is about when the operator SEES
// it, not what it is.
//
// The Confinement and Network cards are GONE — `inline_policy` on POST /runs is
// the identical struct a saved policy stores, validated by the same validator,
// so this screen and /policies now author it through ONE component
// (wardyn/policy-panel.tsx). What the JSON
// cannot know (the Workspace card's mounts/repos, the grant lanes' grants and
// hosts) is unioned back in by mergeRunSelections and NAMED on screen, never
// merged behind the operator's back.
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
import { SectionCard } from "./new-run-primitives";
import { policies as policiesApi } from "../../../lib/api/policies";
import { runs as runsApi } from "../../../lib/api/runs";
import { setup as setupApi } from "../../../lib/api/setup";
import { ADOAccessSummary } from "../../wardyn/ado-access-summary";
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
import { PolicyPanel } from "../../wardyn/policy-panel";
import { TierPicker, allowedFromFloor } from "../../wardyn/tier-picker";
import { vaultRequirementReason } from "../setup/environment-step";
import { TIER_PICKER } from "../../../lib/tier-picker-copy";
import { AddWorkspaceDialog } from "../add-workspace-dialog";
import { WorkspaceCard } from "./workspace-card";
import {
  barrierRequirementReason,
  clearedSpecOnCustomSwitch,
  defaultSpecText,
} from "./policy-lane";
import {
  agentLabel,
  initialWizardState,
  primaryWorkspaceId,
  resolvedModelProviders,
  titleFromTask,
  type RunPrefill,
  type WizardState,
} from "./wizard-types";
import { useLaunch } from "./use-launch";
import { providerCandidates as candidatesForAgent, providerGate } from "./model-provider-lane";
import { useModelProviderPick } from "./use-model-provider-pick";
import { WhatToRunStep } from "./step-bodies";
import { useNewRunPolicy } from "./use-new-run-policy";
import { NewRunLaunchPanel } from "./new-run-launch-panel";

export function NewRunScreen() {
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
  // `useSaved` is the mode row: reuse a stored policy by REFERENCE (policy_id)
  // or author one here. A clone of a run that launched by reference opens in
  // that mode — otherwise the picker would hold the id while the panel showed
  // an authored document nobody wrote.
  const [useSaved, setUseSaved] = React.useState(!!prefill?.state.selectedPolicyId);
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
  // Existing run titles, offered as a native <datalist> — grouping is by
  // EXACT string, so a family needs a character-perfect retype without it.
  const [knownTitles, setKnownTitles] = React.useState<string[]>([]);
  // #1197 L2: Title tracks the task's first line (titleFromTask) until the
  // operator edits it themselves — a clone's carried-over title, or clearing
  // the field by hand, both count as an edit and must not be fought.
  const [titleUserEdited, setTitleUserEdited] = React.useState(!!prefill?.state.title);
  // The governance profile bounding THIS caller (GET /policies/default).
  // Undefined for no assignment or a failed read — either way the rail's
  // ceiling section simply does not render, never claiming a ceiling it
  // could not confirm.
  const [governanceProfile, setGovernanceProfile] = React.useState<string | undefined>(undefined);
  // #1200 — the SAME read's min_confinement_class, the governance ceiling's
  // own floor (composer.Clamp raises the run to it, internal/composer/clamp.go).
  // Undefined for the same two reasons governanceProfile is; the Barrier
  // control then falls back to the authored floor alone.
  const [govFloor, setGovFloor] = React.useState<ConfinementClass | undefined>(undefined);
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
    useSaved,
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
    pristineCc,
  });

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
  const pin = workspaces.find((w) => w.id === primaryWsId)?.llm_cred?.provider_ref;

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

  // Launch + preflight state and actions — see use-launch.ts's header for why
  // this lane is a hook rather than a pure function like policy-lane.ts's.
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
  } = useLaunch({
    state,
    workspaces,
    useSaved,
    ccTouched,
    merged: policy.merged,
    onLaunchError: policy.adoDoor.notifyLaunchError,
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

      {/* B4b — a clone says what it carried and what it could not (the
          tool-approval posture came across, the credentials deliberately did
          not). A prefilled form that looks hand-typed is the failure mode: the
          operator would have no way to know. Review F4: a composer launch's
          RunPrefill carries source:"composer", skipping this banner. */}
      {prefill && prefill.source !== "composer" && (
        <div
          role="status"
          className="mb-6 space-y-1 rounded-lg border border-border bg-muted/40 px-3 py-2.5 text-xs leading-relaxed text-muted-foreground"
        >
          <p>{RUN.CLONE_NOTE}</p>
          {/* Create re-clamps against the caller's governance ceiling, so a
              member cloning an admin's run is narrowed at launch — said here,
              before Launch, rather than as a warning after it. */}
          <p>{RUN.CLONE_CEILING_NOTE}</p>
          {/* An inline policy is never stored, so the barrier below is this
              wizard's default, not the original run's document. */}
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
                  // Rulebook §8: default focus lands on the primary field.
                  autoFocus
                  // NO Enter-to-launch: this input carries the datalist below,
                  // and Chrome dispatches keydown Enter when a suggestion is
                  // picked, which would LAUNCH the run. Launch is the rail's
                  // button only.
                  maxLength={200}
                  // Native datalist: existing titles are offered as you type,
                  // so joining a family is a pick, not an exact retype. No combobox
                  // library, and typing something new still just works.
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

          {/* #214 — the Barrier control OUT of the Policy card and into its
              own section: it decides whether a run is confined at all, and
              it was the hardest thing on the screen to find. */}
          <SectionCard title="Barrier">
            {/* #1200 — the shared TierPicker: only what THIS run can actually
                use (installed ∧ at-or-above the active floor, folding in the
                governance ceiling only where the server would clamp to it —
                govFloorApplies). A tier the floor forbids or the host can't
                build is DROPPED, never shown disabled (the global picker
                rule); ONE qualifying tier collapses to the decided row, NONE
                shows the T-9 requirement card. requirementNote below names
                the active floor whenever one is set — every fresh form has
                one, the default spec's CC1 — so TierPicker's own
                REQUIREMENT_TITLE shows only when no floor is authored; the
                no-runner (#214) reason is the rail's noBarrier line.
                Review P2-1/P2-3: decidedLine/pickOneNote override
                TierPicker's defaults, which assume a governance floor and a
                browser-persisted pick — neither true here. */}
            <TierPicker
              // P2-7: an UNKNOWN probe (qualifying: null) must not offer
              // a tier the active floor already forbids — it falls back
              // to the floor's own allowed set, not the unfiltered
              // ORDERED_CLASSES, and only to that when there is no floor
              // either.
              tiers={policy.qualifying ?? allowedFromFloor(policy.effectiveFloor) ?? ORDERED_CLASSES}
              selected={cc}
              onSelect={(id) => {
                setCcTouched(true);
                patch({ confinementClass: id });
              }}
              decidedLine={policy.governanceBinding ? undefined : () => RUN.BARRIER_ONLY_QUALIFIER}
              pickOneNote={TIER_PICKER.PICK_ONE_PER_RUN}
              unprobed={!availableClasses}
              requirementNote={
                // A host with no barrier at all (noBarrierOnHost) is not "the
                // floor needs a tier this host lacks": every fresh form carries
                // the default CC1 floor, so that line would name Fence as the
                // missing piece. The picker's own no-runner title stands, and
                // the rail beside Launch gives the reason.
                !policy.noBarrierOnHost && policy.qualifying && policy.qualifying.length === 0 && policy.effectiveFloor
                  ? (policy.governanceBinding ? TIER_PICKER.GOVERNANCE_REQUIREMENT_LINE : TIER_PICKER.REQUIREMENT_LINE)(
                      CC_META[policy.effectiveFloor].label,
                      barrierRequirementReason(policy.effectiveFloor, policy.unavailable, policy.belowFloor, vaultReason),
                    )
                  : undefined
              }
            />
            {/* Unknown never blocks launch: an untouched pick sends no
                confinement_class (ccTouched), so the server decides. */}
            {probeSettled && !availableClasses && (
              <p className="mt-2 text-xs text-muted-foreground">{RUN.BARRIER_UNKNOWN}</p>
            )}
          </SectionCard>

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
                adoCeiling={adoCeiling}
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
                      <ADOAccessSummary caps={savedPolicies.find((p) => p.id === state.selectedPolicyId)?.spec.azure_devops_capabilities} />
                      {/* Rulebook §9: an empty picker carries the action that
                          fills it. M-1b: Policies is Admin view only, so the
                          door renders only for the tier that authors them. */}
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
              {!useSaved && policy.unparseableFloor && (
                <p className="text-xs text-warning">{AGENTS.FLOOR_UNPARSEABLE(policy.unparseableFloor)}</p>
              )}

              {/* What buildSpec unions in AFTER the parse, named out loud. A
                  policy the operator did not write is one they cannot be held
                  to — and these are exactly the entries the document itself
                  cannot know: the Workspace card's attachments, the grant
                  lanes' grants, and the hosts those grants must reach (an
                  api_key grant whose host is not on the allowlist
                  authenticates nothing, allow_all_egress included). Saved-policy runs launch by REFERENCE, so nothing
                  is merged into a stored spec. */}
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
            </div>
          </SectionCard>
        </div>

        {/* Right: the live rail, a fixed 320px. startup, workspaceUnavailable and
            problem are derived inside the panel from the same raw fields Launch
            reads, so the rail cannot describe or gate one run while Launch
            sends another. */}
        <NewRunLaunchPanel
          governanceProfile={governanceProfile}
          savedPolicy={policy.selectedPolicy}
          cc={cc}
          showModelWarning={isAgent && llmReady === false}
          isInteractive={isInteractive}
          interactiveStart={state.interactiveStart}
          mode={state.mode}
          agent={state.agent}
          toolApprovals={state.toolApprovals}
          toolRules={policy.toolRules}
          pushRules={pushRules}
          unattended={unattended}
          onLaunch={launch}
          launchDisabled={launchDisabled}
          launchSpinning={launchSpinning}
          launching={launching}
          task={state.task}
          useSaved={useSaved}
          specParsedOk={policy.parsed.ok}
          selectedPolicyId={state.selectedPolicyId}
          policiesLoaded={policiesLoaded}
          pin={pin}
          workspaces={workspaces}
          selectedWorkspaceId={state.workspaces[0]?.workspaceId}
          caps={caps}
          modelProviders={modelProviders}
          noBarrier={policy.noBarrierOnHost}
          error={error}
          errorSeq={errorSeq}
          credentialRefused={credentialRefused}
          refusedProvider={refusedProvider}
          preflightIsCurrent={preflightIsCurrent}
          preflightError={preflightError}
          preflightErrorSeq={preflightErrorSeq}
          preflightResult={preflightResult}
          agentRow={isAgent ? harnesses?.find((h) => h.id === state.agent) : undefined}
          isAgent={isAgent}
          providerCandidates={providerCandidates}
          providerAccess={providerAccess}
          selectedModelProviderId={state.modelProviderId}
          onModelProviderChange={onModelProviderChange}
          providerChangeNote={providerChangeNote}
          providerGateState={providerGateState}
          agentName={agentName}
          adoDialog={policy.adoDoor.dialog}
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
