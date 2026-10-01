/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Azure DevOps personal-access-token copy (#1428, #1430): every string the
// approved "each person's own token" mock draws, character for character. Pure
// TS, no React and no fetch; the components that use it add no copy of their
// own, and ado-pat-copy.test.ts pins each string literally so a reworded
// sentence fails a gate instead of shipping.
//
// {placeholders} in the mock are functions here. The Go refusal constants that
// the server sends verbatim (the launch and admin refusals) are the same
// sentences; the console only re-states the ones it draws before the server is
// asked (a preflight line, an inline admin note), and renders anything else
// the server says as it says it.

export const ADO_PAT = {
  // ---- Admin: how people connect ----
  SECTION_TITLE: "How people connect to Azure DevOps",
  MODE_MINTED: "Wardyn creates a short-lived token for each run (recommended)",
  MODE_MINTED_HELP:
    "Each person connects once. For every run, Wardyn creates a personal access token in that person's name with only the access the run was given, and revokes it when the run ends.",
  MINTED_SETUP:
    "Wardyn creates each person's tokens with the app registration your people already sign in with. In Entra, add two Azure DevOps permissions to it (vso.pats and vso.pats_manage) and grant admin consent.",
  // Not in the approved mock: the one sentence added for the plan review's
  // finding F5 (a public console registration must keep its redirect on the
  // Web platform before a secret can be used). Rendered as its own line under
  // MINTED_SETUP, so the approved text above stays whole.
  MINTED_SETUP_REDIRECT: "Register the console redirect under the Web platform, then add the secret.",
  TOKEN_LIFE_LABEL: "Longest token life",
  TOKEN_LIFE_UNIT: "hours",
  TOKEN_LIFE_HINT:
    "A longer run gets a fresh token before this one expires. Keep it at or below your organisation's maximum token lifespan.",
  TOKEN_LIFE_RANGE: "Enter 1 to 168 hours.",
  MODE_BEARER: "Use each person's Entra sign-in",
  MODE_BEARER_HELP:
    "No token is created in Azure DevOps. The sign-in carries every Azure DevOps permission the person consented to; Wardyn checks each request against the run's access.",
  MODE_OWN: "Each person adds their own token",
  MODE_OWN_HELP:
    "For organisations that can't register an app. Wardyn checks the token is theirs and stops using it at the expiry they enter. Wardyn can't revoke it.",
  OWN_EXPIRY_LABEL: "Longest expiry",
  OWN_EXPIRY_UNIT: "days",
  OWN_EXPIRY_RANGE: "Enter 1 to 90 days.",
  RECOMMENDED_SETTINGS:
    "Recommended in Azure DevOps (Organization settings → Microsoft Entra): Restrict full-scoped personal access token creation — On. Enforce maximum personal access token lifespan — On.",

  // ---- Admin: check organisation settings ----
  CHECK_BUTTON: "Check organisation settings",
  CHECK_TITLE: "Organisation settings",
  CHECK_LAST: (time: string) => `Last checked ${time}`,
  CHECK_CHIP: (time: string) => `Checked ${time}`,
  CHECK_PERMS_OK: "Your app registration has both Azure DevOps token permissions.",
  CHECK_PERMS_MISSING:
    "Your app registration doesn't have the Azure DevOps token permissions yet. Add vso.pats and vso.pats_manage and grant admin consent.",
  CHECK_LIFESPAN_ON: (hours: number) => `Maximum token lifespan is on, and tokens of ${hours} hours are allowed.`,
  CHECK_LIFESPAN_OFF:
    "Maximum token lifespan is off in Azure DevOps. A stolen connection could create tokens that last up to a year. Turn it on under Organization settings → Microsoft Entra.",

  // Round 2: the lifespan probe got an answer that is neither "on" nor "off".
  CHECK_LIFESPAN_UNKNOWN: (error: string) =>
    `Wardyn couldn't tell whether your organisation's maximum token lifespan is on. Azure DevOps answered: ${error}.`,

  // ---- Admin: refusals ----
  NO_CLIENT_SECRET:
    "Per-run tokens need this row to use Wardyn's own sign-in app, and that app to have a client secret. Name Wardyn's app here and set WARDYN_OIDC_CLIENT_SECRET, or choose another way to connect.",
  // {n} is the longest life the organisation check saw accepted; unknown reads
  // "Lower it."
  LIFESPAN_REFUSAL: (hours?: number) =>
    `Longest token life is above your organisation's maximum token lifespan. ${
      hours ? `Lower it to ${hours} hours or less.` : "Lower it."
    }`,
  // Admin banner from the admin's own organisation check ({person} is the admin who ran it).
  POLICY_BANNER: (person: string) =>
    `Azure DevOps refused to create a token for ${person}: your organisation restricts who can create personal access tokens. Add the people who use Wardyn to that policy's allow list, or switch to Entra sign-in.`,
  POLICY_BANNER_BUTTON: "Switch to Entra sign-in",
  BEARER_WITH_TOKEN_PERMS:
    "Your app registration holds the Azure DevOps token permissions, so Entra sign-in can't be used for runs: its tokens would let a run create tokens. Remove vso.pats and vso.pats_manage from the app first, or keep Wardyn creating a token for each run.",

  // ---- Member: the connection card ----
  MEMBER_NOT_CONNECTED:
    "Connect once so Wardyn can create a short-lived token for each of your runs. Each token has only that run's access and is revoked when the run ends.",
  MEMBER_CONNECT: "Connect Azure DevOps",
  MEMBER_CONNECTED:
    "Tokens are created in your name, one per run, and revoked when it ends. Azure DevOps lists them under Personal access tokens as 'Wardyn run …'.",
  MEMBER_LAST_TOKEN: (created: string, revoked: string) => `Last token: created ${created}, revoked ${revoked}.`,
  MEMBER_DISCONNECT: "Disconnect",
  DISCONNECT_TITLE: "Disconnect Azure DevOps?",
  DISCONNECT_BODY: "Disconnecting revokes the tokens of any of your runs in progress. They lose Azure DevOps access.",
  DISCONNECT_CANCEL: "Cancel",
  CHIP_NOT_CONNECTED: "Not connected",
  CHIP_CONNECTED: "Connected",
  CHIP_SIGN_IN_AGAIN: "Sign in again",
  CHIP_BLOCKED: "Blocked by your organisation",
  // What a member reads when the row cannot create tokens for want of the
  // admin's setup.
  MEMBER_NEEDS_ADMIN: "Your administrator needs to finish setting up Azure DevOps before runs can use it.",
  SIGN_IN_AGAIN_BODY: "Your organisation asked you to sign in again before Wardyn can create tokens.",
  BLOCKED_BODY:
    "Your organisation doesn't let you create personal access tokens. Ask an Azure DevOps administrator to add you to the allow list.",

  // ---- Launch ----
  LAUNCH_NOT_CONNECTED: "Connect Azure DevOps once before launching; Wardyn creates the run's token from that connection.",
  LAUNCH_POLICY_REFUSED:
    "Azure DevOps refused to create a token for this run: your organisation restricts who can create personal access tokens. Ask an Azure DevOps administrator to add you to the allow list.",
  // New Run's access line: PREFIX + the capabilities (bold) + SUFFIX.
  NEWRUN_LINE_PREFIX: "Azure DevOps: a token for this run with ",
  NEWRUN_LINE_SUFFIX: ". It's revoked when the run ends.",

  // ---- Run page ----
  RUN_TOKEN_TITLE: "Azure DevOps",
  RUN_TOKEN_LINE: (created: string, expires: string) => `Azure DevOps token: created ${created} · expires ${expires}`,
  RUN_TOKEN_LINE_REVOKED: (created: string, expires: string, revoked: string, reason: string) =>
    `Azure DevOps token: created ${created} · expires ${expires} · revoked ${revoked} (${reason})`,
  // A token is never revoked early, so one replaced by a renewal or a widening,
  // or closed at its own expiry, has no revoke time to name. It reads its expiry
  // and why it ended.
  RUN_TOKEN_LINE_NOTE: (created: string, expires: string, note: string) =>
    `Azure DevOps token: created ${created} · expires ${expires} (${note})`,
  RUN_TOKEN_NOTE: { renewed: "renewed", access_added: "access added", expired: "expired" },
  // The reasons the server writes (ado_run_pats.revoke_reason). "expired" is not
  // one: see RUN_TOKEN_NOTE.
  REVOKE_REASON: {
    run_end: "run ended",
    kill: "run stopped",
    pause: "paused",
    drift: "access changed",
    disconnect: "disconnected",
    sweep: "cleaned up after a restart",
    upstream_401: "rejected by Azure DevOps",
    offboarding: "person removed",
  } as Record<string, string>,
  RUN_PAUSED: "Paused: this run's token was revoked. A new one is created when the run resumes.",
  RUN_RENEWAL_FAILED: (expires: string) =>
    `Wardyn couldn't renew this run's token, so it stops working at ${expires}. Sign in to Azure DevOps again to keep this run going.`,
  RUN_REVOKE_FAILED: (expires: string) =>
    `Wardyn couldn't revoke this run's token. It expires at ${expires}; revoke it in Azure DevOps under Personal access tokens.`,
  RUN_ACCESS_ADDED: (time: string, capability: string) => `Access added ${time}: ${capability} (new token)`,
  APPROVAL_WIDENS: "Approving adds this access to the run's token for the rest of this run, even for a one-time approval.",

  // ---- Own token ----
  OWN_ADD_CTA: "Add your personal access token",
  OWN_DIALOG_TITLE: "Add your personal access token",
  OWN_DIALOG_LEAD: (org: string, days: number) =>
    `In Azure DevOps, create a token for ${org} only, with these scopes and an expiry within ${days} days, then paste it here.`,
  OWN_OPEN_TOKENS: "Open Azure DevOps tokens",
  OWN_FIELD_TOKEN: "Token",
  OWN_FIELD_EXPIRES: "Expires on",
  OWN_DIALOG_ADD: "Add token",
  OWN_DIALOG_CANCEL: "Cancel",
  OWN_MISMATCH: "This token belongs to a different Azure DevOps account than yours.",
  OWN_TOO_LONG: (days: number) => `This token expires after the ${days}-day limit your administrator set.`,
  OWN_REJECTED: "Azure DevOps didn't accept this token.",
  OWN_CHIP_EXPIRING: (days: number) => `Expires in ${days} days`,
  OWN_CHIP_EXPIRED: "Expired",
  OWN_EXPIRING_LINE: (org: string, date: string) => `Your token for ${org} expires on ${date}.`,
  OWN_CHIP_REFUSED: "Refused",
  OWN_REFUSED_LINE: (date: string, expiry: string) => `Azure DevOps refused this token on ${date}, before it expires on ${expiry}. Replace it.`,
  OWN_REPLACE: "Replace token",
  OWN_EXPIRED_BODY: "Your runs can't reach Azure DevOps until you add a new token.",
  OWN_SERVER_TITLE: "Azure DevOps Server",
  OWN_SERVER_NOTE: "Git only. Azure DevOps Server has no Entra sign-in.",

  // ---- After the upgrade ----
  CONVERTED_CHECKLIST: "Azure DevOps no longer uses one shared token. Choose how people connect.",
  CONVERTED_CHOOSE: "Choose",
  CONVERTED_NOTE:
    "Azure DevOps no longer uses one shared token. Choose how people connect, then turn this row on. Until then, runs can't clone from this organisation.",
  CONVERTED_CEILING: (names: string) => `Read-only, carried over from the retired token: ${names}.`,
  CONVERTED_SAVE_ON: "Save and turn on",
} as const;

// Getting started's chip row prefixes every Azure DevOps chip with the
// connection's name, so a person's own token reads in Settings' chip words after
// it (the approved own-token chip packet). One helper over OWN_CHIP_* and
// CHIP_CONNECTED: rewording a word there reaches both screens.
export function gettingStartedOwnChip(kind: "connected" | "expiring" | "refused" | "expired", days = 0): string {
  const word = {
    connected: ADO_PAT.CHIP_CONNECTED,
    expiring: ADO_PAT.OWN_CHIP_EXPIRING(days),
    refused: ADO_PAT.OWN_CHIP_REFUSED,
    expired: ADO_PAT.OWN_CHIP_EXPIRED,
  }[kind];
  return `Azure DevOps · ${word}`;
}
