/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Available to" copy canon — the frozen wording from docs/design/mock-08/
// user-types-design.md §2.6, transcribed verbatim. Every resource editor's
// AvailabilityControl (components/wardyn/availability-control.tsx) reads
// these instead of retyping the copy, so the control can't drift between the
// git provider row, the agents roster, an org workspace and a stored policy.
//
// Pure TS — no React, no fetch, no DOM.
export const AVAILABILITY = {
  LABEL: "Available to",
  EVERYONE: "Everyone",
  ONLY: "Only these",
  ADD_PLACEHOLDER: "Add a type, group or person…",
  ADD_CTA: "Add",
  HINT_USER: "An email address or the sign-in subject id.",
  HINT_GROUP: "A group or app-role name exactly as your identity provider sends it in the token.",
  HINT_USER_TYPE: "The user type's id, exactly as it appears on the User types screen.",
  // §2.6: "Images are off for everyone until you list someone here." — the
  // one widening kind's own hint, since a bare Everyone/Only radio reads
  // backwards for a family that starts admins-only.
  IMAGE_HINT: "Images are off for everyone until you list someone here.",
  // §2.6's "Never merely hidden in the console" — the standing fact under the
  // control, so an admin reads this as the real wall it is, not a filter.
  FOOTER: "People not listed can't add, run or launch against this — the server refuses it, wherever it's named.",
} as const;
