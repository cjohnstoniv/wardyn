/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #659 Q2 — the Azure DevOps sign-in home banner: byte-exact strings for the
// six capture-callback reasons and the success redirect. Mock approved by the
// owner (ado-signin-error-659-packet.html, 2026-09-28) — Q1 (a banner on the
// home screen, reading the redirect exactly as it lands) and Q2 (identity_binding
// alone retries with prompt=select_account) are both decided there.
//
// A SEPARATE module from ado-entra-copy.ts (not a fourth block merged into its
// own §7 tables — the same reasoning push.ts gives for staying out of copy.ts):
// nothing outside this one banner reads these strings.
export const ADO_SIGNIN = {
  CONNECTED: "Azure DevOps is connected.",
  REASON: {
    consent_required:
      "Microsoft needs you to allow this connection before it can finish. Try again, and allow it when Microsoft asks.",
    interaction_required: "Microsoft needs you to sign in again to finish this connection. Try again.",
    exchange_failed: "Something went wrong finishing this connection with Microsoft. Try again.",
    identity_binding:
      "This sign-in didn't match the account you're signed in to Wardyn as, so nothing was connected. Try again, and sign in to Microsoft as yourself.",
    unusable_grant: "Microsoft didn't give Wardyn a connection it can keep using, so nothing was saved. Try again.",
    store_error: "Saving this connection failed, so nothing was connected. Try again.",
  },
  RETRY: "Try again",
} as const;
