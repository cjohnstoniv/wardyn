/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Available to" copy canon. The visible lines come from the approved mocks
// (mock-08/user-types-packet-a.html, "Settings → Git providers → an Azure
// DevOps organisation", and docs/design/available-to-mock/canon.html for
// #923) and are copied from them exactly: availability-copy.test.ts parses
// canon.html back and compares. Every resource editor's AvailabilityControl
// (components/wardyn/availability-control.tsx) reads these instead of
// retyping them. A backticked literal in the canon is plain text here; the
// component renders it mono.
//
// Pure TS — no React, no fetch, no DOM.
export const AVAILABILITY = {
  LABEL: "Available to",
  EVERYONE: "Everyone",
  // An image's first choice in place of Everyone: an image nobody is listed
  // for can only be launched by admins (capImage widens).
  ADMINS_ONLY: "Admins only",
  ONLY: "Only these",
  // Audience chips (packet A): a type by its name, a group marked, a person by email.
  CHIP_TYPE: (name: string) => name,
  CHIP_GROUP: (name: string) => `${name} (group)`,
  CHIP_USER: (email: string) => email,
  REMOVE_ARIA: (chip: string) => `Remove ${chip}`,
  ADD_PLACEHOLDER: "Add a type, group or person…",
  ADD_CTA: "Add",
  LOAD_FAILED: "Available to — couldn't load.",
  HINT_USER: "An email address or the sign-in subject id.",
  HINT_GROUP: "A group or app-role name exactly as your identity provider sends it in the token.",
  HINT_USER_TYPE: "The user type's id, exactly as it appears on the User types screen.",
  // Each family's "Only these" line, set after its label as the mocks set it,
  // and its note under the list.
  PROVIDER_ONLY_HINT:
    "Only these — people not listed can't add or run repositories from this organisation. Runs already going aren't affected.",
  PROVIDER_NOTE:
    "Security admins can change this list from the type editor too. The organisation itself is edited only by super admins.",
  POLICY_ONLY_HINT:
    "Only these — people not listed can't choose this policy for a run. Runs already going aren't affected.",
  POLICY_NOTE:
    "Security admins can change this list from the type editor too. The policy itself is edited only by super admins.",
  MODEL_PROVIDER_ONLY_HINT:
    "Only these — people not listed can't run with this provider, even from a workspace pinned to it. Runs already going aren't affected.",
  MODEL_PROVIDER_NOTE:
    "Security admins can change this list from the type editor too. The provider itself is edited only by super admins.",
  IMAGE_HINT: "Images are off for everyone until you list someone here.",
  IMAGE_NOTE:
    "Security admins can change this list from the type editor too. The image list itself is edited only by super admins.",
  // Why the last audience's remove button is disabled while "Only these" is
  // on: removing it would leave the resource for nobody, the case the
  // server's empty "Only" refusal exists for (permissions_availability.go).
  LAST_AUDIENCE_LOCKED: "Choose Everyone before removing the last one, or nobody could use this.",
  LAST_AUDIENCE_LOCKED_IMAGE: "Choose Admins only before removing the last one.",
  // A creation form whose resource saved but whose list write was refused
  // (decision 4): the title, then the server's sentence, then what holds now.
  CREATE_PARTIAL_TITLE: "Saved, but not who it's available to",
  CREATE_PARTIAL_EVERYONE: "It is available to everyone until you set it again below.",
  CREATE_PARTIAL_IMAGE: "It stays admins-only until you set it again below.",
  // The org workspace detail page's card around the control.
  WORKSPACE_CARD_TITLE: "Availability",
  WORKSPACE_CARD_SUBTITLE: "Who may launch a run against this workspace.",
} as const;

// The Images tab on Workspace providers (decision 1).
export const IMAGES = {
  TAB: "Images",
  LEAD: "Images people may launch by name, with --image or as their own workspace's image. An image you set on an org workspace needs nothing here.",
  ADD_CTA: "Add image",
  EMPTY_TITLE: "No images listed",
  EMPTY_BODY: "Until you add one, only admins can launch an image by name.",
  REF: "Image",
  REF_HINT: "The exact reference people will type, e.g. ghcr.io/acme/dev-toolbox:1.4.",
} as const;
