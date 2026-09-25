/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Sign-in first-contact copy canon (#457,
// docs/design/signin-first-contact-canon.md) — the frozen strings the
// sign-in screen (sign-in.tsx) renders, transcribed verbatim from that
// doc's table. NO_ROLE/EMAIL_VERIFIED_ABSENT/ROLE_CHECK_UNAVAILABLE/
// CLAIMS_OVERAGE used to be split between here and people-access-copy.ts
// (0.7 SSO Phase 3, §7.7) — #457 rewrote the whole screen's copy for
// honesty/no-jargon (no env var names, no "operator" jargon, an honest
// loading state before the daemon has answered which doors exist), so
// everything the screen reads now lives in the one module that owns it.
//
// Pure TS — no React, no fetch, no DOM. Same discipline as
// permissions-copy.ts: the component that consumes this adds NO copy of
// its own.
export const SIGNIN = {
  LEAD: "This console governs the agents on this host.",

  // First contact, before /healthz has answered even once which sign-in
  // doors exist (Q457-1: hide both doors while checking — no token field, no
  // SSO button, no claim about what's configured, rather than guess and
  // maybe be wrong for a moment).
  CHECKING: "Checking sign-in options…",
  // Q457-2: after three unanswered reads, say the checking is still going —
  // silence past that point reads as a hang, not a wait.
  STILL_CHECKING: "Still checking — Wardyn hasn't answered yet.",

  // Q457-3: names neither the demo token nor an env var — just what belongs
  // in the field and who has it.
  TOKEN_HINT:
    "Paste the admin token this Wardyn was started with — your Wardyn admin has it.",
  TOKEN_REJECTED: "That admin token was rejected. Check the value and try again.",
  UNREACHABLE_ERROR: "Wardyn isn't answering. Try again.",

  // The OIDC callback (internal/auth/oidc/oidc.go's CallbackHandler)
  // redirects a user-actionable login denial to "/?auth_error=<code>"
  // instead of dead-ending the browser on a bare http.Error text page.
  // These are the stable, machine-readable codes redirectAuthError sends;
  // keep in sync with oidc.go's authError* consts. #457 drops every env var
  // name and "operator" from the lot — a signed-in-or-refused human cannot
  // reach a chart value, and "your Wardyn admin" is who actually can.
  EMAIL_DOMAIN: "This email's domain isn't allowed to sign in here. Ask your Wardyn admin to allow it.",
  NO_ROLE:
    "Your account has no Wardyn role assigned. Ask your Wardyn admin to add you to a role mapping.",
  // claims_overage: the IdP withheld the groups/App Roles claim entirely (too
  // many groups to fit a sign-in token), so the role map matched nothing and
  // the server refused to let the default role decide. NOT a "try again" —
  // retrying sends the same token.
  CLAIMS_OVERAGE:
    "Your identity provider sent too many groups to list in a sign-in token, so Wardyn can't tell what access you should have — and won't guess. Ask your Wardyn admin to map your role by App Role or email instead. Trying again won't help.",
  EMAIL_VERIFIED_ABSENT:
    "Your identity provider doesn't send an email_verified claim at all (common on Entra ID), so Wardyn can't confirm your email on its own. Ask your Wardyn admin to map your role by App Role or group instead.",
  ROLE_CHECK_UNAVAILABLE: "Couldn't check your access — try again, or ask your Wardyn admin.",
  // 0.8 user types: the role map gives this person two types at the same
  // priority (Wardyn never picks one), or a type that doesn't exist.
  USER_TYPE_AMBIGUOUS:
    "Your account matches two user types with the same priority, so Wardyn won't pick one. Ask your Wardyn admin to give one of them a higher priority.",
  USER_TYPE_UNKNOWN:
    "Your account maps to a user type that doesn't exist. Ask your Wardyn admin to create it or change the mapping.",
  OIDC_TRANSIENT:
    "Your identity provider didn't respond in time. This is usually temporary — try signing in again.",
  OIDC_CONFIG:
    "Sign-in with your identity provider failed. Try again; if it keeps happening, ask your Wardyn admin to check the sign-in configuration.",
  AUTH_FAILED: "Sign-in failed. Try again, or ask your Wardyn admin.",
} as const;
