/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run canon strings for step 3, Harnesses, byte for byte from docs/design/new-run-canon.md.
// Lazy-only: imported by the New Run flow and never re-exported through copy.ts, whose barrel is in the eager
// entry chunk. new-run-copy.test.ts keeps this file and the doc equal in both directions.

export const NR_HARNESSES = {
  TITLE: "Choose the harnesses",
  LEDE_OPTIONAL:
    "Harnesses are optional for this workload. Add one only when the sandbox should include an agent tool.",
  LEDE_INTERACTIVE:
    "Add one or more harnesses. At most one can start with the run and receive an optional starting prompt.",
  LEDE_AUTONOMOUS: "Pick the agent and give it its task.",
  EMPTY_TITLE: "No harnesses added",
  EMPTY_BODY: "This workload can continue without one.",
  LOADING: "Loading harnesses…",
  EYEBROW: (n: string | number) => `Harness ${n}`,
  HARNESS_LABEL: "Harness",
  AVAILABLE: (name: string | number) => `${name} is available in this environment.`,
  CODEX_HINT: "Codex CLI cannot hold tools for approval.",
  /** pending W-08: the recommended text until the owner answers. */
  CUSTOM_HINT: (name: string | number) => `${name} cannot hold tools for approval.`,
  /** pending W-08: the recommended text until the owner answers. */
  NOT_INSTALLED: (name: string | number) =>
    `${name} isn't installed in this server's sandbox images. Ask an admin to add an image that includes it.`,
  /** pending W-08: the recommended text until the owner answers. */
  NO_COMBINATION: (a: string | number, b: string | number) =>
    `No sandbox image here includes ${a} and ${b} together.`,
  /** pending W-08: the recommended text until the owner answers. */
  NO_PROVIDER: (name: string | number) => `No model provider you can use serves ${name}.`,
  NOT_ENABLED: "Not enabled by your admin",
  OPTIONAL_BEHAVIOUR: "Available inside the sandbox without changing how this workload starts.",
  START_IN_HARNESS: "Start the run in this harness",
  STARTING_PROMPT: "Starting prompt",
  STARTING_PROMPT_PLACEHOLDER: "Give this harness a starting point",
  STARTING_PROMPT_HELP: "Only the harness that starts with the run can receive a starting prompt.",
  TASK_LABEL: "Task",
  TASK_PLACEHOLDER: "What should this agent accomplish?",
  TASK_HELP: "This harness starts automatically when it has a task.",
  /** Lead wording 2026-10-17 (FR-0099): the Review blocker for a run that needs a harness and has none. */
  HARNESS_REQUIRED: "A run needs at least one harness.",
  TASK_ERROR: "An autonomous run needs at least one task to perform.",
  ADD_HARNESS: "Add a harness",
  ADD_ANOTHER: "Add another harness",
  ADDED: "Harness added.",
  REMOVED: "Harness removed.",
} as const;
