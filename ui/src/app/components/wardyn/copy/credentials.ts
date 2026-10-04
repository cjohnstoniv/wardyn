/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The admin "Stored credentials" page (packet F §4, design F-1/F-2) and its
// erase flow (packet F §5, design F-5/F-6), byte-for-byte from
// cs8-credentials-packet.html's strings table. GET /model-providers/credentials
// (CS-6, PR #1164) and DELETE /people/{principal}/credentials (CS-5) already
// ship; this page is the console for both.
//
// The nav label itself (canon NAV.CREDENTIALS, "Credentials") lives in
// lib/nav-copy.ts's CREDENTIALS_NAV_TITLE instead — the eager sidebar's own
// entry-chunk-size reason (see that file) — and is a DIFFERENT string from
// this page's own heading (INVENTORY.TITLE, below).

export const INVENTORY = {
  TITLE: "Stored credentials",
  LEDE: "Who holds a credential for each model provider, when it was added and when a run last used it. Values are never shown.",
  SUMMARY: (people: number, n: number) =>
    `${people} ${people === 1 ? "person holds" : "people hold"} ${n} credential${n === 1 ? "" : "s"}`,
  FILTER_ALL: "All providers",
  COL_PERSON: "Person",
  COL_PROVIDER: "Model provider",
  COL_STATE: "State",
  COL_STORE: "Stored in",
  // M5 S3: the key domain a person's next key is made in, and why (KEY_DOMAINS.SOURCE_*).
  COL_DOMAIN: "Key domain",
  COL_ADDED: "Added",
  COL_LAST_USED: "Last used",
  STATE_STORED: "Stored",
  STATE_EXPIRED: "Expired",
  EXPIRED_HINT: "Past its expiry. The daily sweep deletes it.",
  NEVER_USED: "Never",
  // credentialInventoryRow.store's three wire values, named for a person —
  // shown only when a page's rows name more than one store (a deployment
  // mid-migration between them).
  STORE: { pg: "Wardyn's database", vaultkv: "Vault", azurekv: "Key Vault" } as Record<string, string>,
  FOOTER_LOCAL: "Encrypted in Wardyn's database. Every use is in the Audit log.",
  // The store-mode footer (design §3): names the product and where its OWN
  // audit trail lives, never a host or vault name.
  FOOTER: (storage: "vault" | "key_vault") =>
    storage === "key_vault"
      ? "Stored in Key Vault. Every use is in the Audit log, and in your organisation's Key Vault logs."
      : "Stored in Vault. Every use is in the Audit log, and in your organisation's Vault audit device.",
  OPEN_AUDIT: "Open Audit",
  ERASE_ROW: "Erase credentials",
  ERASE_BY_EMAIL: "Erase someone's credentials",
  EMPTY_TITLE: "No one has stored a credential yet",
  EMPTY_BODY: "People add their own keys and sign-ins in Your account, in the user view. They show here once they do.",
  NO_PROVIDERS_BODY: "Add one in Settings. Each person then connects their own, and it shows here.",
  OPEN_SETTINGS: "Open Settings",
} as const;

export const ERASE = {
  TITLE: (person: string) => `Erase every credential ${person} stored?`,
  BODY: "This deletes all of them now: model keys and sign-ins, and secrets they added. Their runs stop using them within 10 minutes. Wardyn can't revoke them at AWS, Anthropic or your gateway — do that there, and in your identity provider.",
  RETENTION_LOCAL: "Backups keep a copy until they expire.",
  RETENTION_VAULT: "Your organisation's Vault snapshots keep a copy until they expire.",
  RETENTION_KEY_VAULT: "Your organisation's Key Vault may keep them recoverable for a while; the result says how long.",
  CONFIRM_LABEL: (person: string) => `Type ${person} to confirm`,
  BY_EMAIL_TITLE: "Erase someone's credentials",
  FIELD: "Email or subject",
  HINT: "For someone who isn't listed, for example because their only stored credentials are secrets.",
  CONFIRM: "Erase credentials",
  CANCEL: "Cancel",
  CLOSE: "Close",
  DONE: (n: number, person: string) => `Erased ${n} credential${n === 1 ? "" : "s"} for ${person}.`,
  DONE_NONE: (person: string) => `Nothing was stored for ${person}, so nothing was erased.`,
  DONE_AUDIT: "Recorded in the Audit log as credential.erase.",
  KEY_VAULT_RECOVERABLE: (days: number) =>
    `Your organisation's Key Vault keeps deleted secrets recoverable for ${days} days; ask its operators to purge them sooner.`,
  FAILED: (person: string) =>
    `The erase didn't finish, so some of ${person}'s credentials may still be stored. Try again — erasing twice is safe.`,
} as const;

// #1477: the read-only list of tokens an admin created for another person
// (console-085-packet, approved 2026-10-01, Q4–Q6). Strings are the packet's,
// character for character. The singular forms (CHIP/COUNT at 1) are the
// regular pluralisation of the packet's plural lines.
export const MINTED = {
  TITLE: "Tokens an admin created for someone else",
  CHIP: (n: number) => (n === 1 ? "1 still works" : `${n} still work`),
  COUNT: (n: number) =>
    n === 1
      ? "1 token was created by an admin for another person. It keeps working until revoked. Values are never shown."
      : `${n} tokens were created by an admin for another person. They keep working until revoked. Values are never shown.`,
  NOTE: "No one can create a token that acts as another person. They sign in and create their own.",
  EMPTY: "No admin has created a token for someone else.",
  COL_PERSON: "Person",
  COL_TOKEN: "Token",
  COL_CREATED_BY: "Created by",
  COL_ADDED: "Added",
  COL_LAST_USED: "Last used",
  NEVER: "Never",
  REVOKE: "Revoke",
  REVOKE_TITLE: (token: string, person: string) => `Revoke ${token} for ${person}?`,
  REVOKE_BODY: (person: string) => `It stops working now. ${person} can create their own after signing in.`,
  REVOKE_CANCEL: "Cancel",
  REVOKE_CONFIRM: "Revoke token",
  REVOKED_TOAST: "Token revoked.",
} as const;

// Key custody on the Credentials screen (mock packet M5, approved 2026-10-03; the strings are the
// packet's, character for character). The keys marked "not in M5" are the few words the dialog and
// the conflict chip need that the packet leaves undrawn; SUBMITTED_* are M3's CHANGES.SUBMITTED_*,
// unchanged, until the governance Changes tab hoists them into one block.
export const KEY_DOMAINS = {
  TITLE: "Key domains",
  LEDE: "Each domain is its own key in your key service. A person's keys are made in their domain, so whoever holds one domain's key can't open another's. Domains are declared in WARDYN_KEY_DOMAINS_FILE.",
  COL_DOMAIN: "Domain",
  COL_KEY: "Key",
  COL_BOOT: "At boot",
  PROVEN: "Proven",
  NOT_PROVEN: "Not proven",
  DEFAULT_NOTE: "default is this deployment's credential key.",
  ASSIGN_TITLE: "Assignments",
  COL_SUBJECT: "Person or group",
  PRECEDENCE: "A person's own assignment beats a group's, and a group's beats everyone. Anyone matching nothing uses default.",
  ASSIGN_CTA: "Assign a domain",
  FIELD_DOMAIN: "Domain",
  ASSIGN_HINT: "Applies to keys made from now on. Keys made earlier stay in the domain they were made in.",
  REMOVE: "Remove",
  REMOVE_CONFIRM: (subject: string) =>
    `Remove the domain assignment for "${subject}"? Their next key is made in whatever else matches them, or in default.`,
  OFF_NOTE: "Per-person keys are off for stored credentials (WARDYN_PRINCIPAL_KEYS), so domains apply to audit records only.",
  SOURCE_USER: "set for them",
  SOURCE_GROUP: (group: string) => `from ${group}`,
  SOURCE_ALL: "everyone",
  SOURCE_DEFAULT: "default",
  CONFLICT: "Two groups name different domains. No new key until one is removed.",
  // Not in M5: the dialog's subject-type choice (the wire's user | group | all) and the conflict
  // chip's visible word (its title is CONFLICT).
  TYPE_USER: "Person",
  TYPE_GROUP: "Group",
  TYPE_ALL: "Everyone",
  CONFLICT_CHIP: "Conflict",
  // M3 CHANGES.SUBMITTED_*.
  SUBMITTED_TITLE: "Submitted for approval",
  SUBMITTED_BODY: "Nothing has changed yet. It applies when someone else approves it, and expires if nobody does.",
  SUBMITTED_LINK: "View in Changes",
} as const;
