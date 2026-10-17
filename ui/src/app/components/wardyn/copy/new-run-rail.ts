/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// M1 (approved 2026-10-03) — the automatic check's states and its manual button.
export const RAIL_CHECK = {
  CHECKING: "Checking this run…",
  NOT_CHECKED: "Not checked — too many checks this minute. It retries on your next edit.",
  BUTTON: "Check again",
  HINT: "Checked as you edit. A refusal holds Launch for up to a minute.",
} as const;

// #542 (design §5.6, packet MP-C — approved as drawn 2026-09-25) — the New Run
// rail's provider picker: which of THIS PERSON's own credentials a run
// authenticates with, known from the chosen provider's kind before launch
// rather than resolved only by a dry run. R5's states (a disabled or wholly ungranted default) were excluded
// from PR #1036's build (owner ruling) and R5c is now drawn by the rail-gap
// packet below (owner-approved 2026-09-25, docs/design/542-rail-gaps-mock/canon.md) —
// DEFAULT_OFF/DEFAULT_OFF_ONLY.
export const RAIL_PROVIDER = {
  LABEL: "Model provider",
  STATIC: (name: string) => `Model provider · ${name}`,
  OPTION: (name: string, what: string, state: string) => `${name} — your ${what} · ${state}`,
  PLACEHOLDER: "Choose a model provider",
  NOT_SIGNED_IN: (name: string) => `You're not signed in to AWS for ${name}.`,
  NO_TOKEN: (name: string) => `No token is available for ${name}.`,
  LAUNCH_HINT: "Choose a model provider to launch.",
  CHANGED: (next: string, prev: string, harness: string) =>
    `Model provider changed to ${next} — ${prev} isn't available to ${harness}.`,
  // #542 rail-gap packet (canon.md's "R3 — selected, not connected (the three
  // missing kinds)") — the other two credential kinds ProviderNotConnectedLine
  // had no branch for: a stored key (anthropic_api_key/openai_api_key) and the
  // non-Bedrock sign-in kind (anthropic_subscription).
  NO_KEY: (name: string) => `No key is available for ${name}.`,
  NOT_SIGNED_IN_CLAUDE: (name: string) => `You're not signed in to Claude for ${name}.`,
  // canon.md's "R5b — granted none": no candidate serves this person for this
  // harness at all. UNUSED for now (Opus review round 2, #542): the console
  // has no signal to tell this apart from R9's silent shape —
  // setupModelProviderState's capVisible (internal/api/provider_access.go)
  // already narrows `model_providers` to granted providers before the wire,
  // so an ungranted one never reaches the console to read as "all disabled".
  // Kept defined because it is canon-pinned (new-run-rail.test.ts); a
  // follow-up issue gives the console a real signal and wires this in.
  NOT_GRANTED: (harness: string) => `You haven't been granted a model provider for ${harness} — ask your admin.`,
  // canon.md's "R5c — the default is turned off": the admin's own default is a
  // disabled provider. DEFAULT_OFF names it when another candidate remains to
  // choose instead; DEFAULT_OFF_ONLY when none does.
  DEFAULT_OFF: (name: string, harness: string) =>
    `${name}, the default for ${harness}, is turned off. Choose another model provider to launch.`,
  DEFAULT_OFF_ONLY: (name: string, harness: string) =>
    `${name}, the default for ${harness}, is turned off. Ask your admin.`,
} as const;
