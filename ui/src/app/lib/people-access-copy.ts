/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// People-access copy canon (0.7 SSO, Phase 3 UI) — the frozen canonical-strings
// table from docs/design/people-access-prompt.md §7, plus its Adjudication
// section, transcribed verbatim. The People step's role-mappings editor
// (setup/access-panel.tsx, step-bodies.tsx's DeploymentStep multi-user branch)
// and the sign-in screen's reworded auth_error arms (sign-in.tsx) read these
// instead of retyping the copy, so the shipped wording can't drift from the
// reviewed mock (docs/design/people-access-mock/index.html).
//
// Pure TS — no React, no fetch, no DOM. Same discipline as permissions-copy.ts
// (permissions.tsx:11-13): the component that consumes this adds NO copy of
// its own.
//
// Backtick-mono rule (§7, header note): a backticked substring inside a frozen
// string here (an env var name) is plain text in this module — the mono
// styling is a DISPLAY concern, applied by the consuming component (see
// access-panel.tsx's withMono helper), not baked into the string itself.
//
// Casing rule (§7.2): a {role}/{defaultRole} interpolated INSIDE a sentence is
// lowercase ("admin"/"member") — PEOPLE.ROLE_ADMIN/ROLE_MEMBER below are the
// ONLY title-case forms (the chip/label shape) and are never interpolated
// into a sentence as-is.

export const PEOPLE = {
  TABLE_TITLE: "Role mappings",
  TABLE_LEAD:
    "A value — an Entra App Role, a groups-claim entry, or an email — mapped to admin or member.",
  EFFECT_NOTE:
    "Takes effect at next sign-in. A person already signed in keeps the role they were given until then.",
  COL_VALUE: "Value",
  COL_ROLE: "Role",
  COL_SOURCE: "Source",
  COL_ADDED: "Added",
  ROLE_ADMIN: "Admin",
  // The 0.7 third tier, frozen in docs/design/governance-prompt.md §7.9 as
  // DIRECTORY.ROLE_SECURITY_ADMIN. It lives HERE, next to ROLE_ADMIN/
  // ROLE_MEMBER, because §7.9 says so in as many words ("one string for all
  // three" — picker option, table chip, mapped-role label — "next to
  // PEOPLE.ROLE_ADMIN / PEOPLE.ROLE_MEMBER, which is why it is title case").
  // When Phase 6's governance-copy.ts transcribes §7.9's DIRECTORY block for
  // the combobox, it re-exports this constant rather than retyping the
  // string: two homes for one frozen label is how they drift.
  ROLE_SECURITY_ADMIN: "Security admin",
  ROLE_MEMBER: "Member",
  SOURCE_CHART: "From your chart",
  SOURCE_CONSOLE: "Console",
  CHART_HINT: "Edit in your chart values.",
  ADDED_CHART_NA: "—",
  SHADOWED_BADGE: "Shadowed by your chart",
  SHADOWED_BODY:
    "Your chart now maps this value too, and the chart always wins. This row is stored but has no effect until you remove the chart entry or delete this row.",
  SHADOWED_OPERATOR_BADGE: "Shadowed by your operator allowlist",
  SHADOWED_OPERATOR_BODY:
    "This value is on your chart's WARDYN_OIDC_OPERATOR_EMAILS and always resolves to admin. The row is stored but has no effect until the allowlist entry or this row is removed.",
  EMAIL_KEY_BADGE: "Unverified claim",
  // Post-adjudication canon addition (backend review round): the
  // email_verified clause only applies when WARDYN_OIDC_EMAIL_DOMAINS is
  // UNSET (email_domains_configured=false on GET /access) — the claim is
  // untrue on a deployment that sets it, so the clause is dropped rather than
  // rendered false. The doc names the CONDITION, not this exact alternate
  // wording; flagged in this campaign's report as the one string not given
  // verbatim.
  EMAIL_KEY_BODY: (emailDomainsConfigured: boolean) =>
    emailDomainsConfigured
      ? "This is an email-keyed mapping. Prefer an App Role or group instead."
      : "This is an email-keyed mapping riding an unverified IdP claim (WARDYN_OIDC_EMAIL_DOMAINS is unset, so email_verified isn't enforced). Prefer an App Role or group instead.",
  ADD_TITLE: "Add a mapping",
  ADD_CTA: "Add mapping",
  FIELD_VALUE: "Value",
  VALUE_HINT:
    "An Entra App Role or groups-claim value exactly as your identity provider sends it in the token, or an email address. Matched case-insensitively.",
  FIELD_ROLE: "Role",
  DELETE: "Delete",
  DELETE_CONFIRM: (value: string) =>
    `Delete the mapping for "${value}"? At their next sign-in, they fall through to whatever the rest of your map resolves to.`,
  CANCEL: "Cancel",
  EMPTY_TITLE: "No mappings added here",
  EMPTY_BODY:
    "Add one for a person or group. If your chart already maps someone, they're listed above and don't need a row here.",
  DEFAULTS_TITLE: "Defaults",
  DEFAULT_ROLE_LABEL: "Default role",
  DEFAULT_ROLE_UNSET: "Unset — anyone matching no mapping is denied at sign-in.",
  DEFAULT_ROLE_HINT:
    "Applies to anyone who matches no mapping above. Set in your chart's WARDYN_OIDC_DEFAULT_ROLE.",
  OPERATOR_EMAILS_LABEL: "Operator allowlist",
  OPERATOR_EMAILS_HINT:
    "Always admin, on top of any mapping above. Set in your chart's WARDYN_OIDC_OPERATOR_EMAILS.",
  OPERATOR_EMAILS_EMPTY: "None set.",
  IDP_NOTE:
    'Creating people and groups, and assigning Entra App Roles, happens in your identity provider — mapping a role here only tells Wardyn what to do with a value your IdP already sends. "Assignment required" on the app registration is Entra\'s gate, not this one\'s.',
} as const;

// §7.3 — posture-flip guards. FIRST_ROW_* fires on the first console row added
// (chart+console both empty); LAST_ROW_* fires on the last console row
// deleted (chart empty, this is the only remaining row) — see access-panel.tsx
// for how {before}/{after} are derived (swapped between the two directions;
// §2.1/§7.3 in the prompt doc).
export const GUARD = {
  FIRST_ROW_TITLE: "This first mapping changes who gets in",
  FIRST_ROW_BODY: (before: string, after: string) =>
    `Today, anyone matching no mapping signs in as ${before}. After this mapping, they will ${after} at their next sign-in.`,
  LAST_ROW_TITLE: "Removing the last mapping changes who gets in",
  LAST_ROW_BODY: (before: string, after: string) =>
    `Today, anyone matching no mapping ${before}. After this removal, they will ${after} at their next sign-in.`,
  ALLOWLIST_NOTE: "People on your operator allowlist stay admins either way.",
  GUARD_ACK_LABEL: "I understand this changes who can sign in.",
  FIRST_ROW_CONFIRM: "Add mapping",
  LAST_ROW_CONFIRM: "Delete mapping",
} as const;

// §7.4 — collision, lockout and the stale-snapshot refusal. Collision 400s
// are STRUCTURED (access.go's accessCollisionBody: {error, cause, value}) as
// of the backend review round (commit 544467ed) — COLLISION_ERROR_CHART/
// _OPERATOR key directly off `cause`, no client-side reconstruction.
export const ACCESS_ERROR = {
  COLLISION_ERROR_CHART: (value: string) =>
    `"${value}" is already mapped in your chart's WARDYN_OIDC_ROLE_MAP — the chart always wins, so a console row here would only ever be shadowed. Edit your chart values instead.`,
  COLLISION_ERROR_OPERATOR: (value: string) =>
    `"${value}" is already on your chart's operator allowlist (WARDYN_OIDC_OPERATOR_EMAILS) and always resolves to admin — a console row here would have no effect. Edit your chart values instead.`,
  LOCKOUT_ERROR:
    "This change would leave you without admin access, checked against your last sign-in — refused. The admin token is never bound by this check, so it stays your recovery path if you lock out any other way.",
  // Post-adjudication canon addition (backend review round) — DISTINCT from
  // LOCKOUT_ERROR: fires when the server can't even re-derive the acting
  // admin's role from their (nil/truncated) session snapshot, so it can't
  // tell whether the write is a real demotion at all. LOCKOUT_ERROR now fires
  // only once the snapshot proves admin-before, non-admin-after. Access-
  // panel.tsx matches the server's exact raw message (no `.includes`) to pick
  // between the two.
  STALE_SNAPSHOT_ERROR: "your sign-in is too old to verify this change — sign in again before changing role mappings.",
  // Q7 adjudication — byte-identical to access.go's accessEmailKeyRefused
  // const; kept here too so the component never renders the server's raw
  // string directly (see access-panel.tsx's classifyWriteError).
  EMAIL_KEY_REFUSED:
    "Email mappings are disabled on this install. Map an App Role or group instead, or opt in with WARDYN_OIDC_ALLOW_EMAIL_MAPPINGS in your chart.",
} as const;

export const PREVIEW = {
  TITLE: "Preview a sign-in",
  LEAD_PREFIX: "Paste the roles, groups, or email a person's token would carry, or ",
  OWN_SESSION_CTA: "test with my own session",
  LEAD_SUFFIX: ", to see the role they'd derive at their next sign-in. Nothing here is saved.",
  FIELD_CLAIMS: "Roles, groups, or email",
  FIELD_CLAIMS_HINT: "One value per line — an App Role, a group name, or an email address.",
  RUN_CTA: "Preview",
  RESULT_MATCHED: (role: string, matched: string) => `Would sign in as ${role} — matched by ${matched}.`,
  RESULT_DEFAULT: (role: string) => `Would sign in as ${role} — nothing matched, so your default role applies.`,
  RESULT_DENIED: "Would be denied at sign-in — nothing matched, and no default role is set.",
  RESULT_LEGACY: (role: string) =>
    `Would sign in as ${role} — no mappings are configured; the operator allowlist decides.`,
  RESULT_UNKNOWN: "Couldn't check this against your mappings — try again.",
} as const;

export const ACCESS_STATE = {
  SSO_UNAVAILABLE_TITLE: "SSO isn't configured on this deployment",
  SSO_UNAVAILABLE_BODY:
    "Role mappings need SSO to mean anything — there's no per-person identity to map without it. This is likely a stale view; reload to see the single-user explainer instead.",
  FETCH_FAILED_TITLE: "Couldn't load role mappings",
  FETCH_FAILED_BODY:
    "Something went wrong reaching the server. Your chart's mappings still apply even though this list can't confirm them right now.",
  FETCH_FAILED_RETRY: "Retry",
} as const;

// §7.7 — SIGNIN, the reworded auth_error arms sign-in.tsx's authErrorMessage
// renders. Only these three keys: the other auth_error codes are unchanged
// and stay authored directly in sign-in.tsx (out of scope this round).
export const SIGNIN = {
  NO_ROLE:
    "Your account has no Wardyn role assigned. Ask your Wardyn admin to add you to a role mapping (WARDYN_OIDC_ROLE_MAP or WARDYN_OIDC_OPERATOR_EMAILS, or the equivalent on the People step).",
  EMAIL_VERIFIED_ABSENT:
    "Your identity provider doesn't send an email_verified claim at all (common on Entra ID), so this console can't confirm the email on its own. Ask your Wardyn admin to map your role by App Role or group instead (WARDYN_OIDC_ROLE_MAP, or the People step).",
  ROLE_CHECK_UNAVAILABLE: "Couldn't check your access — try again, or contact your admin.",
} as const;
