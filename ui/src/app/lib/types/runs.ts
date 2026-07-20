/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Core run identity/state types + the run-create input/result shapes.
// All wire fields are snake_case.

// The backend emits dotted agent ids like "claude-code" / "codex-cli".
// Older mock data used "claude_code" / "codex". Keep the union open
// (string) so label mapping can tolerate both forms; the literals below
// are kept for editor hints / autocomplete only.
export type Agent =
  | "claude-code"
  | "codex-cli"
  | "claude_code"
  | "codex"
  | "cursor"
  | (string & {});

export type ConfinementClass = "CC1" | "CC2" | "CC3";

export type RunState =
  | "PENDING"
  | "STARTING"
  | "RUNNING"
  | "WAITING_FOR_CONFIRMATION"
  | "COMPLETED"
  | "STOPPED"
  | "ARCHIVED"
  | "FAILED"
  | "KILLED"
  // Keep the union open so an unrecognized backend state degrades to a
  // neutral badge instead of crashing the console (see primitives.tsx).
  | (string & {});

export interface AgentRun {
  id: string;
  created_at: string;
  updated_at: string;
  created_by: string;
  agent: Agent;
  repo: string;
  task: string;
  policy_id?: string;
  confinement_class: ConfinementClass;
  state: RunState;
  spiffe_id: string;
  runner_target: string;
  sandbox_ref?: string;
  // interactive runs come up idle (no agent task) so a human attaches and drives
  // them via the WS PTY; the fleet board badges these as "awaiting attach".
  // Optional so an older backend payload (or a test fixture) without the field
  // still type-checks and degrades to autonomous.
  interactive?: boolean;
  // The host working-directory the run's workspace is bind-mounted from. Surfaced
  // on the fleet board so a workspace-directory collision is visible at a glance.
  workspace_path?: string;
  // The RESOLVED sandbox image the run dispatched with (convention image,
  // devcontainer build, workspace-built, or BYOI-wrapped) — provenance, shown
  // on run detail. Empty/absent for legacy rows.
  image?: string;
}

// Terminal run states: the run has finished and can no longer be killed.
// Mirrors the backend terminal guard in internal/api/runs.go. COMPLETED is the
// terminal-success state (agent exited 0) and MUST be included — omitting it
// left the Kill button enabled on finished runs (the backend then 409s).
export const TERMINAL_RUN_STATES: readonly RunState[] = [
  "COMPLETED",
  "STOPPED",
  "ARCHIVED",
  "FAILED",
  "KILLED",
];

export function isTerminalRunState(state: RunState): boolean {
  return (TERMINAL_RUN_STATES as readonly string[]).includes(state as string);
}

// The fields the New Run wizard composes into a POST /api/v1/runs body. policy_id
// and inline_policy are MUTUALLY EXCLUSIVE (XOR); neither set => default policy.
export interface CreateRunInput {
  agent: Agent;
  repo: string;
  task: string;
  policy_id?: string;
  confinement_class?: ConfinementClass;
  interactive?: boolean;
  // Bring Your Own Image: a user-supplied base image the backend WRAPS with the
  // runner tools (FROM <image> + COPY tools + cleared ENTRYPOINT) before use.
  // Mutually exclusive with a devcontainer build. Omitted → the convention image.
  image?: string;
  // task_mode selects how a non-interactive run executes `task`: "" / "harness"
  // (default) runs the agent harness; "exec" runs `task` as a plain shell
  // command in the same governed sandbox — no agent, no LLM credentials.
  // Ignored for an interactive run. Omitted → "harness" (backward-compatible).
  task_mode?: "harness" | "exec";
}

// POST /api/v1/runs response: the created run's fields PLUS an optional advisory
// `warnings` list (e.g. a workspace-directory collision with another active run).
// The run still launched — warnings are surfaced (toast / inline notice) but they
// never block. Structurally assignable to AgentRun, so onCreated callbacks that
// expect an AgentRun keep working.
export type CreateRunResult = AgentRun & { warnings?: string[] };
