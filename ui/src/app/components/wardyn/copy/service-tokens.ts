/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Your account: the Invitations card, the "Service identities I sponsor" card and the show-once token dialog
// both end in (#1508, mock round M-SVC v2, approved 2026-10-12). Every string is the mock's, character for
// character; service-tokens-copy.test.ts freezes them. A function key renders the mock's own template.
//
// The server's refusals are shown as the server says them. They are not here.

const plural = (n: number, unit: string) => `${n} ${unit}${n === 1 ? "" : "s"}`;

/** "in 2 days", "12 hours ago", "just now": the mock's relative stamp (whole units, no abbreviations). */
export function inWords(iso: string, now: number = Date.now()): string {
  const d = Date.parse(iso) - now;
  const a = Math.abs(d);
  const H = 3_600_000;
  const D = 86_400_000;
  let s: string;
  if (a < 60_000) return "just now";
  // A count that rounds up to the next unit's size is said in that unit: 59.5 minutes is "1 hour", not "60 minutes".
  if (a < H && Math.round(a / 60_000) < 60) s = plural(Math.round(a / 60_000), "minute");
  else if (a < D && Math.round(a / H) < 24) s = plural(Math.round(a / H), "hour");
  else s = plural(Math.round(a / D), "day");
  return d >= 0 ? `in ${s}` : `${s} ago`;
}

const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

/** "11 Nov 2026", in UTC. */
export function dayOf(iso: string): string {
  const d = new Date(iso);
  return `${d.getUTCDate()} ${MONTHS[d.getUTCMonth()]} ${d.getUTCFullYear()}`;
}

/** "30 days", "6 hours", "45 minutes": a lifetime in seconds, in the largest whole unit. */
export function lifetimeWords(seconds: number): string {
  if (seconds % 86_400 === 0) return plural(seconds / 86_400, "day");
  if (seconds % 3_600 === 0) return plural(seconds / 3_600, "hour");
  return plural(Math.round(seconds / 60), "minute");
}

export const INVITATIONS = {
  TITLE: "Invitations",
  SUMMARY: (n: number) => `${n} waiting`,
  LEDE: "An admin invited you to create an API token. Only you can claim an invitation, signed in as yourself.",
  FROM: (createdBy: string) => `Invitation from ${createdBy}`,
  EXPIRES: (when: string) => `Expires ${when}`,
  TOKEN_NAME: "Token name:",
  LASTS: (life: string) => `Lasts at most ${life}`,
  REVOKES_LEAD: "Claiming it also revokes",
  REVOKES_TAIL: ", a token an admin created for you.",
  CLAIM: "Claim",
  CLAIM_LABEL: (createdBy: string) => `Claim invitation from ${createdBy}`,
  UNAVAILABLE_TITLE: "Invitations are unavailable",
  UNAVAILABLE_BODY: "This server does not manage invitations yet.",
  LOADING: "Loading invitations",
  LOAD_ERROR_LABEL: "Invitations",
  CLAIMED_TOAST: "Invitation claimed.",
  /** The token's name when the replaced token cannot be read. */
  REPLACES_UNKNOWN: "a token",
} as const;

export const SERVICES = {
  TITLE: "Service identities I sponsor",
  SUMMARY: (active: number, total: number) => `${active} active${total > active ? `, ${total - active} not active` : ""}`,
  LEDE: "A service identity is automation that runs under its own name, not yours. As its sponsor you create its tokens, see its runs and can stop them. An admin creates, suspends and retires it.",
  STATE_ACTIVE: "Active",
  STATE_SUSPENDED: "Suspended",
  STATE_RETIRED: "Retired",
  STATE_SPONSOR_INACTIVE: "Sponsor inactive",
  HINT_SUSPENDED: "Suspended by an admin. Its tokens are refused until an admin resumes it.",
  HINT_RETIRED: "Retired. Its tokens were revoked.",
  HINT_SPONSOR_INACTIVE: "Your sign-in is not active as its sponsor, so its tokens are refused.",
  MINT: "Mint a token",
  MINT_LABEL: (name: string) => `Mint a token for ${name}`,
  SHOW_TOKENS: "Show tokens",
  HIDE_TOKENS: "Hide tokens",
  SHOW_TOKENS_LABEL: (name: string) => `Show tokens of ${name}`,
  HIDE_TOKENS_LABEL: (name: string) => `Hide tokens of ${name}`,
  COL_TOKEN: "Token",
  COL_EXPIRES: "Expires",
  COL_LAST_USED: "Last used",
  TABLE_LABEL: (name: string) => `Tokens of ${name}`,
  CREATED: (when: string) => `Created ${when}`,
  EXPIRED: "Expired",
  REVOKED: "Revoked",
  EXPIRED_AT: (when: string) => `Expired ${when}`,
  REVOKED_AT: (when: string) => `Revoked ${when}`,
  NEVER: "Never",
  NO_TOKENS: "No tokens yet.",
  NO_TOKENS_MINT: (name: string) => `Mint one to let ${name} start runs.`,
  REVOKE: "Revoke",
  REVOKE_LABEL: (token: string, name: string) => `Revoke ${token} of ${name}`,
  REVOKE_TITLE: (token: string, name: string) => `Revoke ${token} for ${name}?`,
  REVOKE_BODY: (name: string) => `It stops working now. Anything using it as ${name} is refused. You can mint another.`,
  UNAVAILABLE_TITLE: "Service identities are unavailable",
  UNAVAILABLE_BODY: "This server does not manage service identities yet.",
  LOADING: "Loading service identities",
  LOADING_TOKENS: "Loading tokens",
  LOAD_ERROR_LABEL: "Service identities",
  MINTED_TOAST: "Token minted.",
} as const;

export const TOKEN_DIALOG = {
  CLAIM_TITLE: "Claim your token",
  CLAIM_LEDE: "This invitation is for you alone. The token acts as you, with your role, and is shown once.",
  CLAIM_WARNING_LEAD: "Claiming also revokes",
  CLAIM_WARNING_TAIL: ". Anything still using it stops working now.",
  MINT_TITLE: (name: string) => `Mint a token for ${name}`,
  MINT_LEDE: (name: string) => `The token acts as ${name}, not as you. It is shown once.`,
  MINT_WARNING: (name: string) => `Anyone who holds the token can do what ${name} is allowed to do, which may be more than you can.`,
  NAME_LABEL: "Token name",
  NAME_HINT: "Helps you tell your tokens apart.",
  NAME_REQUIRED: "Name the token.",
  EXPIRES_LABEL: "Expires in",
  OPT_7_DAYS: "7 days",
  OPT_30_DAYS: "30 days",
  OPT_90_DAYS: "90 days",
  OPT_1_YEAR: "1 year",
  OPT_LONGEST: "Longest the server allows",
  CLAIM_HINT_LIMITED: (life: string) => `The invitation limits the token to ${life}.`,
  CLAIM_HINT_OPEN: "The server may set a shorter limit.",
  MINT_HINT: "At most 90 days, or less if the server's limit is lower.",
  CANCEL: "Cancel",
  CLOSE: "Close",
  CLAIM_SUBMIT: "Claim token",
  CLAIM_BUSY: "Claiming…",
  MINT_SUBMIT: "Mint token",
  MINT_BUSY: "Minting…",

  REVEAL_TITLE: "Your new token",
  REVEAL_WARNING: "You won't see this token again. Copy it now and keep it somewhere safe.",
  REVEAL_UNCOPIED: "You haven't copied it yet. Once you close this, it can't be shown again.",
  TOKEN_LABEL: "Token",
  DETAIL_NAME: "Name",
  DETAIL_ACTS_AS: "Acts as",
  ACTS_AS_YOU: (role: string) => `you (${role})`,
  DETAIL_EXPIRES: "Expires",
  EXPIRY: (day: string, when: string) => `${day} (${when})`,
  NEVER: "Never",
  CLAMPED: "The server's limit is shorter than the one you chose, so it expires sooner.",
  COPY: "Copy token",
  COPIED: "Copied",
  COPIED_STATUS: "Token copied to the clipboard.",
  DONE: "Done",
  CLOSE_UNCOPIED: "Close without copying",
} as const;

/** The role the new token carries, as the reveal names it: "you (User)". */
export const ROLE_WORDS: Record<string, string> = { user: "User", security_admin: "Security admin", admin: "Super admin" };
