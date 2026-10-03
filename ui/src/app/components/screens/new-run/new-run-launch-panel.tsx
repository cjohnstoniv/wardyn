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
import { RAIL_SETUP } from "../../wardyn/copy";
import { RAIL_MODEL_ACCESS } from "../../wardyn/model-access-copy";
import { launchGates } from "./new-run-launch-gates";
import type { WizardState } from "./wizard-types";
import { RunRail } from "./new-run-rail";
import type { ProviderGate } from "./model-provider-lane";

export interface NewRunLaunchPanelProps {
  governanceProfile: string | undefined;
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
  credentialRefused: boolean;
  refusedProvider: string | undefined;
  /** The request Launch would send right now — see RunRailProps.launch.body. */
  launchBody: string | null;
  /** Re-runs preflight on the current body: what a preflight-origin sign-in does. */
  onPreflight: () => Promise<void>;
  preflightRefusal: { body: string; provider: string } | null;
  noBarrier: boolean;

  /** The screen's ONE validation rule (RunRail's `launch.problem`) and the
   *  #922 workspace-availability disable (`launch.workspaceUnavailable`) —
   *  both derived HERE from the raw inputs below, since neither is read
   *  anywhere else on the screen. */
  mode: WizardState["mode"];
  task: string;
  useSaved: boolean;
  specParsedOk: boolean;
  selectedPolicyId: string | undefined;
  policiesLoaded: boolean;
  pin: string | undefined;
  workspaces: Workspace[];
  selectedWorkspaceId: string | undefined;
  caps: MeCapabilities | null;
  modelProviders: SetupModelProvider[] | undefined;

  /** RunRail's `preflight` object, flattened. */
  preflightIsCurrent: boolean;
  /** Current body AND graded less than a minute ago (use-launch.ts). */
  preflightFresh: boolean;
  /** A fresh refusal for this body holds Launch (use-launch's preflightBlock). */
  preflightBlock: boolean;
  preflightError: string | null;
  preflightErrorSeq: number;
  preflightResult: PreflightResult | null;

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
  governanceProfile,
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
  credentialRefused,
  refusedProvider,
  launchBody,
  onPreflight,
  preflightRefusal,
  noBarrier,
  mode,
  task,
  useSaved,
  specParsedOk,
  selectedPolicyId,
  policiesLoaded,
  pin,
  workspaces,
  selectedWorkspaceId,
  caps,
  modelProviders,
  preflightIsCurrent,
  preflightFresh,
  preflightBlock,
  preflightError,
  preflightErrorSeq,
  preflightResult,
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

  // The screen's ONE validation rule lives in launchGates, the same pure
  // derivation the screen reads to decide whether an automatic preflight may
  // fire. `workspaceUnavailable` is NOT a `problem` clause: it disables Launch
  // through the rail's own prop, so the sentence renders once, on the workspace
  // picker's own advisory line (review F5).
  const gates = launchGates({
    isAgent,
    mode,
    task,
    useSaved,
    specParsedOk,
    selectedPolicyId,
    savedPolicy,
    policiesLoaded,
    pin,
    workspaces,
    selectedWorkspaceId,
    caps,
    modelProviders,
    providerGateState,
    providerCandidates,
    selectedModelProviderId,
    agentName,
  });
  const workspaceUnavailable = gates.workspaceUnavailable;
  // f-f4: preflight's `backend` row says this runner cannot enforce the run's
  // barrier; Launch would 422 on it, so the rail says so first. Only a FRESH
  // verdict for the CURRENT body counts (use-launch's preflightFresh), and only
  // `missing`: an `unverified` row never blocks. Not when `noBarrier`: a host
  // with no barrier at all has its own host-wide line in the rail.
  const backendMissing =
    !noBarrier &&
    preflightFresh &&
    !!preflightResult?.setup_items?.some((i) => i.kind === "backend" && i.status === "missing");
  // f-f5: mirrors the server's runNeedsModelWarning. An unattended agent run
  // with no reachable model waits. Interactive bodies are exempt. The verdict
  // is the current body's own fresh preflight `llm_access` row.
  const modelBlocked =
    isAgent &&
    !isInteractive &&
    preflightFresh &&
    !!preflightResult?.setup_items?.some((i) => i.kind === "llm_access" && i.status === "missing");
  const problem = backendMissing
    ? RAIL_SETUP.BACKEND_BLOCK
    : (gates.problem ?? (modelBlocked ? RAIL_MODEL_ACCESS.UNATTENDED_BLOCK(agentName) : null));
  // The Connect link belongs to the unattended-block sentence only; an
  // earlier arm that wins while modelBlocked is true keeps its own sentence.
  const modelBlockShown = modelBlocked && problem === RAIL_MODEL_ACCESS.UNATTENDED_BLOCK(agentName);

  return (
    <RunRail
      governanceProfile={governanceProfile}
      savedPolicy={savedPolicy}
      cc={cc}
      showModelWarning={showModelWarning && !modelBlocked}
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
        // noBarrier follows workspaceUnavailable's rule (#1328 review F4 — ONE
        // source of truth: the rail's own disabled check already folds
        // `noBarrier` in, so `disabled` never duplicates it): the rail states
        // ITS OWN reason beside Launch, so `problem` never also carries it.
        workspaceUnavailable,
        noBarrier,
        error,
        errorSeq,
        credentialRefused,
        refusedProvider,
        body: launchBody,
      }}
      preflight={
        preflightIsCurrent
          ? { error: preflightError, errorSeq: preflightErrorSeq, result: preflightResult, onPreflight, refusal: preflightRefusal }
          : { error: null, errorSeq: preflightErrorSeq, result: null, onPreflight, refusal: preflightRefusal }
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
