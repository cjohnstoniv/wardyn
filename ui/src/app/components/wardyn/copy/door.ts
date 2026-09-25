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
  // §5.4's own page (packet MP-D, states C3-C10) — the page every person,
  // admins included, connects their own credential from. Not wired here: C9b
  // (a token removed by an admin's address change) and C11 (the admin-token
  // caller, which never mounts this page — model-access-context.tsx's own
  // comment). SetupProviderAccess carries no signal distinguishing "removed"
  // from "never stored", so C9b's own sentence is left unwired rather than
  // invented — see #541's report.
  TITLE: "Your model connections",
  LEDE: "Connect the model providers your admin set up. Your runs use your own sign-in or key.",
  FOR: (harnesses: string) => `For ${harnesses}`,
  SUMMARY_READY: "Model access · Ready",
  SUMMARY_NEEDS_YOU: "Model access · Needs you",
  SUMMARY_NOT_SET_UP: "Model access · Not set up by your admin",
  NOT_SIGNED_IN: "Not signed in",
  SIGNED_IN: "Signed in",
  EXPIRING: "Expiring",
  EXPIRING_LINE: (when: string) => `Sign in again before ${when}`,
  SIGNED_OUT: "Signed out",
  NO_KEY: "No key added",
  NO_TOKEN: "No token added",
  KEY_GOES_TO: (host: string) => `Your key goes to ${host}`,
  // vendor is fixed to "Anthropic" at every call site (packet MP-D's own
  // wording) — the packet's own report flags this as questionable on a row
  // that also serves a non-Anthropic agent; shipped as drawn.
  TOKEN_GOES_TO: (host: string, vendor: string) => `Your token goes to ${host} — not directly to ${vendor}`,
  YOUR_KEY: "Your key",
  YOUR_TOKEN: "Your token",
  SENT_TO: (host: string) => `Sent to ${host}`,
  REPLACE: "Replace",
  CLAUDE_AGING: "Your Claude sign-in is over 11 months old and may stop working — sign in again.",
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
