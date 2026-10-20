/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run canon strings for the Start screen, byte for byte from docs/design/new-run-canon.md.
// Lazy-only: imported by the New Run flow and never re-exported through copy.ts, whose barrel is in the eager
// entry chunk. new-run-copy.test.ts keeps this file and the doc equal in both directions.

export const NR_START = {
  EYEBROW: "Launch a governed workload",
  TITLE: "New run",
  LEDE: "Choose how you want to shape this run. Every path creates the same draft and meets again at review.",
  GUIDED_TITLE: "Guided setup",
  GUIDED_BADGE: "Fastest",
  GUIDED_BODY: "Answer a few plain questions. Your organisation's defaults take care of the rest.",
  GUIDED_FOOT: "Start guided setup",
  DOC_TITLE: "YAML / JSON",
  DOC_BADGE: "Advanced",
  DOC_BODY: "Edit or import a schema-aware run document and see what the server will allow.",
  DOC_YAML: "YAML",
  DOC_JSON: "JSON",
  TRUST_LINE: "Policy and availability are checked again when you launch.",
  /** The one muted line for what is not built, on this screen only (#3045). No version number: it would be wrong the day it ships. */
  ROADMAP: "On the roadmap: scheduled runs and multi-agent runs.",
} as const;
