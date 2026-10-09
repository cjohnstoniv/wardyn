/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run-create request/result shapes (POST /api/v1/runs).

import type { Agent, AgentRun, ConfinementClass } from "./runs";

// The fields the New Run wizard composes into a POST /api/v1/runs body. policy_id
// and inline_policy are MUTUALLY EXCLUSIVE (XOR); neither set => default policy.
export interface CreateRunInput {
  // Omitted for a governed command (task_mode=exec) whose target already
  // carries a real base image — an explicit `image`, or a selected workspace
  // with one: task_mode=exec runs no agent harness, so naming one there was a
  // formality (see agentRequirementError, server-side). Every other run still
  // requires it.
  agent?: Agent;
  repo: string;
  task: string;
  // The run's name (grouping key) and an optional note on why it exists.
  // Optional on the wire — the server accepts a titleless run so the CLI and
  // the server's own system runs keep working — but REQUIRED by the New Run
  // screen, which is where a human is present to name the thing.
  title?: string;
  description?: string;
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
  // task_mode's INTERACTIVE counterpart: what the attach shell opens with.
  // "agent" launches the image's agent CLI in the prepared workspace once, on
  // first attach; "shell" / omitted is a bare terminal there. Ignored for a
  // non-interactive run (the server drops it structurally).
  interactive_start?: "shell" | "agent";
  // Opt-in for an interactive run's agent-started boot seed (`task`,
  // interpreted per interactive_start above): true lets the seed use tools
  // before a human attaches, instead of parking at its first tool-approval
  // prompt until someone joins. Omitted/false = supervised (default).
  seed_auto_tools?: boolean;
  // Tool-approval posture for an AUTONOMOUS (non-interactive) Claude Code run:
  // "hold" routes every tool call through a Wardyn approval instead of running
  // unsupervised. Omitted (wire default "auto") is today's behavior. Rejected
  // by the server for codex-cli and structurally inert for an interactive run.
  tool_approvals?: "auto" | "hold";
}

// POST /api/v1/runs response: the created run's fields PLUS an optional advisory
// `warnings` list (e.g. a workspace-directory collision with another active run).
// The run still launched — warnings are surfaced (toast / inline notice) but they
// never block. Structurally assignable to AgentRun, so onCreated callbacks that
// expect an AgentRun keep working.
export type CreateRunResult = AgentRun & { warnings?: string[] };
