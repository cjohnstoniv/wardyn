/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// §7.7 — SIGNIN, the reworded auth_error arms sign-in.tsx's authErrorMessage
// renders. Only these three keys: the other auth_error codes are unchanged
// and stay authored directly in sign-in.tsx (out of scope this round).
//
// Split out of people-access-copy.ts (#498): sign-in.tsx is the ONLY consumer
// and it is eager (rendered before the router, outside React.lazy), while
// people-access-copy.ts's other tables (PEOPLE, GUARD, ACCESS_ERROR, PREVIEW,
// ACCESS_STATE) belong to the People step and the role-mappings editor, both
// lazy. One static importer was enough to pull that whole module — every
// export any lazy screen reads from it too — into the entry chunk; people-
// access-copy.ts re-exports SIGNIN so `from "./people-access-copy"` still
// works for anything that imported it that way.
export const SIGNIN = {
  NO_ROLE:
    "Your account has no Wardyn role assigned. Ask your Wardyn admin to add you to a role mapping (WARDYN_OIDC_ROLE_MAP or WARDYN_OIDC_OPERATOR_EMAILS, or the equivalent on the People step).",
  EMAIL_VERIFIED_ABSENT:
    "Your identity provider doesn't send an email_verified claim at all (common on Entra ID), so this console can't confirm the email on its own. Ask your Wardyn admin to map your role by App Role or group instead (WARDYN_OIDC_ROLE_MAP, or the People step).",
  ROLE_CHECK_UNAVAILABLE: "Couldn't check your access — try again, or contact your admin.",
  // DRAFT (M2 canon pending): role is three-valued (0.7 SSO Phase 3) — this
  // sentence must name all three; naming only "admin or member" tells a
  // security admin they'd sign in as one of the two when neither is true.
  ROLE_SOURCE:
    "Your role — admin, security admin or member — comes from your SSO role assignment. Everyone is an admin only when neither a role map nor the operator allowlist is set.",
} as const;
