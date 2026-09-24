/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Available to" copy canon. The visible lines come from the approved mock
// (mock-08/user-types-packet-a.html, "Settings → Git providers → an Azure
// DevOps organisation") and are copied from it exactly. Every resource
// editor's AvailabilityControl (components/wardyn/availability-control.tsx)
// reads these instead of retyping them.
//
// Pure TS — no React, no fetch, no DOM.
export const AVAILABILITY = {
  LABEL: "Available to",
  EVERYONE: "Everyone",
  ONLY: "Only these",
  ADD_PLACEHOLDER: "Add a type, group or person…",
  ADD_CTA: "Add",
  LOAD_FAILED: "Available to — couldn't load.",
  HINT_USER: "An email address or the sign-in subject id.",
  HINT_GROUP: "A group or app-role name exactly as your identity provider sends it in the token.",
  HINT_USER_TYPE: "The user type's id, exactly as it appears on the User types screen.",
  // The git provider row's two lines from the mock: the "Only these" hint,
  // set after its label as the mock sets it, and the note under the list.
  PROVIDER_ONLY_HINT:
    "Only these — people not listed can't add or run repositories from this organisation. Runs already going aren't affected.",
  PROVIDER_NOTE:
    "Security admins can change this list from the type editor too. The organisation itself is edited only by super admins.",
  // Why the last audience's remove button is disabled while "Only these" is
  // on: removing it would leave the resource for nobody, the case the
  // server's empty "Only" refusal exists for (permissions_availability.go).
  LAST_AUDIENCE_LOCKED: "Choose Everyone before removing the last one, or nobody could use this.",
  // The org workspace detail page's card around the control.
  WORKSPACE_CARD_TITLE: "Availability",
  WORKSPACE_CARD_SUBTITLE: "Who may launch a run against this workspace.",
} as const;
