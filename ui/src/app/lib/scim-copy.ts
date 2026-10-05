/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The SCIM card's frozen copy (0.8.6 mock packet M5, approved 2026-10-03: every string here is canon).
// Literals (env vars, the endpoint) are plain text in the strings; the card decides which render mono.
import { DRIVES } from "./user-drives-copy";

/** The leaver runbook, OPERATIONS.md's "Leavers and SCIM". */
export const SCIM_RUNBOOK_URL = "https://github.com/cjohnstoniv/wardyn/blob/main/docs/OPERATIONS.md#leavers-and-scim";

export const SCIM = {
  TITLE: "SCIM provisioning",
  SUMMARY_ON: (deactivated: number, unfinished: number) => `${deactivated} deactivated · ${unfinished} unfinished`,
  SUMMARY_OFF: "Not set up",
  LEAD: "Your identity provider tells Wardyn when someone leaves or changes groups. Wardyn then cuts their sessions, tokens, SSH keys and runs, and erases their stored credentials after the purge delay.",
  OFF_BODY: "Not set up. Someone removed in your identity provider keeps their sessions, tokens and runs here until an admin revokes them.",
  OFF_HOW: "Set WARDYN_SCIM_TOKEN on wardynd, then point your Entra ID provisioning at the endpoint below.",
  OFF_LINK: "Leaver runbook",
  ENDPOINT: "Endpoint",
  TOKEN_LABEL: "Token last matched",
  TOKEN_PRIMARY: "Primary token",
  TOKEN_NEXT: "Next token",
  TOKEN_NONE: "None yet",
  TOKEN_NEXT_HINT:
    "Your identity provider is using the next token. Move it to WARDYN_SCIM_TOKEN and unset WARDYN_SCIM_TOKEN_NEXT to finish the rotation.",
  PURGE_DELAY: "Purge delay",
  PURGE_DAYS: (n: number) => `${n} day${n === 1 ? "" : "s"}`,
  PURGE_OFF: "Off — only a delete from your identity provider purges",
  WORKSPACES: "Workspaces on purge",
  WS_REASSIGN: "Handed to the operator",
  WS_KEEP: "Left as they are",
  DEACTIVATED_TITLE: "Deactivated",
  COL_PERSON: "Person",
  COL_SINCE: "Deactivated",
  COL_PURGE: "Purge scheduled",
  PURGE_NOT_SCHEDULED: "Not scheduled",
  DEACTIVATED_EMPTY: "No one is deactivated.",
  PENDING_TITLE: "Unfinished deprovisioning",
  COL_STEP: "Step",
  COL_ERROR: "Last error",
  // The ledger's step keys (internal/api scim_suspend.go, scim_purge.go); a key not named here shows as it is.
  STEP: {
    cutoff: "Cut sessions",
    sweep: "Revoke tokens and SSH keys",
    kill_run: "Stop runs",
    erase: "Erase credentials",
    workspaces: "Reassign workspaces",
    // A group removal's steps (internal/store store_scim_groups.go).
    sessions: "Group removal: cut sessions",
    tokens: "Group removal: revoke tokens",
    audit: "Group removal: record",
  } as Record<string, string>,
  PENDING_HINT:
    "Wardyn resumes from the failed step each time your identity provider retries. To cut someone off now, erase their credentials under Credentials and kill their runs.",
  PENDING_EMPTY: "Nothing unfinished.",
  DRIVES_TITLE: "Drives to reclaim",
  COL_DRIVE: "Drive",
  COL_PURGED: "Purged",
  DRIVES_HINT: `A purge lists these and leaves them in place. Reclaim each one as its drive's "${DRIVES.COL_RECLAIM}" setting says.`,
  DRIVES_EMPTY: "No drives to reclaim.",
} as const;
