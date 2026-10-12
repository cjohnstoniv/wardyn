/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Save-as dialog's strings behind ONE seam. Every string is a placeholder
// until the New Run copy modules land; the dialog takes this object as a prop,
// so swapping the source touches this file only. The tag on each line says
// where the words come from:
//   MOCK    the approved Make prototype's Save dialog
//   SPEC    the recommended sentence in DS-2 (Q2, Q6, Q7)
//   A17     paraphrases the approved disclosure decision; wording row W-25 is pending
//   MISSING no source states it; it needs an owner-approved sentence

import type { TemplateKind } from "../../../../lib/types/templates";
import type { SavePartId } from "./save-as-dialog.model";

export interface SaveAsCopy {
  title: (kind: TemplateKind) => string;
  intro: (kind: TemplateKind) => string;
  nameLabel: string;
  required: string;
  namePlaceholder: (kind: TemplateKind) => string;
  availableTo: string;
  onlyMe: string;
  /** A group the person manages that an administrator labelled. */
  groupLabelled: (label: string) => string;
  helperPlain: string;
  helperManager: string;
  include: string;
  everything: string;
  subset: string;
  omittedValues: string;
  partLabels: Record<SavePartId, string>;
  disclosure: string;
  ownDrivesLeftOut: string;
  nothingChanged: string;
  nothingPicked: string;
  cancel: string;
  save: (kind: TemplateKind) => string;
}

export const SAVE_AS_COPY: SaveAsCopy = {
  title: (kind) => `Save as ${kind}`, // MOCK
  intro: (kind) => `Saving changes the ${kind}, not this run draft. A run title is never stored.`, // MOCK
  nameLabel: "Name", // MOCK
  required: "Required", // MOCK
  namePlaceholder: (kind) => (kind === "template" ? "e.g. Frontend investigation" : "e.g. Package access"), // MOCK
  availableTo: "Available to", // MOCK
  onlyMe: "Only me", // MOCK
  groupLabelled: (label) => `${label} group`, // SPEC (Q3)
  helperPlain: "Only you can use it. Organisation admins and group template managers publish shared templates.", // SPEC (Q2)
  helperManager: "You can also publish to the groups you manage.", // SPEC (Q2)
  include: "Include", // MOCK
  everything: "Everything in this draft", // MOCK
  subset: "Choose a subset", // MOCK
  omittedValues: "Omitted values inherit defaults or are asked for when it's used. Explicit false, zero and empty values are kept.", // SPEC (Q7)
  partLabels: {
    run: "Run and timing", where: "Where it runs", work: "What it works on", tools: "Tools and model", access: "Access", limits: "Limits", // SPEC (Q6)
    network: "Network", credentials: "Credentials", access_rules: "Access rules", // SPEC (Q6)
  },
  disclosure:
    "A saved template keeps references such as secret names, workspace ids and drive ids, never values. People you share it with see those names. Their own access is checked when they use it, and anything they lack becomes Needs setup.", // A17
  ownDrivesLeftOut: "Your own drives are left out of shared templates.", // SPEC (DR5-L-privacy)
  nothingChanged: "You haven't changed anything for this run.", // SPEC (Q6)
  nothingPicked: "Choose at least one part to include.", // MISSING
  cancel: "Cancel", // MOCK
  save: (kind) => `Save ${kind}`, // MOCK
};
