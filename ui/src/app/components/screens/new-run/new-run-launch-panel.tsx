/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run's right rail, wired for this screen — split out of
// new-run-screen.tsx (the file's own 1000-line gate, scripts/check-file-size.sh).
// RunRail itself takes props and renders, owning no screen state (its own doc
// comment); this component is the same contract one level up — it assembles
// RunRail's nested `launch`/`preflight`/`modelProvider` objects, and derives
// `startup`/`workspaceUnavailable`/`problem` (each read ONLY by this panel),
// from the flat, explicit fields new-run-screen.tsx already computes. Nothing
// here reads or writes any of the screen's own React state.
import * as React from "react";
import type {
  ConfinementClass,
  MeCapabilities,
  PreflightResult,
  PushRulesSpec,
  RunPolicySpec,
  SetupHarnessTool,
  SetupModelProvider,
  SetupProviderAccess,
  Workspace,
} from "../../../lib/types";
import { RAIL_MODEL_ACCESS } from "../../wardyn/model-access-copy";
import { launchGates, preflightHolds, withPreflightIssues, type LaunchIssue, type NewRunPanelId } from "./new-run-launch-gates";
import type { PolicyMode } from "../../wardyn/policy-panel";
import type { WizardState } from "./wizard-types";
import { RunRail } from "./new-run-rail";
import type { ProviderGate } from "./model-provider-lane";
import type { PolicyRef } from "../../../lib/api/health";

export interface NewRunLaunchPanelProps {
  /** The panel on screen, and how an issue shows its own (RunRail's props). */
  panel?: NewRunPanelId;
  onIssue?: (issue: LaunchIssue) => void;
  guardLink?: React.ComponentProps<typeof RunRail>["guardLink"];
  governanceProfile: string | undefined;
  /** GET /me's governance_contact, for the remedy beside the profile line. */
  governanceContact?: PolicyRef;
  savedPolicy: { id: string; name: string; spec: RunPolicySpec } | undefined;
  cc: ConfinementClass;
  showModelWarning: boolean;
  toolRules: string | null;
  pushRules: PushRulesSpec | undefined;
  unattended: boolean;

  /** What happens the moment this launches, in one sentence (RunRail's
   *  `startup`) — derived HERE from the run's own shape, so the rail cannot
   *  describe one run while Launch sends another. */
  isInteractive: boolean;
  interactiveStart: WizardState["interactiveStart"];
  /** The server derives a hold in the OPPOSITE case from what this checks:
   *  autonomyDerive (runs_autonomy.go) sets tool_approvals=hold when the run
   *  is non-interactive, the agent has a hold lane (claude-code, here) and
   *  the request did NOT already ask for hold. Picking hold yourself derives
   *  nothing to announce. */
  agent: WizardState["agent"];
  toolApprovals: WizardState["toolApprovals"];

  /** RunRail's `launch` object, flattened to explicit fields — see
   *  new-run-rail.tsx's RunRailProps for what each one means. */
  onLaunch: () => void | Promise<string | void>;
  launchDisabled: boolean;
  launchSpinning: boolean;
  launching: boolean;
  error: string | null;
  errorSeq: number;
  /** The launch refusal's own `policy`, for the remedy under the alert. */
  errorPolicy?: PolicyRef;
  credentialRefused: boolean;
  refusedProvider: string | undefined;
  /** The request Launch would send right now — see RunRailProps.launch.body. */
  launchBody: string | null;
  draftRevision?: number;
  /** Re-runs preflight on the current body: what a preflight-origin sign-in does. */
  onPreflight: () => Promise<void>;
  preflightRefusal: { body: string; provider: string } | null;
  noBarrier: boolean;
  /** The barrier probe read no class list: no runner is configured, or the
   *  status read failed. Create-run skips its capability gate with no runner,
   *  so a `missing` backend row then predicts no refusal and never blocks. */
  runnerUnknown?: boolean;

  /** The screen's ONE validation rule (RunRail's `launch.problem`) and the
   *  #922 workspace-availability disable (`launch.workspaceUnavailable`) —
   *  both derived HERE from the raw inputs below, since neither is read
   *  anywhere else on the screen. */
  mode: WizardState["mode"];
  /** The text this run shape sends: the Task, the Command or the Startup command. */
  task: string;
  title: string;
  policyMode: PolicyMode;
  specParsedOk: boolean;
  selectedPolicyId: string | undefined;
  policiesLoaded: boolean;
  pin: string | undefined;
  workspaces: Workspace[];
  selectedWorkspaceId: string | undefined;
  attachedWorkspaces: number;
  caps: MeCapabilities | null;
  modelProviders: SetupModelProvider[] | undefined;

  /** RunRail's `preflight` object, flattened. */
  preflightIsCurrent: boolean;
  /** Current body AND graded less than a minute ago (use-launch.ts). */
  preflightFresh: boolean;
  /** A fresh refusal for this body holds Launch (use-launch's preflightBlock). */
  preflightBlock: boolean;
  /** A check is in flight / the current body's last check was a 429. */
  preflightChecking: boolean;
  preflightNotChecked: boolean;
  preflightError: string | null;
  preflightErrorSeq: number;
  preflightResult: PreflightResult | null;
  /** The Access rows that hold Launch (access-rows-model.ts's accessIssues). */
  accessIssues?: LaunchIssue[];

  agentRow: SetupHarnessTool | undefined;

  /** RunRail's `modelProvider` object, flattened — undefined (no section)
   *  whenever `isAgent` is false: a Shell/exec run has no model-provider door. */
  isAgent: boolean;
  providerCandidates: SetupModelProvider[];
  providerAccess: SetupProviderAccess[] | undefined;
  selectedModelProviderId: string | undefined;
  onModelProviderChange: (id: string) => void;
  providerChangeNote: string | null;
  providerGateState: ProviderGate | undefined;
  agentName: string;

  adoDialog: React.ComponentProps<typeof RunRail>["adoDialog"];
}

export function NewRunLaunchPanel({
  panel,
  onIssue,
  guardLink,
  governanceProfile,
  governanceContact,
  savedPolicy,
  cc,
  showModelWarning,
  toolRules,
  pushRules,
  unattended,
  isInteractive,
  interactiveStart,
  agent,
  toolApprovals,
  onLaunch,
  launchDisabled,
  launchSpinning,
  launching,
  error,
  errorSeq,
  errorPolicy,
  credentialRefused,
  refusedProvider,
  launchBody,
  draftRevision,
  onPreflight,
  preflightRefusal,
  noBarrier,
  runnerUnknown,
  mode,
  task,
  title,
  policyMode,
  specParsedOk,
  selectedPolicyId,
  policiesLoaded,
  pin,
  workspaces,
  selectedWorkspaceId,
  attachedWorkspaces,
  caps,
  modelProviders,
  preflightIsCurrent,
  preflightFresh,
  preflightBlock,
  preflightChecking,
  preflightNotChecked,
  preflightError,
  preflightErrorSeq,
  preflightResult,
  accessIssues,
  agentRow,
  isAgent,
  providerCandidates,
  providerAccess,
  selectedModelProviderId,
  onModelProviderChange,
  providerChangeNote,
  providerGateState,
  agentName,
  adoDialog,
}: NewRunLaunchPanelProps) {
  // What happens the moment this launches, in one sentence.
  const startup = isInteractive
    ? task.trim()
      ? interactiveStart === "agent"
        ? `Starts ${agentName} on your prompt at boot — attach to watch and take over.`
        : "Runs your startup command at boot, then a terminal is ready."
      : interactiveStart === "agent"
        ? `Comes up idle with the workspace ready. Attaching starts ${agentName} in it.`
        : "Comes up idle with the workspace ready. Attaching drops you into a terminal."
    : isAgent
      ? `${agentName} runs the task unattended, then the run stops.`
      : "The command runs unattended in the sandbox, then the run stops.";

  const showHoldNote = !isInteractive && isAgent && agent === "claude-code" && toolApprovals !== "hold";

  // The form's own reasons Launch is held live in launchGates, the same pure
  // derivation the screen reads to decide whether an automatic preflight may
  // fire and what the panel nav counts.
  const gates = launchGates({
    isAgent,
    mode,
    task,
    title,
    policyMode,
    specParsedOk,
    selectedPolicyId,
    savedPolicy,
    policiesLoaded,
    pin,
    workspaces,
    selectedWorkspaceId,
    attachedWorkspaces,
    caps,
    modelProviders,
    providerGateState,
    providerCandidates,
    selectedModelProviderId,
    agentName,
  });
  const workspaceUnavailable = gates.workspaceUnavailable;
  // Preflight's own two holds for this body join them: the first issue is
  // the one the rail names, and it is what disables Launch.
  const holds = preflightHolds({
    isAgent,
    isInteractive,
    agentName,
    noBarrier,
    runnerUnknown,
    preflightFresh,
    setupItems: preflightResult?.setup_items,
  });
  const modelBlocked = holds.modelBlocked;
  const issue = withPreflightIssues(gates.issues, holds, agentName, accessIssues)[0] ?? null;
  const problem = issue?.text ?? null;
  // The Connect link belongs to the unattended-block sentence only; an
  // earlier arm that wins while modelBlocked is true keeps its own sentence.
  const modelBlockShown = modelBlocked && problem === RAIL_MODEL_ACCESS.UNATTENDED_BLOCK(agentName);

  return (
    <RunRail
      panel={panel}
      onIssue={onIssue}
      guardLink={guardLink}
      governanceProfile={governanceProfile}
      governanceContact={governanceContact}
      savedPolicy={savedPolicy}
      cc={cc}
      showModelWarning={showModelWarning}
      modelBlocked={modelBlocked}
      startup={startup}
      showHoldNote={showHoldNote}
      toolRules={toolRules}
      pushRules={pushRules}
      unattended={unattended}
      launch={{
        onLaunch,
        disabled: launchDisabled,
        preflightBlock,
        problemLink: modelBlockShown ? { to: "/account", label: RAIL_MODEL_ACCESS.NO_PROVIDER_CTA } : undefined,
        spinning: launchSpinning,
        inFlight: launching,
        problem,
        issue,
        // noBarrier follows workspaceUnavailable's rule (#1328 review F4 — ONE
        // source of truth: the rail's own disabled check already folds
        // `noBarrier` in, so `disabled` never duplicates it): the rail states
        // ITS OWN reason beside Launch, so `problem` never also carries it.
        workspaceUnavailable,
        referenceHold: gates.referenceWorkspaceBlocked,
        noBarrier,
        error,
        errorSeq,
        policy: errorPolicy,
        credentialRefused,
        refusedProvider,
        body: launchBody,
        draftRevision,
      }}
      preflight={
        preflightIsCurrent
          ? { error: preflightError, errorSeq: preflightErrorSeq, result: preflightResult, onPreflight, refusal: preflightRefusal, checking: preflightChecking, notChecked: preflightNotChecked }
          : { error: null, errorSeq: preflightErrorSeq, result: null, onPreflight, refusal: preflightRefusal, checking: preflightChecking, notChecked: false }
      }
      agentRow={agentRow}
      modelProvider={
        isAgent
          ? {
              candidates: providerCandidates,
              access: providerAccess,
              selectedId: selectedModelProviderId,
              onChange: onModelProviderChange,
              changeNote: providerChangeNote,
              gate: providerGateState,
              harnessLabel: agentName,
            }
          : undefined
      }
      adoDialog={adoDialog}
    />
  );
}
