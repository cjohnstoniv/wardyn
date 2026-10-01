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
    `${harness} runs use ${name}, and no ${token ? "token" : "key"} is available.`,
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
  NO_KEY: "No key available",
  NO_TOKEN: "No token available",
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
  // #592 (CS-8) — the meta line every row that HOLDS a credential gets, from
  // this caller's own provider_access row (added_at/last_used_at); a row with
  // nothing stored gets no line at all (cs8-credentials-packet.html §2).
  // "Last used" is relative, to the minute, with the exact time on hover —
  // the same convention every other relative stamp in the console uses.
  ADDED: (date: string) => `Added ${date}`,
  LAST_USED: (when: string) => `Last used ${when}`,
  NOT_USED: "Not used by a run yet",
} as const;

// The run's model provider in the run header (#543, decision 5). A provider
// deleted since the run chose it keeps its chip, marked removed.
// LAUNCHED_VIA (#1234) is the line under the header bar for a run a registered
// portal launched on its owner's behalf; the fallback is for a portal whose
// name the server could not give.
export const RUN_FACTS = {
  PROVIDER: (name: string, removed: boolean) => `Model provider · ${name}${removed ? " (removed)" : ""}`,
  LAUNCHED_VIA: (name: string) => `Launched via ${name}`,
  LAUNCHED_VIA_FALLBACK: "Launched via a portal",
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
