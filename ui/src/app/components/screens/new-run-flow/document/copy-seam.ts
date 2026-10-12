/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// SEAM: the run document screen's console strings. The words are the approved
// mock's; the copy/new-run/document module (E1d) replaces DOCUMENT_COPY with its
// own export of the same shape, and nothing else in this folder changes.
// Strings the mock has no words for are marked PENDING: they are owner wording
// rows, reported to the lead, not settled copy.

export interface DocumentCopy {
  eyebrow: string;
  title: string;
  lede: string;
  back: string;
  importFile: string;
  loadExample: string;
  validate: string;
  openInGuided: string;
  continueToReview: string;
  /** The textarea's accessible name. */
  editorLabel: (format: "yaml" | "json") => string;
  /** The status line before any check. */
  ready: string;
  /** The status line after a clean check; also what the route announces. */
  valid: string;
  /** The headline over the problems; also what the route announces. */
  issues: (n: number) => string;
  /** The route's announcement after a check that found problems. */
  announceIssues: (n: number) => string;
  /** The local YAML parse failed (NEW-RUN-PLAN Q5, recommended text). PENDING. */
  yamlInvalid: string;
  /** A file or document over 1 MiB, refused before any request. PENDING. */
  tooLarge: string;
  /** The chosen file could not be read. PENDING. */
  fileUnreadable: string;
  /** Why Load example is held when the roster has no enabled harness. PENDING. */
  noHarness: string;
}

export const DOCUMENT_COPY: DocumentCopy = {
  eyebrow: "Advanced setup",
  title: "Run document",
  lede: "Edit the shared draft directly. Unsupported input is reported, never discarded.",
  back: "Back",
  importFile: "Import file",
  loadExample: "Load example",
  validate: "Validate",
  openInGuided: "Open in Guided setup",
  continueToReview: "Continue to review",
  editorLabel: (format) => `${format.toUpperCase()} run document`,
  ready: "Ready to validate against Wardyn's schema and your current policy.",
  valid: "Document valid.",
  issues: (n) => `${n} issues must be fixed`,
  announceIssues: (n) => `${n} errors found.`,
  yamlInvalid: "This YAML document isn't valid. Check the highlighted structure.",
  tooLarge: "PENDING: this file is larger than 1 MiB.",
  fileUnreadable: "PENDING: this file could not be read.",
  noHarness: "PENDING: no harness is enabled for your account.",
};
