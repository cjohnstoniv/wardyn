/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The two router-state doors into New Run: the Runs composer and the clone
// fallback for a run with no launch intent. Both arrive as the older wizard's
// `{ prefill }` state, read structurally here.

import type { ConfinementClass } from "../../../../lib/types/runs";
import type { NewRunOrigin, RunIntent } from "./run-intent";

export interface PrefillState {
  task?: string;
  command?: string;
  startupCommand?: string;
  runType?: "agent" | "command";
  mode?: "interactive" | "batch";
  agent?: string;
  interactiveStart?: "shell" | "agent";
  toolApprovals?: "auto" | "hold";
  confinementClass?: ConfinementClass;
  selectedPolicyId?: string;
  workspaces?: { workspaceId: string }[];
}
export interface RouterPrefill {
  prefill?: { source?: string; inlinePolicy?: boolean; state?: PrefillState };
}

const workspacesOf = (s: PrefillState): Pick<RunIntent, "workspaces"> => {
  const workspaces = (s.workspaces ?? []).map((w) => ({ workspace_id: w.workspaceId }));
  return workspaces.length ? { workspaces } : {};
};

/** The Runs composer's text becomes an agent task; its workspace stays chosen. */
function composerIntent(s: PrefillState): RunIntent {
  return { shape: "agent_task", workload: { kind: "agent_task", task: s.task ?? "" }, ...workspacesOf(s) };
}

/**
 * A clone of a run, in the 0.9 carriers. Title and description are not cloned.
 * The older tool-approval hold becomes its harness's default "hold", never a
 * quieter run. With no harness this build can name, the hold stays as the
 * older field: the server refuses it beside the carriers until it is resolved,
 * and any harness added meanwhile starts held. The boot-seed opt-in only
 * widens, so it is not carried.
 */
function cloneIntent(s: PrefillState): RunIntent {
  const hold = s.toolApprovals === "hold";
  const tools = s.agent ? [{ id: s.agent, kind: "harness" as const, ...(hold ? { default_effect: "hold" as const } : {}) }] : undefined;
  const common: RunIntent = {
    ...(tools ? { tools } : {}),
    ...(hold && !tools ? { tool_approvals: "hold" as const } : {}),
    ...(s.confinementClass ? { confinement_class: s.confinementClass } : {}),
    ...(s.selectedPolicyId ? { policy_id: s.selectedPolicyId } : {}),
    ...workspacesOf(s),
  };
  if (s.runType === "command") {
    return { ...common, shape: "application", experience: "background", workload: { kind: "command", command: s.command ?? "" } };
  }
  if (s.mode !== "interactive") {
    return {
      ...common, shape: "agent_task", experience: "background",
      workload: { kind: "agent_task", ...(s.agent ? { agent: s.agent } : {}), task: s.task ?? "" },
    };
  }
  if (s.interactiveStart === "shell") {
    const command = s.startupCommand?.trim() ? { startup: { kind: "command" as const, command: s.startupCommand } } : {};
    return { ...common, shape: "environment", experience: "interactive", ...command };
  }
  // The run's first prompt is its task; a blank one is left out, never sent as an empty prompt.
  const prompt = s.task?.trim() ? { prompt: s.task } : {};
  return { ...common, shape: "agent_task", experience: "interactive", ...(s.agent ? { startup: { kind: "harness", tool: s.agent, ...prompt } } : {}) };
}

/** The seed a router-state prefill makes, or undefined when the state holds none. */
export function prefillSeed(state: unknown): { origin: NewRunOrigin; intent: RunIntent } | undefined {
  const prefill = (state as RouterPrefill | null)?.prefill;
  if (!prefill) return undefined;
  const s = prefill.state ?? {};
  return prefill.source === "composer"
    ? { origin: { kind: "composer" }, intent: composerIntent(s) }
    : { origin: { kind: "clone", ...(prefill.inlinePolicy ? { policyDropped: true } : {}) }, intent: cloneIntent(s) };
}
