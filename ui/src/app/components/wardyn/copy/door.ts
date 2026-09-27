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
  // Packet F §1: a Replace open (a credential is already stored) gets its own
  // title — the Add title would claim there is nothing there yet.
  TITLE_REPLACE: (token: boolean, name: string) => `Replace your ${token ? "token" : "key"} for ${name}`,
  FIELD: (token: boolean) => (token ? "Token" : "Key"),
  DESTINATION: (host: string) => `Sent to ${host}`,
  NOTE: "Stored for you alone. Wardyn injects it at the proxy, so it never enters the sandbox.",
  SAVE: "Save",
  CANCEL: "Cancel",
  REMOVE: "Remove",
  SAVED_TOAST: (token: boolean) => (token ? "Token saved" : "Key saved"),
  // The PUT's 503 when the configured store answered unavailable (packet F,
  // server canon: keyDoorSaveUnavailable, internal/api/model_provider_credentials.go).
  // Kept here too so a test can assert the dialog shows the server's own
  // sentence verbatim rather than a client-invented one.
  SAVE_UNAVAILABLE:
    "Wardyn couldn't reach the service that stores credentials, so this wasn't saved. Nothing changed. Try again in a moment.",
} as const;

// The write-only chip on the key door's field label (packet F §1) — was
// "write-only" (lower-case) on /secrets; sentence case, and shared so the two
// surfaces can never say it two ways.
export const WRITE_ONLY = {
  CHIP: "Write-only",
  TOOLTIP: "Write-only: the value can be replaced or removed, but never read back — not even by you.",
} as const;

// The key door's three-line notice (packet F §1). Line 1 is KEY_DOOR.NOTE,
// unchanged; KEK is the store-mode line (design §3, F-4).
export const CRED_NOTICE = {
  STORED_HINT: (token: boolean) =>
    `Your ${token ? "token" : "key"} is stored and can't be shown. Paste a new one to replace it.`,
  LOCAL: "Encrypted in Wardyn's database with a key this deployment holds.",
  KEY_SERVICE: (product: string) =>
    `Encrypted in Wardyn's database. The key that unlocks it is held in ${product} and never leaves it.`,
  // The store-mode line (design §3, F-4): names the product, never a host,
  // path or vault name.
  KEK: (product: string) => `Stored in your organisation's ${product}. Wardyn keeps no copy and no key.`,
  ADMINS: "Admins can see that you stored it, when, and when a run last used it — never the value.",
} as const;

// The confirm dialog Remove now opens (packet F §3, CONSOLE-RULES §6): today
// it deletes on the first click.
export const REMOVE_CONFIRM = {
  TITLE: (token: boolean, name: string) => `Remove your ${token ? "token" : "key"} for ${name}?`,
  BODY: "It's deleted now, and runs already going stop using it within 10 minutes. Backups keep a copy until they expire.",
  BODY_VAULT:
    "It's deleted from your organisation's Vault now, and runs already going stop using it within 10 minutes.",
  BODY_KEY_VAULT:
    "It's deleted from your organisation's Key Vault now, which keeps deleted secrets recoverable for a set time. Runs already going stop using it within 10 minutes.",
  UPSTREAM: (host: string) => `It still works at ${host} until you revoke it there.`,
  CONFIRM: "Remove",
  CANCEL: "Cancel",
  REMOVED_TOAST: (token: boolean) => (token ? "Token removed" : "Key removed"),
} as const;
