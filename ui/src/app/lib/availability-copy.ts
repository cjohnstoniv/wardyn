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
  // "Only these" chosen with nobody listed yet: held on screen, nothing saved, so the
  // server still has the resource open to everyone (or admins only, on an image).
  PENDING_NOTE: "Not saved yet. Until you add someone, this stays available to everyone.",
  PENDING_NOTE_AGENT: "Not saved yet. Until you add someone, this agent stays available to everyone.",
  PENDING_NOTE_IMAGE: "Not saved yet. Until you add someone, this image stays admins only.",
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

// A pre-wrapped image (design final §3): the approved mock
// (codex-handoff/010/mocks-approved/final-prewrap), copied exactly. The lines
// marked "not in the mock" are the reasons the mock drew no row for.
export const PREWRAPPED = {
  HOW_LABEL: "How it runs",
  KIND_WRAP: "Wardyn adds its tools",
  KIND_WRAP_HINT: "Any image. The image builder adds Wardyn's runner tools before each run.",
  KIND_WRAP_NO_BUILDER: "This control plane has no image builder.",
  KIND_WRAP_ADMINS_ONLY: "Admins only.",
  KIND: "Pre-wrapped: runs as is",
  KIND_HINT: "Built FROM Wardyn's agent-base and pinned by digest. Checked on the cluster when added; no image builder needed.",
  REF: "Image, pinned by digest",
  REF_HINT: "Built FROM agent-base 0.10.0 or later, e.g. ghcr.io/acme/sdk-agent:2.3@sha256:…",
  HOW_TO_BUILD: "How to build one",
  HOW_TO_BUILD_URL: "https://github.com/cjohnstoniv/wardyn/blob/main/deploy/images/README.md#pre-wrapped-image",
  ADD_CTA: "Check and add",
  NOT_PINNED: "Pin the image by digest (…@sha256:…). A tag can be moved to another image after it is checked.",
  BADGE: "Pre-wrapped · runs as is",
  READY: (arches: readonly string[], date: string) =>
    `Ready: checked on the cluster on ${arches.map((a) => `linux/${a}`).join(" and ")}, ${date}`,
  READY_NO_BUILD: (ok: string, missing: string) =>
    `Ready on linux/${ok} only: this image has no linux/${missing} build, so its runs go to ${ok} nodes and ${missing} runners can't run it.`,
  // not in the mock
  READY_NO_NODE: (ok: string, missing: string) =>
    `Ready on linux/${ok} only: the cluster has no linux/${missing} node to check on, so its runs go to ${ok} nodes and ${missing} runners can't run it.`,
  CHECKING: "Checking on the cluster. Not offered to anyone until it is ready.",
  MISSING_TOOL: (tool: string, builder: boolean) =>
    `Not ready: ${tool} is missing from /usr/local/bin${tool === "wardyn-rec" ? ", so a run can't be recorded" : ""}. Build the image FROM agent-base${builder ? ", or choose Wardyn adds its tools." : "."}`,
  UNPULLABLE: (why: string, registry: string) =>
    `Not ready: the cluster couldn't pull this image (${why}). Check that the deployment's image pull secret can read ${registry}.`,
  CUT_SHORT: "The check didn't finish (it is limited to 10 minutes, or the control plane restarted). Check again.",
  // not in the mock
  CONTRACT_OLD: "Not ready: this image's wardyn-rec is older than agent-base 0.10.0. Rebuild it FROM agent-base 0.10.0 or later.",
  NO_NODE: "Not ready: the cluster has no node of an architecture to check this image on.",
  NO_BUILD: "Not ready: this image has no linux/amd64 or linux/arm64 build.",
  FAILED: (what: string) => `Not ready: the check failed (${what}). Check again.`,
  CHECK_AGAIN: "Check again",
  NEEDS_BUILDER: "Needs an image builder; this control plane has none.",
  FOOTNOTE:
    "The check runs a short pod from the pinned image on each node architecture, with no network, and looks for Wardyn's runner tools. It proves the tools are there and start; it doesn't test your agent.",
} as const;
