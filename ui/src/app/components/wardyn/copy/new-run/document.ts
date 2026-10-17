/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run canon strings for the run document editor, byte for byte from docs/design/new-run-canon.md.
// Lazy-only: imported by the New Run flow and never re-exported through copy.ts, whose barrel is in the eager
// entry chunk. new-run-copy.test.ts keeps this file and the doc equal in both directions.

export const NR_DOCUMENT = {
  EYEBROW: "Advanced setup",
  TITLE: "Run document",
  LEDE: "Edit the shared draft directly. Unsupported input is reported, never discarded.",
  OPEN_IN_GUIDED: "Open in Guided setup",
  FORMAT_ARIA: "Document format",
  FORMAT_YAML: "YAML",
  FORMAT_JSON: "JSON",
  IMPORT_FILE: "Import file",
  LOAD_EXAMPLE: "Load example",
  EDITOR_ARIA: (format: string | number) => `${format} run document`,
  /** pending W-18: the recommended text until the owner answers. */
  READY:
    "Ready to validate against Wardyn's run document schema. Policy and availability are checked at review.",
  /** pending W-18: the recommended text until the owner answers. */
  VALID: "Valid document. Policy and availability are checked at review.",
  ISSUES: (n: string | number) => `${n} issues must be fixed`,
  /** pending W-53: the recommended text until the owner answers; the singular of ISSUES. */
  ISSUES_ONE: "1 issue must be fixed",
  VALIDATE: "Validate",
  CONTINUE_REVIEW: "Continue to review",
  ANNOUNCE_ERRORS: (n: string | number) => `${n} errors found.`,
  /** pending W-53: the recommended text until the owner answers; the singular of ANNOUNCE_ERRORS. */
  ANNOUNCE_ERRORS_ONE: "1 error found.",
  ANNOUNCE_VALID: "Document valid.",
  JSON_INVALID: "This JSON document isn't valid. Check the highlighted structure.",
  /** pending W-19: the recommended text until the owner answers. */
  YAML_INVALID: "This YAML document isn't valid. Check the highlighted structure.",
  /** Owner-approved 2026-10-17 (FR-0098): a file or document over MAX_DOCUMENT_BYTES (1 MiB) is refused unread. */
  TOO_LARGE: "This file is over 1 MiB, the most the editor reads.",
  /** Owner-approved 2026-10-17 (FR-0098): the browser's FileReader failed. */
  FILE_UNREADABLE: "The browser could not read this file.",
} as const;
