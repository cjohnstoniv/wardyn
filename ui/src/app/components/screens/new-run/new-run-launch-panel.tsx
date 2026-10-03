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
import { RAIL_PROVIDER, RUN } from "../../wardyn/copy";
import { RAIL_MODEL_ACCESS } from "../../wardyn/model-access-copy";
import { savedPolicyGone } from "./policy-lane";
import { workspaceUnavailableToCaller, type WizardState } from "./wizard-types";
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

  // The screen's ONE validation rule. Deliberately a local derivation rather
  // than a shared validator: it answers "can this button be pressed", which is
  // this screen's question, and a second general-purpose answer living
  // elsewhere is what drifts out of sync with the form it describes.
  const needsTask = !isAgent || mode === "batch";
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
  // WORKSPACE and git-provider arms (#1267's `available_to_you`) are NOT
  // gated — they refuse regardless of run type, because the server excludes
  // the model-provider pin from that flag for the identical reason.
  const pickedWorkspace = workspaces.find((w) => w.id === selectedWorkspaceId);
  const workspaceUnavailable =
    !!pickedWorkspace && workspaceUnavailableToCaller(pickedWorkspace, caps, modelProviders, isAgent);
  // #1197 L2: Title dropped out of this chain — the server never required
  // one (runs_create_validate.go's own doc comment), only the console did,
  // and the console default now derives one from the task instead of asking.
  //
  // review F5: `workspaceUnavailable` is NOT a clause here — it disables
  // Launch through the rail's own `workspaceUnavailable` prop instead (below),
  // so the sentence renders exactly once, on the workspace picker's own
  // advisory line (workspace-card.tsx), never a second time in the rail's
  // problem slot.
  const modelBlocked =
    isAgent &&
    !isInteractive &&
    preflightIsCurrent &&
    !!preflightResult?.setup_items?.some((i) => i.kind === "llm_access" && i.status === "missing");
  const problem = needsTask && !task.trim()
    ? isAgent
      ? "An autonomous run needs a task to perform."
      : "Enter a command to run."
    : // A Custom policy that doesn't parse has nothing to send. The saved
      // lane launches by reference, so its body is never on the wire.
      !useSaved && !specParsedOk
      ? "The policy spec isn't valid JSON."
      : savedPolicyGone(useSaved, selectedPolicyId, savedPolicy, policiesLoaded) // F2-F5
        ? RUN.POLICY_GONE
        : useSaved && !selectedPolicyId
          ? "Pick a saved policy, or write a custom one."
          : // R5b (#1052) — no provider serves this person for this agent at
            // all, though at least one serves it org-wide. Launch is refused,
            // never silently left on R9's "nothing to see" shape.
            providerGateState?.kind === "not_granted"
            ? RAIL_PROVIDER.NOT_GRANTED(agentName)
            : // R5c (#542 rail-gap packet) — the admin's own default is
              // disabled. Named regardless of how many other candidates
              // remain, until an explicit pick lands (same "silent once
              // chosen" rule as R7's changeNote).
              providerGateState?.kind === "default_off" && !selectedModelProviderId
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
              providerCandidates.length > 1 && !selectedModelProviderId && !pin
              ? RAIL_PROVIDER.LAUNCH_HINT
              : // f-f5: mirrors the server's runNeedsModelWarning — an unattended
                // agent run with no reachable model waits. Interactive bodies are
                // exempt (a shell still works). The verdict is the current body's
                // own preflight `llm_access` row, never the mount-time llm_ready.
                modelBlocked
                ? RAIL_MODEL_ACCESS.UNATTENDED_BLOCK(agentName)
                : null;
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
