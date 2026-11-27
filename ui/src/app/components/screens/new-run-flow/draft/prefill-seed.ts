/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The router-state doors into New Run: the Runs composer, the clone fallback
// for a run with no launch intent, and the assistant's draft. Each arrives as the
// older wizard's `{ prefill }` state, read structurally here, and each door picks
// the named fields it takes; router state is never used whole as an intent.

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
  prefill?: {
    source?: string;
    inlinePolicy?: boolean;
    state?: PrefillState;
    /** The assistant's draft; only its `inline_policy` is taken. */
    document?: Pick<RunIntent, "inline_policy">;
    /** The assistant session the draft came from; the launch names it so the run is marked as drafted with it. */
    assistantSession?: string;
  };
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

/** The assistant's inline policy, when the router state carries a usable one; the steps choose everything else. */
function assistantPolicy(document: unknown): RunIntent["inline_policy"] | undefined {
  const policy = (document as { inline_policy?: unknown } | null)?.inline_policy;
  return policy && typeof policy === "object" && !Array.isArray(policy) ? (policy as RunIntent["inline_policy"]) : undefined;
}

/** A session id with nothing in it is no session. */
const assistantOrigin = (session: unknown): NewRunOrigin => {
  const id = typeof session === "string" ? session.trim() : "";
  return id ? { kind: "assistant", assistantSession: id } : { kind: "assistant" };
};

/** The seed a router-state prefill makes, or undefined when the state holds none. */
export function prefillSeed(state: unknown): { origin: NewRunOrigin; intent: RunIntent } | undefined {
  const prefill = (state as RouterPrefill | null)?.prefill;
  if (!prefill) return undefined;
  if (prefill.source === "assistant" || prefill.document) {
    // The assistant's door takes its inline policy and nothing else; with none it seeds nothing, never another door's fields.
    const inline_policy = assistantPolicy(prefill.document);
    return inline_policy ? { origin: assistantOrigin(prefill.assistantSession), intent: { inline_policy } } : undefined;
  }
  const s = prefill.state ?? {};
  return prefill.source === "composer"
    ? { origin: { kind: "composer" }, intent: composerIntent(s) }
    : { origin: { kind: "clone", ...(prefill.inlinePolicy ? { policyDropped: true } : {}) }, intent: cloneIntent(s) };
}
