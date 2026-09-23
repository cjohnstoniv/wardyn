/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { SIGNIN_HELP_LINK_LABEL } from "./people-access-copy";

// #484's frozen strings for the everyone-is-an-admin banner, byte-for-byte
// from docs/design/admin-access-canon.md (approved mock packet 3). Not in
// wardyn/copy.ts: that file is near its size cap, the same reason
// confinement-posture-copy.ts got its own module.
//
// The state is graded server-side by /setup/status's sso_rbac row
// (internal/api/setup_checks.go's ssoRBACCheck); its warn Detail says the same
// thing in one line — Q457-4, the same words on both surfaces.
export const ADMIN_ACCESS_BANNER = {
  TITLE: "Everyone who signs in is an admin",
  BODY: "Nobody is mapped to a role and no admin list is set, so every person your identity provider lets in can change policies, read and write secrets, decide approvals, and open a shell in any running sandbox.",
  ACTION: "Set who is an admin",
} as const;

// The People step, where role mappings live (setup/steps.ts's "people").
export const ADMIN_ACCESS_PEOPLE_STEP = "/setup?step=people";

// #484 — the People step's "When someone can't sign in" card
// (setup/sign-in-help-card.tsx), frozen byte-for-byte from the same canon doc.
// Here rather than in people-access-copy.ts, which the sign-in page pulls into
// the entry chunk (bundle-split.test.ts's budget); only the link label and the
// refusal set the sign-in page itself needs live there.
export const SIGNIN_HELP = {
  TITLE: "When someone can't sign in",
  LEAD: "Wardyn says what happened. You say what to do about it.",
  TEXT_LABEL: "What to tell them",
  TEXT_PLACEHOLDER: "Ask in #it-helpdesk to be added to Wardyn.",
  TEXT_HINT:
    "Up to 1,000 characters. Anyone who can reach the sign-in page can read it, signed in or not — so name your request process, not your internal systems.",
  COUNTER: (n: number) => `${n} / 1000`,
  URL_LABEL: "Link",
  URL_PLACEHOLDER: "https://",
  URL_HINT: 'Optional. Must start with http:// or https://. It shows as "Request access" — the address itself is public.',
  EMPTY_NOTE: "Nothing set. People see Wardyn's own sentence and are told to ask their Wardyn admin.",
  PREVIEW_HEADING: "What a signed-out person sees",
  APPLIES_NOTE:
    "Shown on the four refusals a person can't clear themselves: no role, an email domain that isn't allowed, too many groups to list, and a missing email claim.",
  LINK_LABEL: SIGNIN_HELP_LINK_LABEL,
  // Not in the mock — the card's save mechanics (canon doc, "implementation
  // strings").
  SAVE: "Save",
  SAVED_TOAST: "Sign-in help saved.",
  SAVE_ERROR: "Couldn't save the sign-in help.",
  LOAD_FAILED: "Couldn't load the sign-in help. Retry to edit it.",
  RETRY: "Retry",
  SAVED_ELSEWHERE:
    "Someone else saved this deployment's settings after you opened this card. Their version is loaded now and your text is unchanged — save again to apply it.",
} as const;

// Q457-8: characters, not bytes — the server counts runes (validateSignInHelp).
export const SIGNIN_HELP_TEXT_MAX = 1000;
