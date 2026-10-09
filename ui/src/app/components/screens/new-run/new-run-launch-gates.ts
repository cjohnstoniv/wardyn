/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The LOCAL arms of "can Launch be pressed": everything the form itself can
// say, with no help from a preflight answer. Each arm is an ISSUE that names
// its panel and its owning control: the panel nav counts them, the line above
// Launch links to the first, and the screen reads the same list to decide
// whether an automatic preflight may fire (use-launch.ts), so none of the three
// can disagree about a body. Arms derived from preflight's own answer (backend,
// llm_access) are NOT here: the launch panel appends them, and they never gate
// firing a check.
import type { MeCapabilities, RunPolicySpec, SetupModelProvider, Workspace } from "../../../lib/types";
import { DENIED } from "../../../lib/permissions-copy";
import { PROVIDERS } from "../../../lib/workspace-providers-copy";
import { RAIL_PROVIDER, RAIL_SETUP, RUN } from "../../wardyn/copy";
import { RAIL_MODEL_ACCESS } from "../../wardyn/model-access-copy";
import { NEW_RUN_FLOW } from "../../wardyn/copy/new-run-flow";
import { POLICY_DOCUMENT } from "../../wardyn/copy/policy-document";
import { POLICY_TEMPLATE_COPY } from "../../wardyn/copy/policy-templates";
import type { PolicyMode } from "../../wardyn/policy-panel";
import { savedPolicyGone } from "./policy-lane";
import { hasSourceNotAdmitted, workspaceUnavailableToCaller, type WizardState } from "./wizard-types";
import type { ProviderGate } from "./model-provider-lane";

export type NewRunPanelId = "run" | "workspace" | "access" | "policy";

/** The DOM ids issue links focus. One list, so a control and its issue cannot
 *  drift apart. A wrapper id focuses the first control inside it. */
export const ISSUE_TARGET = {
  TITLE: "nr-title",
  TASK: "nr-task",
  RUN_MODE: "nr-run-mode",
  PROVIDER: "nr-provider",
  PROVIDER_PICKER: "nr-model-provider",
  WORKSPACE: "nr-workspace",
  POLICY_MODE: "nr-policy-mode",
  SAVED_POLICY: "nr-saved-policy",
  POLICY_SOURCE: "policy-spec-run",
  /** The read view's heading: where a link to a read-only policy lands. */
  POLICY_READ: "nr-policy-read-title",
  BARRIER: "nr-barrier",
  TOOL_RULES: "policy-tool-rules-run",
} as const;

/** The line above Launch that names the first issue; a control whose issue it
 *  is points its aria-describedby here. */
export const ISSUE_LINE_ID = "nr-launch-issue";

/** One thing that holds Launch. */
export interface LaunchIssue {
  panel: NewRunPanelId;
  /** The owning control's id (ISSUE_TARGET). */
  focus: string;
  text: string;
  /** Printed beside its own control, so the line above Launch stays silent
   *  while that panel is the one on screen: a sentence is never shown twice. */
  inline?: boolean;
}

export interface LaunchGateInputs {
  isAgent: boolean;
  mode: WizardState["mode"];
  /** The text this run shape requires: the Task, or the Command. */
  task: string;
  title: string;
  policyMode: PolicyMode;
  specParsedOk: boolean;
  selectedPolicyId: string | undefined;
  savedPolicy: { id: string; name: string; spec: RunPolicySpec } | undefined;
  policiesLoaded: boolean;
  pin: string | undefined;
  workspaces: Workspace[];
  selectedWorkspaceId: string | undefined;
  /** How many workspaces the Workspace panel shows as attached (the primary and its chips). */
  attachedWorkspaces: number;
  caps: MeCapabilities | null;
  modelProviders: SetupModelProvider[] | undefined;
  providerGateState: ProviderGate | undefined;
  providerCandidates: SetupModelProvider[];
  selectedModelProviderId: string | undefined;
  agentName: string;
}

export interface LaunchGates {
  /** Every local reason Launch is held, in the order the line above Launch
   *  names them. Empty means the form itself has nothing against launching. */
  issues: LaunchIssue[];
  /** The picked workspace refuses this caller (#922). */
  workspaceUnavailable: boolean;
  /** The reference lane's own refusal (default or saved, one sentence each),
   *  printed beside Check again while Policy is on screen: set whenever it
   *  applies. It is also an inline issue, so the line above Launch names it
   *  only from another panel. */
  referenceWorkspaceProblem: string | null;
  /** Neither reference policy mode can carry extra workspace attachments. */
  referenceWorkspaceBlocked: boolean;
}

// The API's workspaces[] only narrows attachments; it cannot attach a second
// workspace to a saved/default policy reference.
export function referenceLaneDropsWorkspace(policyMode: PolicyMode, attachedWorkspaces: number): boolean {
  return (policyMode === "default" || policyMode === "saved") && attachedWorkspaces > 1;
}

// The model-provider arm: at most one sentence. Gated on `isAgent` by the
// caller — a Shell/exec run sends no `agent`, so the server's model-provider
// door never asks it (review F2).
function providerIssue(i: LaunchGateInputs): LaunchIssue | null {
  const gate = i.providerGateState;
  // R5b (#1052) — no provider serves this person for this agent at all, though
  // one serves it org-wide.
  if (gate?.kind === "not_granted") {
    return { panel: "run", focus: ISSUE_TARGET.PROVIDER, text: RAIL_PROVIDER.NOT_GRANTED(i.agentName), inline: true };
  }
  // R5c — the admin's own default is disabled; silent once an explicit pick lands.
  if (gate?.kind === "default_off" && !i.selectedModelProviderId) {
    const name = gate.provider.name ?? gate.provider.id;
    return i.providerCandidates.length > 0
      ? { panel: "run", focus: ISSUE_TARGET.PROVIDER_PICKER, text: RAIL_PROVIDER.DEFAULT_OFF(name, i.agentName), inline: true }
      : { panel: "run", focus: ISSUE_TARGET.PROVIDER, text: RAIL_PROVIDER.DEFAULT_OFF_ONLY(name, i.agentName), inline: true };
  }
  // R6 (QC-4): several candidates, none granted as default — Launch waits for
  // an explicit pick; a workspace pin already answers that its own way, so this
  // stays silent then.
  if (i.providerCandidates.length > 1 && !i.selectedModelProviderId && !i.pin) {
    return { panel: "run", focus: ISSUE_TARGET.PROVIDER_PICKER, text: RAIL_PROVIDER.LAUNCH_HINT };
  }
  return null;
}

export function launchGates(i: LaunchGateInputs): LaunchGates {
  const issues: LaunchIssue[] = [];
  const needsTask = !i.isAgent || i.mode === "batch";
  if (needsTask && !i.task.trim()) {
    issues.push({
      panel: "run",
      focus: ISSUE_TARGET.TASK,
      text: i.isAgent ? "An autonomous run needs a task to perform." : "Enter a command to run.",
    });
  }
  if (!i.title.trim()) issues.push({ panel: "run", focus: ISSUE_TARGET.TITLE, text: NEW_RUN_FLOW.TITLE_REQUIRED });

  // A Custom policy that doesn't parse has nothing to send. The saved lane
  // launches by reference and the default lane sends no policy at all, so
  // neither puts a document on the wire.
  if (i.policyMode === "custom" && !i.specParsedOk) {
    issues.push({ panel: "policy", focus: ISSUE_TARGET.POLICY_SOURCE, text: POLICY_DOCUMENT.INVALID_GATE });
  } else if (savedPolicyGone(i.policyMode, i.selectedPolicyId, i.savedPolicy, i.policiesLoaded)) {
    issues.push({ panel: "policy", focus: ISSUE_TARGET.SAVED_POLICY, text: RUN.POLICY_GONE, inline: true }); // F2-F5
  } else if (i.policyMode === "saved" && !i.selectedPolicyId) {
    issues.push({ panel: "policy", focus: ISSUE_TARGET.SAVED_POLICY, text: "Pick a saved policy, or write a custom one." });
  }

  // The workspace arms refuse regardless of run type; only the pinned-provider
  // half of workspaceUnavailableToCaller is agent-gated.
  const pickedWorkspace = i.workspaces.find((w) => w.id === i.selectedWorkspaceId);
  const workspaceUnavailable =
    !!pickedWorkspace && workspaceUnavailableToCaller(pickedWorkspace, i.caps, i.modelProviders, i.isAgent);
  if (workspaceUnavailable) {
    issues.push({ panel: "workspace", focus: ISSUE_TARGET.WORKSPACE, text: DENIED.WORKSPACE_NOT_AVAILABLE, inline: true });
  }
  if (pickedWorkspace && hasSourceNotAdmitted(pickedWorkspace)) {
    issues.push({ panel: "workspace", focus: ISSUE_TARGET.WORKSPACE, text: PROVIDERS.CARD_NOT_ADMITTED, inline: true });
  }

  const referenceWorkspaceBlocked = referenceLaneDropsWorkspace(i.policyMode, i.attachedWorkspaces);
  // M-F: one sentence per reference lane, printed once beside Check again
  // (policy-panel.tsx's POLICY_HOLD_ID) and named above Launch from any other panel.
  const referenceWorkspaceProblem = !referenceWorkspaceBlocked
    ? null
    : i.policyMode === "saved"
      ? POLICY_TEMPLATE_COPY.SAVED_ONE_WORKSPACE
      : POLICY_TEMPLATE_COPY.DEFAULT_ONE_WORKSPACE;
  if (referenceWorkspaceProblem) {
    issues.push({ panel: "policy", focus: ISSUE_TARGET.POLICY_MODE, text: referenceWorkspaceProblem, inline: true });
  }

  const provider = i.isAgent ? providerIssue(i) : null;
  if (provider) issues.push(provider);

  return { issues, workspaceUnavailable, referenceWorkspaceProblem, referenceWorkspaceBlocked };
}

export interface PreflightHoldInputs {
  isAgent: boolean;
  isInteractive: boolean;
  agentName: string;
  noBarrier: boolean;
  /** No runner is configured, or the status read failed: create-run skips its
   *  capability gate then, so a `missing` backend row predicts no refusal. */
  runnerUnknown?: boolean;
  /** Current body AND graded less than a minute ago (use-launch.ts). */
  preflightFresh: boolean;
  setupItems: { kind: string; status: string }[] | undefined;
}

/** The two holds read off preflight's OWN answer for the current body. Only a
 *  FRESH verdict counts, and only a `missing` row: `unverified` never blocks. */
export function preflightHolds(i: PreflightHoldInputs): { backendMissing: boolean; modelBlocked: boolean } {
  const missing = (kind: string) => i.preflightFresh && !!i.setupItems?.some((r) => r.kind === kind && r.status === "missing");
  return {
    // f-f4: this runner cannot enforce the run's barrier; Launch would 422 on it.
    // Not when `noBarrier` (a host with no barrier at all has its own host-wide
    // line beside Launch) nor when `runnerUnknown`.
    backendMissing: !i.noBarrier && !i.runnerUnknown && missing("backend"),
    // f-f5: mirrors the server's runNeedsModelWarning. An unattended agent run
    // with no reachable model waits; interactive bodies are exempt.
    modelBlocked: i.isAgent && !i.isInteractive && missing("llm_access"),
  };
}

/** Every reason Launch is held, first one first: the barrier this host cannot
 *  build outranks the form's own issues, then the Access rows that hold it
 *  (access-rows-model.ts, read off the same two checks), and the model block
 *  speaks last. */
export function withPreflightIssues(
  local: LaunchIssue[],
  holds: { backendMissing: boolean; modelBlocked: boolean },
  agentName: string,
  accessIssues: LaunchIssue[] = [],
): LaunchIssue[] {
  return [
    ...(holds.backendMissing ? [{ panel: "policy" as const, focus: ISSUE_TARGET.BARRIER, text: RAIL_SETUP.BACKEND_BLOCK }] : []),
    ...local,
    ...accessIssues,
    ...(holds.modelBlocked
      ? [{ panel: "run" as const, focus: ISSUE_TARGET.RUN_MODE, text: RAIL_MODEL_ACCESS.UNATTENDED_BLOCK(agentName) }]
      : []),
  ];
}

/** How many issues each panel owns, for the panel nav's chips. */
export function issueCounts(issues: LaunchIssue[]): Record<NewRunPanelId, number> {
  const counts: Record<NewRunPanelId, number> = { run: 0, workspace: 0, access: 0, policy: 0 };
  for (const issue of issues) counts[issue.panel] += 1;
  return counts;
}

/** The issue the line above Launch names: the first one, unless its own panel
 *  is on screen and already prints it beside its control. */
export function shownIssue(issues: LaunchIssue[], panel: NewRunPanelId | undefined): LaunchIssue | null {
  const first = issues[0];
  return first && !(first.inline && first.panel === panel) ? first : null;
}
