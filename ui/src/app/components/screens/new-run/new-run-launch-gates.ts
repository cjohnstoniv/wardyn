/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The LOCAL arms of "can Launch be pressed": everything the form itself can
// say, with no help from a preflight answer. The launch panel renders them (the
// rail's problem sentence, the workspace disable) and the screen reads the same
// result to decide whether an automatic preflight may fire (use-launch.ts), so
// the two can never disagree about a body. Arms derived from preflight's own
// answer (backend, llm_access) are NOT here: they live in use-launch's
// `preflightBlock` and never gate firing a check.
import type { MeCapabilities, RunPolicySpec, SetupModelProvider, Workspace } from "../../../lib/types";
import { RAIL_PROVIDER, RUN } from "../../wardyn/copy";
import { POLICY_TEMPLATE_COPY } from "../../wardyn/copy/policy-templates";
import type { PolicyMode } from "../../wardyn/policy-panel";
import { savedPolicyGone } from "./policy-lane";
import { workspaceUnavailableToCaller, type WizardState } from "./wizard-types";
import type { ProviderGate } from "./model-provider-lane";

export interface LaunchGateInputs {
  isAgent: boolean;
  mode: WizardState["mode"];
  task: string;
  policyMode: PolicyMode;
  specParsedOk: boolean;
  selectedPolicyId: string | undefined;
  savedPolicy: { id: string; name: string; spec: RunPolicySpec } | undefined;
  policiesLoaded: boolean;
  pin: string | undefined;
  workspaces: Workspace[];
  selectedWorkspaceId: string | undefined;
  /** How many workspaces the Workspace card shows as attached (the primary and its chips). */
  attachedWorkspaces: number;
  caps: MeCapabilities | null;
  modelProviders: SetupModelProvider[] | undefined;
  providerGateState: ProviderGate | undefined;
  providerCandidates: SetupModelProvider[];
  selectedModelProviderId: string | undefined;
  agentName: string;
}

export interface LaunchGates {
  /** The rail's one validation sentence, null when the form has none. */
  problem: string | null;
  /** The picked workspace refuses this caller (#922) — disables Launch through
   *  the rail's own prop, never as a `problem` sentence (review F5). */
  workspaceUnavailable: boolean;
  /** The default lane's own refusal, for the default-policy panel to show
   *  beside Check again: set whenever it applies, even while `problem` is
   *  carrying an earlier arm's sentence. */
  defaultWorkspaceProblem: string | null;
}

// The default lane sends the primary workspace by reference and nothing else
// (use-launch.ts), and the API has no second attachment on that path. A second
// attached workspace is therefore refused here, never left out of the request.
// The saved lane sends the same one-reference body and is not held here.
export function defaultLaneDropsWorkspace(policyMode: PolicyMode, attachedWorkspaces: number): boolean {
  return policyMode === "default" && attachedWorkspaces > 1;
}

export function launchGates(i: LaunchGateInputs): LaunchGates {
  const needsTask = !i.isAgent || i.mode === "batch";
  // The model-provider arm is gated on `isAgent` — a Shell/exec run sends no
  // `agent`, so the server's model-provider door never asks it (review F2). The
  // workspace and git-provider arms refuse regardless of run type.
  const pickedWorkspace = i.workspaces.find((w) => w.id === i.selectedWorkspaceId);
  const workspaceUnavailable =
    !!pickedWorkspace && workspaceUnavailableToCaller(pickedWorkspace, i.caps, i.modelProviders, i.isAgent);
  const gate = i.providerGateState;
  const defaultWorkspaceProblem = defaultLaneDropsWorkspace(i.policyMode, i.attachedWorkspaces)
    ? POLICY_TEMPLATE_COPY.DEFAULT_ONE_WORKSPACE
    : null;
  const problem =
    needsTask && !i.task.trim()
      ? i.isAgent
        ? "An autonomous run needs a task to perform."
        : "Enter a command to run."
      : // A Custom policy that doesn't parse has nothing to send. The saved
        // lane launches by reference and the default lane sends no policy at
        // all, so neither puts a document on the wire.
        i.policyMode === "custom" && !i.specParsedOk
        ? "The policy spec isn't valid JSON."
        : savedPolicyGone(i.policyMode, i.selectedPolicyId, i.savedPolicy, i.policiesLoaded) // F2-F5
          ? RUN.POLICY_GONE
          : i.policyMode === "saved" && !i.selectedPolicyId
            ? "Pick a saved policy, or write a custom one."
            : // R5b (#1052) — no provider serves this person for this agent at
              // all, though one serves it org-wide.
              gate?.kind === "not_granted"
              ? RAIL_PROVIDER.NOT_GRANTED(i.agentName)
              : // R5c — the admin's own default is disabled; silent once an
                // explicit pick lands.
                gate?.kind === "default_off" && !i.selectedModelProviderId
                ? i.providerCandidates.length > 0
                  ? RAIL_PROVIDER.DEFAULT_OFF(gate.provider.name ?? gate.provider.id, i.agentName)
                  : RAIL_PROVIDER.DEFAULT_OFF_ONLY(gate.provider.name ?? gate.provider.id, i.agentName)
                : // R6 (QC-4): several candidates, none granted as default —
                  // Launch waits for an explicit pick; a workspace pin already
                  // answers that its own way, so this stays silent then.
                  i.providerCandidates.length > 1 && !i.selectedModelProviderId && !i.pin
                  ? RAIL_PROVIDER.LAUNCH_HINT
                  : // The default lane with a second workspace attached.
                    defaultWorkspaceProblem;
  return { problem, workspaceUnavailable, defaultWorkspaceProblem };
}
