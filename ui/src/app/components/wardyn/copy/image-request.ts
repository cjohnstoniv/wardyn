/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Every string of the image request that the elevation request does not already have: the dialog, the kind
// choice, the image rows under Your requests and the image row on Governance › Changes. Screens render these and
// author none. The strings the two kinds share (reason, ticket, buttons, states) are in ELEV.

export const IMG = {
  ACTION: "Request an image",
  TITLE: "New image request",
  LEAD: "Ask to launch one image, named by its digest. The people who decide for your default profile see your request. If one of them approves, you can launch exactly this image.",

  KIND_LABEL: "What do you need?",
  KIND_ELEVATION: "Elevation",
  KIND_IMAGE: "Image",

  IMAGE_LABEL: "Image",
  IMAGE_PLACEHOLDER: "registry.example.com/team/tool@sha256:…",
  IMAGE_HINT: "By digest only: <name>@sha256: and 64 hex characters. Copy it from your registry.",
  REASON_PLACEHOLDER: "Say what you need the image for. The people who decide read this.",
  SEND: "Request image",
  ALREADY_WAITING: "You already have a request waiting for this image. Withdraw it first, or wait for an answer.",

  LIST_LEAD_BOTH: "Time-limited access to a profile, and images by digest. A profile is a governance unit.",
  LIST_LEAD_IMAGES: "Images by digest.",
  LIST_EMPTY: "You have no requests.",
  LIST_FAILED_TITLE: "Couldn't load your requests",
  LIST_FAILED_BODY: "Wardyn isn't answering. Try again.",
  STATE_STALE: "Not applied",
  STATE_WITHDRAWN: "Withdrawn",
  LINE_WAITING: (when: string) => `Requested ${when} · waiting for a decision`,
  LINE_STALE: "Something changed before it was approved, so nothing was granted. Ask again if you still need it.",
  LINE_WITHDRAWN: (when: string) => `You withdrew it · ${when}`,

  CHANGE_LINE: (ticket?: string) => `One person, this digest only${ticket ? ` · ${ticket}` : ""}`,
  CHANGE_TARGET: (person: string, ref: string) => `${person} → ${ref}`,
  ROW_PERSON: "Person",
  ROW_IMAGE: "Image",
  ROW_DIGEST: "Digest",
  ROW_ASKED_UNDER: "Asked under",
  ROW_WRITES: "Approving writes",
  ASKED_UNDER: (unit: string) => `${unit}, their default profile`,
  WRITES:
    "An allow grant of exactly this image for this person. It holds in any profile they run in. The digest rule, a deny of the image and image admission still apply at every launch.",
  F_KIND: "Image",
  F_TITLE: (person: string) => `Grant ${person} one image, by digest`,
  F_TICKET: "Ticket",
  F_FOR_LABEL: "For:",
  F_FOR: (person: string, unit: string) => `${person}, asked under ${unit}, their default profile`,
  F_REASON: "Their reason:",
  F_WHAT_LABEL: "What you're approving:",
  F_WHAT: (person: string) => `${person} can launch exactly this image, in any profile they run in. The grant stays until an admin deletes it.`,
  F_BLAST_LABEL: "Blast radius:",
  F_BLAST: "The digest rule, a deny of the image and image admission still apply at every launch. Nothing else changes.",
} as const;
