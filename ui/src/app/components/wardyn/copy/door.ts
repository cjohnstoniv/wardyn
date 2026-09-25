/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The doors keyed by provider (packet MP-E, approved; design §5.9) and the
// provider strip that opens them (packet MP-D §5.5) — frozen strings verbatim.
// The AWS door's title, toast and cleanup note are reused from
// MODEL_ACCESS_BANNER, and its button from AGENTS.SIGN_IN_AWS.

// The shell strip under a provider block (B1–B8). B9 is the server's sentence.
export const BANNER = {
  B1: (harness: string, name: string) => `${harness} runs use ${name}, and you are not signed in to AWS.`,
  B3: (name: string, when: string) => `Your AWS sign-in for ${name} lapses ${when}.`,
  // Drawn for a token; "a key reads the same with 'key'".
  B4: (harness: string, name: string, token: boolean) =>
    `${harness} runs use ${name}, and you haven't added your ${token ? "token" : "key"}.`,
  B5: (harness: string) => `${harness} runs use your Claude subscription, and you're not signed in to Claude.`,
  B8: (n: number) => `${n} of your model connections need you.`,
  REVIEW: "Review",
} as const;

// The Your-model-connections strings the strip and the doors share (packet MP-D).
export const CONNECTIONS = {
  C6_LINE: (name: string) => `Your AWS sign-in for ${name} no longer works.`,
  SIGN_IN_CLAUDE: "Sign in to Claude",
  ADD_KEY: "Add your key",
  ADD_TOKEN: "Add your token",
} as const;

// The run's model provider in the run header (#543, decision 5). A provider
// deleted since the run chose it keeps its chip, marked removed.
export const RUN_FACTS = {
  PROVIDER: (name: string, removed: boolean) => `Model provider · ${name}${removed ? " (removed)" : ""}`,
} as const;

export const DOOR = {
  FOR: (name: string) => `For ${name}`,
} as const;

export const CLAUDE_DOOR = {
  TITLE: "Sign in to Claude",
  DESCRIPTION:
    "Signs you in with your own Claude subscription for your Claude Code runs. The sign-in runs in the terminal in this dialog.",
  SIGNED_IN_TOAST: "Signed in to Claude — your runs can use your subscription now",
} as const;

export const KEY_DOOR = {
  TITLE: (token: boolean, name: string) => `Add your ${token ? "token" : "key"} for ${name}`,
  FIELD: (token: boolean) => (token ? "Token" : "Key"),
  DESTINATION: (host: string) => `Sent to ${host}`,
  NOTE: "Stored for you alone. Wardyn injects it at the proxy, so it never enters the sandbox.",
  SAVE: "Save",
  CANCEL: "Cancel",
  REMOVE: "Remove",
  SAVED_TOAST: (token: boolean) => (token ? "Token saved" : "Key saved"),
} as const;
